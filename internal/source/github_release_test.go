package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// fakeGitHub serves recorded fixtures the way api.github.com would.
func fakeGitHub(t *testing.T) (*fetch.Client, *[]string) {
	t.Helper()
	var paths []string
	serve := func(w http.ResponseWriter, r *http.Request, fixture, etag string) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		data, err := os.ReadFile("testdata/" + fixture)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("ETag", etag)
		w.Write(data)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/repos/o/app/releases":
			serve(w, r, "releases.json", `"list-1"`)
		case "/repos/o/app/releases/tags/continuous":
			serve(w, r, "release_continuous.json", `"tag-1"`)
		case "/repos/o/empty/releases":
			w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := fetch.New("")
	c.GitHubAPI = srv.URL
	c.Backoff = time.Millisecond
	return c, &paths
}

func app(src def.Source, asset def.Asset) *def.App {
	src.Type = def.SourceGitHubRelease
	if src.Channel == "" {
		src.Channel = "latest"
	}
	if asset.Pick == "" {
		asset.Pick = "first"
	}
	return &def.App{ID: "app", Source: src, Asset: &asset}
}

func TestLatestIncludesPrereleasesAndSkipsDrafts(t *testing.T) {
	c, paths := fakeGitHub(t)
	res, err := GitHubRelease{}.Resolve(context.Background(), c,
		app(def.Source{Repo: "o/app", VersionFrom: "release"}, def.Asset{Glob: "*x86_64.AppImage"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Display != "v2.0.0-beta1" || res.Version != "v2.0.0-beta1|2.0 beta|2026-09-01T10:00:00Z" {
		t.Errorf("version=%q display=%q", res.Version, res.Display)
	}
	if res.URL != "https://dl.example/beta/app-x86_64.AppImage" || res.Filename != "app-2.0.0-beta1-x86_64.AppImage" || res.Size != 200 {
		t.Errorf("asset = %+v", res)
	}
	if res.Cache == nil || res.Cache.ETag != `"list-1"` || res.Cache.Version != res.Version {
		t.Errorf("cache = %+v", res.Cache)
	}
	// The /releases/latest endpoint hides prereleases; it must not be used.
	if len(*paths) != 1 || strings.Contains((*paths)[0], "/latest") {
		t.Errorf("requests = %v", *paths)
	}
}

func TestStableSkipsPrereleases(t *testing.T) {
	c, _ := fakeGitHub(t)
	res, err := GitHubRelease{}.Resolve(context.Background(), c,
		app(def.Source{Repo: "o/app", Channel: "stable", VersionFrom: "tag"}, def.Asset{Glob: def.DefaultAssetGlob}), nil)
	if err != nil || res.Version != "v1.9.0" || res.Display != "v1.9.0" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestVersionFrom(t *testing.T) {
	c, _ := fakeGitHub(t)
	for vf, want := range map[string]string{
		"tag":              "continuous",
		"published_at":     "2026-09-16T05:13:21Z",
		"asset.name":       "Dolphin_Emulator-x86_64.AppImage",
		"asset.updated_at": "2026-09-16T05:20:00Z",
	} {
		res, err := GitHubRelease{}.Resolve(context.Background(), c,
			app(def.Source{Repo: "o/app", Channel: "tag", Tag: "continuous", VersionFrom: vf}, def.Asset{Glob: "Dolphin*"}), nil)
		if err != nil || res.Version != want {
			t.Errorf("%s: err=%v version=%q want %q", vf, err, res.Version, want)
		}
	}
}

func TestAssetSelection(t *testing.T) {
	c, _ := fakeGitHub(t)
	cases := map[string]struct {
		asset def.Asset
		want  string
	}{
		"glob is anchored and case-sensitive": {def.Asset{Glob: "*x86_64.AppImage"}, "app-2.0.0-beta1-x86_64.AppImage"},
		"regex":                               {def.Asset{Regex: `aarch64\.AppImage$`}, "app-2.0.0-beta1-aarch64.AppImage"},
		"exclude by elimination":              {def.Asset{Regex: ".", ExcludeRegex: `\.(exe|sig)$|aarch64`}, "app-2.0.0-beta1-x86_64.AppImage"},
		"pick last":                           {def.Asset{Glob: "*.AppImage", Pick: "last"}, "app-2.0.0-beta1-aarch64.AppImage"},
		"case-insensitive via regex":          {def.Asset{Regex: `(?i)X86_64\.appimage$`}, "app-2.0.0-beta1-x86_64.AppImage"},
	}
	for name, tc := range cases {
		res, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/app"}, tc.asset), nil)
		if err != nil || res.Filename != tc.want {
			t.Errorf("%s: err=%v got %q want %q", name, err, res.Filename, tc.want)
		}
	}
}

func TestNoMatchingAssetIsAnError(t *testing.T) {
	c, _ := fakeGitHub(t)
	_, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/app"}, def.Asset{Glob: "*.deb"}), nil)
	var soft *SoftError
	if err == nil || errors.As(err, &soft) || !strings.HasPrefix(err.Error(), "no asset matches \"*.deb\"; the release has:\napp-2.0.0-beta1-x86_64.AppImage\n") {
		t.Errorf("err = %v", err)
	}
}

func TestMissingRollingTagIsSoft(t *testing.T) {
	c, _ := fakeGitHub(t)
	_, err := GitHubRelease{}.Resolve(context.Background(), c,
		app(def.Source{Repo: "o/app", Channel: "tag", Tag: "nightly"}, def.Asset{Glob: "*"}), nil)
	var soft *SoftError
	if !errors.As(err, &soft) {
		t.Errorf("want SoftError, got %v", err)
	}
}

func TestNoReleasesAndMissingRepoAreHardErrors(t *testing.T) {
	c, _ := fakeGitHub(t)
	for _, repo := range []string{"o/empty", "o/gone"} {
		_, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: repo}, def.Asset{Glob: "*"}), nil)
		var soft *SoftError
		if err == nil || errors.As(err, &soft) {
			t.Errorf("%s: err = %v", repo, err)
		}
	}
}

func TestNotModifiedUsesCachedVersion(t *testing.T) {
	c, _ := fakeGitHub(t)
	res, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/app"}, def.Asset{Glob: "*"}),
		&Cache{ETag: `"list-1"`, Version: "cached-version"})
	if err != nil || !res.NotModified || res.Version != "cached-version" || res.URL != "" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	// A stale ETag falls through to a normal resolve.
	res, err = GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/app"}, def.Asset{Glob: "*x86_64.AppImage"}),
		&Cache{ETag: `"old"`, Version: "cached-version"})
	if err != nil || res.NotModified || res.URL == "" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

