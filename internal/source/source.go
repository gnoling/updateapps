// Package source resolves an app definition to its latest version and
// download URL. One file per source type.
package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// Result is what every source resolves to.
type Result struct {
	Version  string // opaque; compared for equality with recorded state
	Display  string // shown to people; defaults to Version
	URL      string
	Filename string
	Size     int64  // 0 if unknown
	SHA256   string // hex digest to verify the download against, if upstream publishes one

	// Installed is what the system reports as installed (flatpak), so the
	// engine can spot apps updated outside updateapps.
	Installed string
	Headers   map[string]string // extra request headers for the download

	// Cache is handed back on the next Resolve. NotModified means Version came
	// from it and URL/Filename are empty.
	Cache       *Cache
	NotModified bool
}

// Cache is what the engine remembers between runs on a resolver's behalf.
type Cache struct {
	// ETag and the Version it resolved to. Empty if nothing is cached or the
	// run is forced.
	ETag    string
	Version string
	// Installed is the recorded version, so a resolver can reject an older
	// answer.
	Installed string
}

type Resolver interface {
	Resolve(ctx context.Context, c *fetch.Client, app *def.App, cache *Cache) (*Result, error)
}

// SoftError is a failure expected to clear by itself. It's reported but
// doesn't fail the run.
type SoftError struct{ Msg string }

func (e *SoftError) Error() string { return e.Msg }

func softf(format string, args ...any) error { return &SoftError{fmt.Sprintf(format, args...)} }

// Options are the settings resolvers need from the configuration.
type Options struct {
	Vars        def.Vars // what scripts see as config.appdir and friends
	FlatpakUser bool     // default flatpak scope is --user
}

// NewRegistry maps each source type to its resolver.
func NewRegistry(o Options) map[string]Resolver {
	return map[string]Resolver{
		def.SourceGitHubRelease: GitHubRelease{},
		def.SourceGitHubActions: GitHubActions{},
		def.SourceForgejo:       Forgejo{},
		def.SourceGitLab:        GitLab{},
		def.SourceHTML:          HTML{},
		def.SourceHTTPETag:      HTTPETag{},
		def.SourceJSON:          Document{},
		def.SourceYAML:          Document{YAML: true},
		def.SourceScript:        Script{Vars: o.Vars},
		def.SourceFlatpak:       Flatpak{UserScope: o.FlatpakUser},
	}
}

// baseURL turns a host into a URL prefix: https unless a scheme is given.
func baseURL(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + strings.TrimSuffix(host, "/")
}

// urlBasename is the last path segment of rawURL, without query or fragment.
func urlBasename(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		rawURL = u.Path
	}
	return path.Base(rawURL)
}

// sha256Digest extracts the hex from an API "sha256:<hex>" digest field.
func sha256Digest(digest string) string {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return ""
	}
	return hex
}

// named is anything selectable by asset rules.
type named interface{ assetName() string }

// pickAsset applies glob/regex, exclude_regex and pick to candidates in API order.
func pickAsset[T named](sel *def.Asset, candidates []T) (T, error) {
	var zero T
	var re, exclude *regexp.Regexp
	var err error
	if sel.Regex != "" {
		if re, err = regexp.Compile(sel.Regex); err != nil {
			return zero, err
		}
	}
	if sel.ExcludeRegex != "" {
		if exclude, err = regexp.Compile(sel.ExcludeRegex); err != nil {
			return zero, err
		}
	}
	var matches []T
	for _, c := range candidates {
		name := c.assetName()
		if re != nil && !re.MatchString(name) {
			continue
		}
		if sel.Glob != "" {
			if ok, _ := path.Match(sel.Glob, name); !ok {
				continue
			}
		}
		if exclude != nil && exclude.MatchString(name) {
			continue
		}
		matches = append(matches, c)
	}
	if len(matches) == 0 {
		want := sel.Glob
		if want == "" {
			want = sel.Regex
		}
		// Upstreams rename assets often; list what's there.
		msg := fmt.Sprintf("no asset matches %q; the release has:", want)
		if len(candidates) == 0 {
			msg = fmt.Sprintf("no asset matches %q; the release has no assets", want)
		}
		for i, c := range candidates {
			if i == 20 {
				msg += fmt.Sprintf("\n... and %d more", len(candidates)-i)
				break
			}
			msg += "\n" + c.assetName()
		}
		return zero, errors.New(msg)
	}
	if sel.Pick == "last" {
		return matches[len(matches)-1], nil
	}
	return matches[0], nil
}
