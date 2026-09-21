// Package repo loads the configured definition repositories: layering (later
// wins by id), this machine's enabled/disabled lists, and trust.
package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
)

// Schema is the definition format this build understands; repo.yaml may
// require one.
const Schema = 1

// Manifest is a repository's optional repo.yaml.
type Manifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Schema      int    `yaml:"schema"`
}

// Info describes one configured repository as loaded.
type Info struct {
	config.Repository
	Dir      string // where its files are
	DefsDir  string // the folder of definitions inside it
	Manifest Manifest
	Meta     Meta // fetch bookkeeping; zero for path repositories
	Apps     int  // definitions it contributed (before layering)
	Problem  string
}

// Set is the result of loading everything.
type Set struct {
	Apps     []*def.App // layered, sorted by id
	Repos    []Info
	Errors   []error  // bad definitions and unusable repositories
	Shadowed []string // "id: repo B overrides repo A"
}

// Local (apps.d/local/) always exists and is read last, so it wins. Like any
// repository it's untrusted unless configured.
const Local = "local"

// Repositories returns them in reading order, local last. With --defs it's
// that one trusted folder.
func Repositories(cfg *config.Config) []config.Repository {
	if cfg.DefsOverride != "" {
		return []config.Repository{{Name: "definitions", Path: cfg.DefsOverride, Trusted: true}}
	}
	repos := append([]config.Repository(nil), cfg.Repositories...)
	for _, r := range repos {
		if r.Name == Local {
			return repos
		}
	}
	return append(repos, config.Repository{Name: Local})
}

// Dir is where a repository's files live: apps.d/<name>/ unless it has a path.
func Dir(cfg *config.Config, r config.Repository) string {
	if r.Path != "" {
		return r.Path
	}
	return filepath.Join(cfg.Definitions, r.Name)
}

// Load reads every repository and resolves the final app set.
func Load(cfg *config.Config) *Set {
	set := &Set{}
	byID := map[string]*def.App{}
	vars := cfg.Vars()
	repos := Repositories(cfg)
	set.Errors = append(set.Errors, strays(cfg, repos)...)
	for _, r := range repos {
		info := Info{Repository: r, Dir: Dir(cfg, r)}
		info.Meta, _ = readMeta(info.Dir)
		apps, problem, errs := loadOne(&info, vars)
		info.Problem, info.Apps = problem, len(apps)
		if problem != "" {
			set.Errors = append(set.Errors, fmt.Errorf("repository %s: %s", r.Name, problem))
		}
		set.Errors = append(set.Errors, errs...)
		for _, a := range apps {
			a.Repo = r.Name
			if !r.Trusted {
				a.Blocked = needsTrust(a, vars, r.Name)
			}
			if r.Default == "disabled" {
				off := false
				a.Enabled = &off
			}
			if old := byID[a.ID]; old != nil {
				set.Shadowed = append(set.Shadowed, fmt.Sprintf("%s: %s overrides %s", a.ID, r.Name, old.Repo))
			}
			byID[a.ID] = a
		}
		set.Repos = append(set.Repos, info)
	}

	// This machine's choices come last and win.
	for id, a := range byID {
		if listed(cfg.Enabled, a.Repo, id) {
			on := true
			a.Enabled = &on
		}
		if listed(cfg.Disabled, a.Repo, id) {
			off := false
			a.Enabled = &off
		}
		set.Apps = append(set.Apps, a)
	}
	sort.Slice(set.Apps, func(i, j int) bool { return set.Apps[i].ID < set.Apps[j].ID })
	return set
}

func listed(list []string, repo, id string) bool {
	for _, entry := range list {
		if entry == id || entry == repo+"/"+id {
			return true
		}
	}
	return false
}

func loadOne(info *Info, vars def.Vars) (apps []*def.App, problem string, errs []error) {
	if _, err := os.Stat(info.Dir); err != nil {
		switch {
		case info.URL != "":
			return nil, "hasn't been fetched yet; run: updateapps repos pull", nil
		case info.Path != "":
			return nil, info.Dir + " doesn't exist", nil
		}
		return nil, "", nil // an apps.d/<name>/ nobody has put anything in yet
	}
	if data, err := os.ReadFile(filepath.Join(info.Dir, "repo.yaml")); err == nil {
		if err := yaml.Unmarshal(data, &info.Manifest); err != nil {
			return nil, "repo.yaml: " + err.Error(), nil
		}
		if info.Manifest.Schema > Schema {
			return nil, fmt.Sprintf("needs a newer updateapps (its definitions use schema %d, this build understands %d)", info.Manifest.Schema, Schema), nil
		}
	}
	// Definitions are in apps.d/ if the repository has one, else at the top.
	info.DefsDir = info.Dir
	if st, err := os.Stat(filepath.Join(info.Dir, "apps.d")); err == nil && st.IsDir() {
		info.DefsDir = filepath.Join(info.Dir, "apps.d")
	}
	apps, errs = def.LoadDir(info.DefsDir, vars, "repo.yaml")
	return apps, "", errs
}

// strays reports what in apps.d won't be read: loose definitions and unclaimed
// folders.
func strays(cfg *config.Config, repos []config.Repository) (problems []error) {
	if cfg.DefsOverride != "" {
		return nil
	}
	claimed := map[string]bool{}
	for _, r := range repos {
		if r.Path == "" {
			claimed[r.Name] = true
		}
	}
	entries, _ := os.ReadDir(cfg.Definitions)
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		switch {
		case strings.HasPrefix(name, "."):
		case e.IsDir() && !claimed[name]:
			problems = append(problems, fmt.Errorf("%s isn't read: no repository named %s is configured",
				filepath.Join(cfg.Definitions, name), name))
		case !e.IsDir() && (ext == ".yaml" || ext == ".yml"):
			problems = append(problems, fmt.Errorf("%s isn't read: it's outside a repository folder (move it to %s)",
				filepath.Join(cfg.Definitions, name), filepath.Join(cfg.Definitions, Local, name)))
		}
	}
	return problems
}

// needsTrust reports what a definition does that needs a trusted repository:
// shell, root, or writes outside the apps directories. Lua is sandboxed and
// always allowed.
func needsTrust(a *def.App, vars def.Vars, repo string) string {
	var why []string
	if len(a.Post) > 0 {
		why = append(why, "runs shell commands (post)")
	}
	if a.Source.Command != "" {
		why = append(why, "runs a shell command (source.command)")
	}
	if a.Install.Type == def.InstallDeb {
		why = append(why, "installs a system package as root")
	}
	if a.Install.Type == def.InstallFlatpak && a.Install.Scope == "system" {
		why = append(why, "installs a system-wide flatpak")
	}
	if dest := a.Install.Dest; dest != "" && !within(vars.AppDir, dest) && !within(vars.AppImageDir, dest) {
		why = append(why, "installs outside the apps directories ("+dest+")")
	}
	if len(why) == 0 {
		return ""
	}
	return strings.Join(why, ", ") + "; repository " + repo + " isn't trusted"
}

func within(root, p string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
