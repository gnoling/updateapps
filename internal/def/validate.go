package def

import (
	"github.com/andybalholm/cascadia"

	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"text/template"
)

// sourceFields lists each source type's required and optional yaml keys. "a|b"
// means exactly one of them.
var sourceFields = map[string]struct{ required, optional []string }{
	SourceGitHubRelease: {[]string{"repo"}, []string{"channel", "tag", "version_from", "skip_unmatched"}},
	SourceGitHubActions: {[]string{"repo", "workflow", "artifact|artifact_regex"}, []string{"branch", "event", "status"}},
	SourceForgejo:       {[]string{"host", "repo"}, nil},
	SourceGitLab:        {[]string{"project"}, []string{"host"}},
	SourceHTML:          {[]string{"url|command", "select"}, []string{"attr", "include_regex", "exclude_regex", "pick", "version", "download", "headers"}},
	SourceHTTPETag:      {[]string{"url"}, []string{"headers"}},
	SourceJSON:          {[]string{"url", "version", "download|url_jq"}, []string{"headers"}},
	SourceYAML:          {[]string{"url", "version", "download|url_jq"}, []string{"headers"}},
	SourceScript:        {[]string{"lua|script_file"}, nil},
	SourceFlatpak:       {[]string{"ref|app"}, []string{"remote", "branch"}},
}

var installFields = map[string][]string{
	InstallFile:       {"dest", "nested"},
	InstallExtract:    {"dest", "strip", "nested", "exclude", "executables"},
	InstallExtractOne: {"dest", "member", "member_ci", "nested"},
	InstallNone:       {"nested"},
	InstallDeb:        {"nested", "package"},
	InstallFlatpak:    {"nested", "scope", "package"},
}

func (a *App) validate() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	s := a.Source
	spec, ok := sourceFields[s.Type]
	switch {
	case s.Type == "":
		add("source.type is required")
	case !ok:
		add("source.type %q is not a known source type", s.Type)
	default:
		set := setFields(s)
		allowed := map[string]bool{"type": true}
		for _, req := range spec.required {
			alts := strings.Split(req, "|")
			n := 0
			for _, k := range alts {
				allowed[k] = true
				if set[k] {
					n++
				}
			}
			if n == 0 {
				add("source.%s is required for type %s", strings.Join(alts, " or source."), s.Type)
			} else if n > 1 {
				add("source: set only one of %s", strings.Join(alts, ", "))
			}
		}
		for _, k := range spec.optional {
			allowed[k] = true
		}
		for k := range set {
			if !allowed[k] {
				add("source.%s does not apply to type %s", k, s.Type)
			}
		}
	}

	if s.Type == SourceGitHubRelease {
		if !oneOf(s.Channel, "latest", "stable", "tag") {
			add("source.channel must be latest, stable or tag")
		}
		if (s.Channel == "tag") != (s.Tag != "") {
			add("source.tag and channel: tag go together")
		}
		if s.SkipUnmatched && s.Channel == "tag" {
			add("source.skip_unmatched has nothing to skip to with a fixed tag")
		}
		if !oneOf(s.VersionFrom, "release", "tag", "published_at", "asset.name", "asset.updated_at") {
			add("source.version_from %q is not valid", s.VersionFrom)
		}
	}
	if s.Type == SourceGitHubActions && !oneOf(s.Status, "success", "any") {
		add("source.status must be success or any")
	}
	if s.Type == SourceHTML && s.Version != "" && !oneOf(s.Version, "basename", "full") {
		if pattern, ok := strings.CutPrefix(s.Version, "regex:"); !ok {
			add("source.version must be basename, full or regex:<pattern>")
		} else if re, err := regexp.Compile(pattern); err != nil {
			add("source.version: %v", err)
		} else if re.NumSubexp() != 1 {
			add("source.version: the regex needs exactly one capture group")
		}
	}
	if s.Type == SourceHTML && s.Select != "" {
		// goquery quietly matches nothing for a selector it can't parse.
		if _, err := cascadia.Compile(s.Select); err != nil {
			add("source.select: %v", err)
		}
	}
	if s.Download != "" {
		if _, err := template.New("").Option("missingkey=error").Parse(s.Download); err != nil {
			add("source.download: %v", err)
		}
	}
	if s.ScriptFile != "" {
		if _, err := os.Stat(a.ScriptPath()); err != nil {
			add("source.script_file: %v", err)
		}
	}
	if s.Type == SourceFlatpak {
		if a.Install.Type != InstallFlatpak {
			add("source type flatpak needs install type flatpak")
		}
		if s.App != "" && s.Remote == "" {
			add("source.remote is required with source.app (or use source.ref with a .flatpakref URL)")
		}
	}
	if !oneOf(a.Install.Scope, "", "user", "system") {
		add("install.scope must be user or system")
	}
	if a.Install.Type == InstallNone && len(a.Post) == 0 {
		add("install.type none does nothing without post hooks")
	}
	if s.Pick != "" && !oneOf(s.Pick, "first", "last") {
		add("source.pick must be first or last")
	}
	for name, re := range map[string]string{
		"source.artifact_regex": s.ArtifactRegex, "source.include_regex": s.IncludeRegex, "source.exclude_regex": s.ExcludeRegex,
	} {
		checkRegex(add, name, re)
	}

	if as := a.Asset; as != nil {
		if !sourceHasAssets(s.Type) && ok {
			add("asset does not apply to source type %s", s.Type)
		}
		if as.Glob != "" && as.Regex != "" {
			add("asset: set glob or regex, not both")
		}
		if as.Glob == "" && as.Regex == "" && sourceHasAssets(s.Type) {
			add("asset: glob or regex is required for source type %s", s.Type)
		}
		checkGlob(add, "asset.glob", as.Glob)
		checkRegex(add, "asset.regex", as.Regex)
		checkRegex(add, "asset.exclude_regex", as.ExcludeRegex)
		if !oneOf(as.Pick, "", "first", "last") {
			add("asset.pick must be first or last")
		}
	}

	in := a.Install
	if fields, ok := installFields[in.Type]; !ok {
		add("install.type %q is not a known install type", in.Type)
	} else {
		allowed := map[string]bool{"type": true}
		for _, k := range fields {
			allowed[k] = true
		}
		for k := range setFields(in) {
			if !allowed[k] {
				add("install.%s does not apply to type %s", k, in.Type)
			}
		}
		if in.Type != InstallNone && !in.System() && in.Dest == "" {
			add("install.dest is required for type %s", in.Type)
		}
	}
	if in.Strip < 0 {
		add("install.strip must be >= 0")
	}
	for _, g := range in.Exclude {
		checkGlob(add, "install.exclude", g)
	}
	for _, g := range in.Executables {
		checkGlob(add, "install.executables", g)
	}
	checkGlob(add, "install.member", in.Member)
	return p
}

// setFields returns the yaml keys of v's non-zero fields.
func setFields(v any) map[string]bool {
	out := map[string]bool{}
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		if !rv.Field(i).IsZero() {
			out[rv.Type().Field(i).Tag.Get("yaml")] = true
		}
	}
	return out
}

func oneOf(s string, options ...string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

func checkRegex(add func(string, ...any), name, re string) {
	if re == "" {
		return
	}
	if _, err := regexp.Compile(re); err != nil {
		add("%s: %v", name, err)
	}
}

func checkGlob(add func(string, ...any), name, glob string) {
	if _, err := path.Match(glob, ""); err != nil {
		add("%s: bad glob %q", name, glob)
	}
}
