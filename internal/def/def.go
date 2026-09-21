// Package def holds the YAML app-definition types, loading and validation.
package def

import (
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Source types.
const (
	SourceGitHubRelease = "github-release"
	SourceGitHubActions = "github-actions"
	SourceForgejo       = "forgejo"
	SourceGitLab        = "gitlab"
	SourceHTML          = "html"
	SourceHTTPETag      = "http-etag"
	SourceJSON          = "json"
	SourceYAML          = "yaml"
	SourceScript        = "script"
	SourceFlatpak       = "flatpak"
)

// Install types.
const (
	InstallFile       = "file"
	InstallExtract    = "extract"
	InstallExtractOne = "extract-one"
	InstallNone       = "none"
	InstallDeb        = "deb"     // system package via apt; batched, needs root
	InstallFlatpak    = "flatpak" // bundle, flatpakref, or an app from a configured remote
)

// System reports whether the install goes through a package manager: no dest,
// installed in the end-of-run batch.
func (in Install) System() bool { return in.Type == InstallDeb || in.Type == InstallFlatpak }

// Default asset patterns match bash gh_get and fj_get.
const (
	DefaultAssetGlob         = "*AppImage"
	DefaultForgejoAssetRegex = `\.AppImage$`
)

// App is one definition file. ID and Path come from the file, not its contents.
type App struct {
	ID   string `yaml:"-"`
	Path string `yaml:"-"`
	// Repo is the repository it came from. Blocked, if set, is why it may not
	// run (untrusted repository).
	Repo    string `yaml:"-"`
	Blocked string `yaml:"-"`

	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Category    string   `yaml:"category"`
	Enabled     *bool    `yaml:"enabled"`
	Notes       string   `yaml:"notes"`
	Insecure    bool     `yaml:"insecure"`
	Source      Source   `yaml:"source"`
	Asset       *Asset   `yaml:"asset"`
	Install     Install  `yaml:"install"`
	Post        []string `yaml:"post"`
	Notice      string   `yaml:"notice"`
}

// ScriptPath resolves source.script_file, which is relative to the definition.
func (a *App) ScriptPath() string {
	if filepath.IsAbs(a.Source.ScriptFile) {
		return a.Source.ScriptFile
	}
	return filepath.Join(filepath.Dir(a.Path), a.Source.ScriptFile)
}

// IsEnabled reports whether the app runs without being named explicitly.
func (a *App) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Source is the union of every source type's fields; validation rejects those
// that don't apply.
type Source struct {
	Type string `yaml:"type"`

	// github-release, github-actions, forgejo
	Repo        string `yaml:"repo"`
	Channel     string `yaml:"channel"`
	Tag         string `yaml:"tag"`
	VersionFrom string `yaml:"version_from"`
	// SkipUnmatched takes the newest release that has a matching asset instead
	// of failing on the latest.
	SkipUnmatched bool `yaml:"skip_unmatched"`

	// github-actions
	Workflow      string `yaml:"workflow"`
	Branch        string `yaml:"branch"`
	Event         string `yaml:"event"`
	Status        string `yaml:"status"`
	Artifact      string `yaml:"artifact"`
	ArtifactRegex string `yaml:"artifact_regex"`

	// forgejo
	Host string `yaml:"host"`

	// gitlab
	Project string `yaml:"project"`

	// html, http-etag, json, yaml
	URL          string `yaml:"url"`
	Command      string `yaml:"command"`
	Select       string `yaml:"select"`
	Attr         string `yaml:"attr"`
	IncludeRegex string `yaml:"include_regex"`
	ExcludeRegex string `yaml:"exclude_regex"`
	Pick         string `yaml:"pick"`
	Version      string `yaml:"version"`
	Download     string `yaml:"download"`
	URLJQ        string `yaml:"url_jq"`

	// Extra request headers (html, http-etag, json, yaml).
	Headers map[string]string `yaml:"headers"`

	// flatpak: a .flatpakref URL, or an app on a remote that's already configured
	Ref    string `yaml:"ref"`
	Remote string `yaml:"remote"`
	App    string `yaml:"app"`

	// script
	Lua        string `yaml:"lua"`
	ScriptFile string `yaml:"script_file"`
}

// Asset selects a release asset: a glob string or a mapping.
type Asset struct {
	Glob         string `yaml:"glob"`
	Regex        string `yaml:"regex"`
	ExcludeRegex string `yaml:"exclude_regex"`
	Pick         string `yaml:"pick"`
}

func (a *Asset) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		a.Glob = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: asset must be a glob string or a mapping", n.Line)
	}
	// Decode via an alias to avoid recursion; check keys by hand because
	// Node.Decode ignores KnownFields.
	for i := 0; i < len(n.Content); i += 2 {
		switch k := n.Content[i]; k.Value {
		case "glob", "regex", "exclude_regex", "pick":
		default:
			return fmt.Errorf("line %d: field %s not found in asset", k.Line, k.Value)
		}
	}
	type plain Asset
	return n.Decode((*plain)(a))
}

type Install struct {
	Type        string   `yaml:"type"`
	Dest        string   `yaml:"dest"`
	Strip       int      `yaml:"strip"`
	Nested      bool     `yaml:"nested"`
	Exclude     []string `yaml:"exclude"`
	Executables []string `yaml:"executables"`
	Member      string   `yaml:"member"`
	MemberCI    bool     `yaml:"member_ci"`

	// The deb package name or flatpak application id. Optional; lets
	// mark-current ask the package manager without downloading.
	Package string `yaml:"package"`
	// flatpak: user or system; empty means the configured default.
	Scope string `yaml:"scope"`
}
