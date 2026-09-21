package def

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Vars are the values behind ${APPDIR}, ${APPIMAGEDIR} and ${HOME}. ${name} is
// the app's id.
type Vars struct {
	AppDir      string
	AppImageDir string
	Home        string
}

// LoadDir loads every *.yaml/*.yml in dir except the named files. Bad files
// are reported in errs; the rest still load.
func LoadDir(dir string, vars Vars, except ...string) (apps []*App, errs []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") || contains(except, e.Name()) {
			continue
		}
		app, err := LoadFile(filepath.Join(dir, e.Name()), vars)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	for i := 1; i < len(apps); i++ {
		if apps[i].ID == apps[i-1].ID {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q (also %s)", apps[i].Path, apps[i].ID, apps[i-1].Path))
		}
	}
	return apps, errs
}

// LoadFile parses, defaults, expands and validates one definition.
func LoadFile(path string, vars Vars) (*App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(path)
	app := &App{ID: strings.TrimSuffix(base, filepath.Ext(base)), Path: path}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(app); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	app.applyDefaults()

	var problems []string
	if err := app.expand(vars); err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, app.validate()...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	return app, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (a *App) applyDefaults() {
	if a.Name == "" {
		a.Name = a.ID
	}
	s := &a.Source
	if s.Type == SourceGitHubRelease {
		if s.Channel == "" {
			if s.Tag != "" {
				s.Channel = "tag"
			} else {
				s.Channel = "latest"
			}
		}
		if s.VersionFrom == "" {
			s.VersionFrom = "release"
		}
	}
	if s.Type == SourceGitHubActions && s.Status == "" {
		s.Status = "success"
	}
	if sourceHasAssets(s.Type) {
		if a.Asset == nil {
			a.Asset = &Asset{}
		}
		if a.Asset.Glob == "" && a.Asset.Regex == "" {
			switch s.Type {
			case SourceGitHubRelease:
				a.Asset.Glob = DefaultAssetGlob
			case SourceForgejo:
				a.Asset.Regex = DefaultForgejoAssetRegex
			}
		}
		if a.Asset.Pick == "" {
			a.Asset.Pick = "first"
		}
	}
	if a.Install.Type == "" {
		a.Install.Type = InstallFile
		if s.Type == SourceFlatpak {
			a.Install.Type = InstallFlatpak
		}
	}
	if a.Install.Dest == "" && (a.Install.Type == InstallFile || a.Install.Type == InstallExtractOne) {
		a.Install.Dest = "${APPIMAGEDIR}/${name}"
	}
}

func sourceHasAssets(t string) bool {
	return t == SourceGitHubRelease || t == SourceForgejo || t == SourceGitLab
}

func (a *App) expand(vars Vars) error {
	var unknown []string
	lookup := func(strict bool) func(string) string {
		return func(k string) string {
			switch k {
			case "APPDIR":
				return vars.AppDir
			case "APPIMAGEDIR":
				return vars.AppImageDir
			case "HOME":
				return vars.Home
			case "name":
				return a.ID
			}
			if strict {
				unknown = append(unknown, k)
			}
			return "${" + k + "}"
		}
	}
	// Resolve a trailing slash (meaning directory) here; filepath.Clean would
	// drop it.
	dest := os.Expand(a.Install.Dest, lookup(true))
	if strings.HasPrefix(dest, "~/") {
		dest = filepath.Join(vars.Home, dest[2:])
	}
	if a.Install.Type == InstallFile || a.Install.Type == InstallExtractOne {
		if strings.HasSuffix(dest, "/") {
			dest = filepath.Join(dest, a.ID)
		}
	}
	a.Install.Dest = dest
	a.Notice = os.Expand(a.Notice, lookup(false))
	if len(unknown) > 0 {
		return fmt.Errorf("install.dest: unknown variable(s) ${%s}", strings.Join(unknown, "}, ${"))
	}
	return nil
}

// ErrNotFound is returned by Find when no definition has the given id.
var ErrNotFound = errors.New("no such app")

// Find returns the app whose id or name equals s, case-insensitively.
func Find(apps []*App, s string) (*App, error) {
	for _, a := range apps {
		if strings.EqualFold(a.ID, s) || strings.EqualFold(a.Name, s) {
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, s)
}

// Select applies filters: case-insensitive substrings of id, name or
// repo/URL/host/project, OR'd. No filters selects every runnable app. A
// disabled or blocked app must be named exactly.
func Select(apps []*App, filters []string) []*App {
	var out []*App
	for _, a := range apps {
		runnable := a.IsEnabled() && a.Blocked == ""
		if len(filters) == 0 {
			if runnable {
				out = append(out, a)
			}
			continue
		}
		hay := strings.ToLower(strings.Join([]string{
			a.ID, a.Name, a.Source.Repo, a.Source.URL, a.Source.Host, a.Source.Project,
		}, "\x00"))
		for _, f := range filters {
			f = strings.ToLower(f)
			exact := f == strings.ToLower(a.ID) || f == strings.ToLower(a.Name)
			if exact || (runnable && strings.Contains(hay, f)) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}
