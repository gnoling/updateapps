// Package config loads settings shared by the CLI and GUI front ends.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gnoling/updateapps/internal/def"
)

const DefaultJobs = 12

// DefaultRepository is used when the config has no repositories: key at all.
// It's opt-in, so a fresh install doesn't start installing a hundred apps;
// `repositories: []` means none.
var DefaultRepository = Repository{
	Name:    "main",
	URL:     "https://github.com/gnoling/updateapps-definitions",
	Default: "disabled",
}

// Repository is one source of definitions, stored in <definitions>/<name>/
// unless Path is set.
type Repository struct {
	Name string `yaml:"name"`
	// URL is fetched by `repos pull`: a GitHub, Forgejo/Gitea or GitLab
	// repository, or a direct .tar.gz/.zip link. Without one the folder is the
	// user's.
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"` // default: the host's default branch (main elsewhere)
	// Path relocates a user-managed repository (a git checkout, say). Never
	// written to.
	Path string `yaml:"path"`
	// Default "disabled" makes the repository opt-in through the enabled:
	// list.
	Default string `yaml:"default"`
	// Trusted allows shell commands, root installs and writes outside the apps
	// directories. Never implied.
	Trusted bool `yaml:"trusted"`
}

type Config struct {
	AppDir      string `yaml:"appdir"`
	AppImageDir string `yaml:"appimagedir"`
	// Repositories are read in order; a later one wins when ids collide.
	Repositories []Repository `yaml:"repositories"`
	// Enabled and Disabled (id or repo/id) override the repository default and
	// the definition's own enabled: field.
	Enabled  []string `yaml:"enabled"`
	Disabled []string `yaml:"disabled"`
	// AutoPull refreshes url: repositories at the start of every update.
	AutoPull bool `yaml:"auto_pull"`
	// Definitions holds one subfolder per repository. Default: apps.d beside
	// the config.
	Definitions string `yaml:"definitions"`
	// DefsOverride (--defs) replaces the repositories with one flat, trusted
	// folder.
	DefsOverride string `yaml:"-"`
	State        string `yaml:"state"`
	Jobs         int    `yaml:"jobs"`
	GitHubToken  string `yaml:"github_token"`
	// Privilege is how system packages (deb) get root: auto, sudo, pkexec, none.
	Privilege string `yaml:"privilege"`
	// FlatpakScope is the default for flatpak installs: user or system.
	FlatpakScope string `yaml:"flatpak_scope"`

	// Set from the environment (bash compatibility) or flags, never the file.
	Force   bool   `yaml:"-"`
	Verbose int    `yaml:"-"`
	Path    string `yaml:"-"`
}

// DefaultPath is where the config lives when --config isn't given.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "updateapps", "config.yaml")
}

// Load reads path (a missing file means defaults), then applies environment
// overrides. Flags are the caller's job.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := &Config{Path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	home, _ := os.UserHomeDir()
	if c.AppDir == "" {
		c.AppDir = filepath.Join(home, "apps")
	}
	if c.Definitions == "" {
		c.Definitions = filepath.Join(filepath.Dir(path), "apps.d")
	}
	if c.Repositories == nil {
		c.Repositories = []Repository{DefaultRepository}
	}
	if c.State == "" {
		c.State = filepath.Join(stateDir(home), "updateapps", "state.json")
	}
	if c.Jobs == 0 {
		c.Jobs = DefaultJobs
	}
	if c.Privilege == "" {
		c.Privilege = "auto"
	}
	if c.FlatpakScope == "" {
		c.FlatpakScope = "user"
	}

	if v := os.Getenv("APPDIR"); v != "" {
		c.AppDir = v
	}
	if v := os.Getenv("APPIMAGEDIR"); v != "" {
		c.AppImageDir = v
	}
	if v := os.Getenv("MAXJOBS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MAXJOBS=%q is not a number", v)
		}
		c.Jobs = n
	}
	if v := os.Getenv("FORCE"); v != "" && v != "0" {
		c.Force = true
	}
	if v := os.Getenv("VERBOSE"); v != "" {
		c.Verbose, _ = strconv.Atoi(v)
	}

	// Default after the env pass so APPDIR=/x alone moves appimages with it.
	if c.AppImageDir == "" {
		c.AppImageDir = filepath.Join(c.AppDir, "appimages")
	}
	for _, p := range []*string{&c.AppDir, &c.AppImageDir, &c.Definitions, &c.State} {
		*p = expandHome(*p, home)
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		r.Path = expandHome(r.Path, home)
		switch {
		case r.Name == "" || strings.ContainsAny(r.Name, `/\ `) || strings.HasPrefix(r.Name, "."):
			return nil, fmt.Errorf("repositories[%d]: name %q must be a simple word (it names a folder and prefixes ids)", i, r.Name)
		case seen[r.Name]:
			return nil, fmt.Errorf("repositories: two are named %q", r.Name)
		case r.URL != "" && r.Path != "":
			return nil, fmt.Errorf("repository %s: set url or path, not both", r.Name)
		case r.Default != "" && r.Default != "enabled" && r.Default != "disabled":
			return nil, fmt.Errorf("repository %s: default must be enabled or disabled", r.Name)
		}
		seen[r.Name] = true
	}
	switch c.Privilege {
	case "auto", "sudo", "pkexec", "none":
	default:
		return nil, fmt.Errorf("privilege must be auto, sudo, pkexec or none (got %q)", c.Privilege)
	}
	if c.FlatpakScope != "user" && c.FlatpakScope != "system" {
		return nil, fmt.Errorf("flatpak_scope must be user or system (got %q)", c.FlatpakScope)
	}
	if c.Jobs < 1 {
		return nil, fmt.Errorf("jobs must be at least 1 (got %d)", c.Jobs)
	}
	return c, nil
}

// PendingDir holds downloads whose install needed root that wasn't available.
func PendingDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "updateapps", "pending")
}

// Vars are the values definitions can reference.
func (c *Config) Vars() def.Vars {
	home, _ := os.UserHomeDir()
	return def.Vars{AppDir: c.AppDir, AppImageDir: c.AppImageDir, Home: home}
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// stateDir stands in for the os.UserStateDir Go doesn't have.
func stateDir(home string) string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows", "darwin", "ios":
		if dir, err := os.UserConfigDir(); err == nil {
			return dir
		}
	}
	return filepath.Join(home, ".local", "state")
}