// Two real-world shapes of "which release is latest":
//   - rolling-release repos tag many releases on one commit, so they share a
//     created_at and GitHub's order among them is arbitrary: without a token
//     for GraphQL, publish time decides;
//   - a project re-published its whole history in reverse, so by publish time
//     1.0.0 is newest: created_at has to come first.
func TestLatestOrdering(t *testing.T) {
	const rolling = `[
		{"tag_name":"2026-09-11","created_at":"2026-06-28T22:51:40Z","published_at":"2026-09-11T12:13:05Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/old"}]},
		{"tag_name":"continuous","created_at":"2026-06-28T22:51:40Z","published_at":"2026-09-18T17:09:53Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/new"}]},
		{"tag_name":"2026-08-11","created_at":"2026-06-28T22:51:40Z","published_at":"2026-08-11T08:15:01Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/older"}]}]`
	const republished = `[
		{"tag_name":"5.0.1","created_at":"2026-08-25T17:19:30Z","published_at":"2026-09-18T22:31:49Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/5.0.1"}]},
		{"tag_name":"5.0.0","created_at":"2026-08-11T14:06:19Z","published_at":"2026-09-18T22:42:02Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/5.0.0"}]},
		{"tag_name":"1.0.0","created_at":"2024-05-27T00:35:38Z","published_at":"2026-09-18T22:52:22Z","assets":[{"name":"x.AppImage","browser_download_url":"https://dl/1.0.0"}]}]`
	for body, want := range map[string]string{rolling: "continuous", republished: "5.0.1"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		c := fetch.New("")
		c.GitHubAPI = srv.URL
		res, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/r"}, def.Asset{Glob: "*.AppImage"}), nil)
		if err != nil || res.Display != want {
			t.Errorf("want %s: err=%v res=%+v", want, err, res)
		}
		srv.Close()
	}
}

// RPCS3's and xenia-canary's shape: every release tagged on one commit, the
// REST listing ordered by tag hash, and the real newest not on its first
// page. GraphQL orders by true creation time, which is what gh (and so the
// bash script) went by.
func TestLatestUsesTrueCreationOrderWhenTied(t *testing.T) {
	var graphql, byTag int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const tmpl = `{"tag_name":%q,"created_at":"2018-12-31T15:28:29Z","published_at":%q,"assets":[{"name":"x.AppImage","browser_download_url":"https://dl/%s"}]}`
		switch {
		case r.URL.Path == "/graphql":
			graphql++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("graphql request: %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Write([]byte(`{"data":{"repository":{"releases":{"nodes":[{"tagName":"build-d08d568"},{"tagName":"build-accfecd"}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/releases/tags/build-d08d568"):
			byTag++
			fmt.Fprintf(w, tmpl, "build-d08d568", "2026-09-21T16:13:50Z", "newest")
		default: // the listing: by tag hash, newest nowhere in sight
			w.Header().Set("ETag", `"page-1"`)
			fmt.Fprintf(w, "["+tmpl+","+tmpl+"]", "build-fff0c96", "2021-09-24T18:37:11Z", "a", "build-ffeb16f", "2025-09-19T08:54:45Z", "b")
		}
	}))
	defer srv.Close()
	c := fetch.New("tok")
	c.GitHubAPI = srv.URL
	res, err := GitHubRelease{}.Resolve(context.Background(), c, app(def.Source{Repo: "o/r"}, def.Asset{Glob: "*.AppImage"}), nil)
	if err != nil || res.Display != "build-d08d568" || res.URL != "https://dl/newest" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if graphql != 1 || byTag != 1 {
		t.Errorf("graphql=%d byTag=%d, want one of each", graphql, byTag)
	}
	if res.Cache != nil {
		t.Error("page 1's ETag can't vouch for an order that came from GraphQL; don't cache it")
	}
}
