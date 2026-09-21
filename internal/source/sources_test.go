package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/systest"
)

// fixtureServer maps request paths (with query, when given) to fixture files.
func fixtureServer(t *testing.T, routes map[string]string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var reqs []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r)
		fixture, ok := routes[r.URL.RequestURI()]
		if !ok {
			fixture, ok = routes[r.URL.EscapedPath()]
		}
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		data, err := os.ReadFile("testdata/" + fixture)
		if err != nil {
			t.Error(err)
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func client(api, token string) *fetch.Client {
	c := fetch.New(token)
	c.GitHubAPI = api
	c.Backoff = time.Millisecond
	return c
}

func actionsServer(t *testing.T) (*httptest.Server, *[]*http.Request) {
	StaleRetryDelay = time.Millisecond
	return fixtureServer(t, map[string]string{
		"/repos/o/mesen/actions/workflows":                "actions_workflows.json",
		"/repos/o/mesen/actions/workflows/101/runs":       "actions_runs_101.json",
		"/repos/o/mesen/actions/workflows/103/runs":       "actions_runs_103.json",
		"/repos/o/mesen/actions/workflows/build.yml/runs": "actions_runs_101.json",
		"/repos/o/mesen/actions/runs/9001/artifacts":      "actions_artifacts_9001.json",
	})
}

func TestActionsByWorkflowName(t *testing.T) {
	srv, reqs := actionsServer(t)
	app := &def.App{Source: def.Source{Type: def.SourceGitHubActions, Repo: "o/mesen", Workflow: "Build Mesen",
		Status: "success", Branch: "main", Event: "push", Artifact: "Mesen (Linux x64 - AppImage)"}}
	res, err := GitHubActions{}.Resolve(context.Background(), client(srv.URL, "tok"), app, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two workflows share the name; the newer run (9001, not 8000) wins.
	if res.Version != "9001|abc123def" || res.Display != "9001" {
		t.Errorf("version=%q display=%q", res.Version, res.Display)
	}
	if res.URL != "https://api.example/artifacts/2/zip" || res.Filename != "Mesen (Linux x64 - AppImage).zip" || res.Size != 4242 || res.SHA256 != "AABBCC" {
		t.Errorf("artifact = %+v", res)
	}
	var runsQuery string
	for _, r := range *reqs {
		if strings.HasSuffix(r.URL.Path, "/101/runs") {
			runsQuery = r.URL.RawQuery
		}
	}
	// Filter parameters put the request on GitHub's eventually-consistent
	// search path; branch, event and success are judged client-side instead.
	for _, param := range []string{"status=", "branch=", "event="} {
		if strings.Contains(runsQuery, param) {
			t.Errorf("runs query %q uses the unreliable server-side %s filter", runsQuery, param)
		}
	}
}

// The newest successful run (9500) built a pull request. Installing that
// would run unreviewed code, so it's passed over unless asked for by name.
func TestActionsSkipsPullRequestRuns(t *testing.T) {
	srv, _ := actionsServer(t)
	src := def.Source{Type: def.SourceGitHubActions, Repo: "o/mesen", Workflow: "build.yml", Branch: "main", Status: "success", Artifact: "Mesen (Linux x64 - AppImage)"}
	res, err := GitHubActions{}.Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, nil)
	if err != nil || res.Display != "9001" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

// GitHub has answered the same query with a month-old run and then the right
// one. An answer older than what's installed is ignored, not installed.
func TestActionsNeverMovesBackwards(t *testing.T) {
	srv, _ := actionsServer(t)
	src := def.Source{Type: def.SourceGitHubActions, Repo: "o/mesen", Workflow: "build.yml", Branch: "main", Status: "success", Artifact: "Mesen (Linux x64 - AppImage)"}
	var soft *SoftError
	_, err := GitHubActions{}.Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, &Cache{Installed: "9999|newer"})
	if !errors.As(err, &soft) || !strings.Contains(soft.Msg, "stale") {
		t.Fatalf("want a soft stale-answer skip, got %v", err)
	}
	// The installed run is still the latest: up to date, without even asking
	// for its artifacts (which expire after 90 days).
	srv2, reqs := actionsServer(t)
	res, err := GitHubActions{}.Resolve(context.Background(), client(srv2.URL, "tok"), &def.App{Source: src}, &Cache{Installed: "9001|abc123def"})
	if err != nil || !res.NotModified || res.Version != "9001|abc123def" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	for _, r := range *reqs {
		if strings.Contains(r.URL.Path, "/artifacts") {
			t.Error("artifacts were listed for a run that's already installed")
		}
	}
}

// A failed run newer than the last good one must not be picked, and a branch
// whose recent history is all failures falls back to the filtered query.
func TestActionsJudgesSuccessItself(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/mixed.yml/runs"):
			w.Write([]byte(`{"workflow_runs":[
				{"id":30,"conclusion":"failure","status":"completed","event":"push"},
				{"id":20,"conclusion":"success","status":"completed","event":"push","head_sha":"good"},
				{"id":10,"conclusion":"success","status":"completed","event":"push"}]}`))
		case strings.HasSuffix(r.URL.Path, "/broken.yml/runs"):
			queries = append(queries, r.URL.RawQuery)
			if r.URL.Query().Get("status") == "success" && r.URL.Query().Get("branch") == "port" {
				w.Write([]byte(`{"workflow_runs":[{"id":5,"conclusion":"success","status":"completed","event":"push","head_branch":"port","head_sha":"old"}]}`))
			} else {
				w.Write([]byte(`{"workflow_runs":[{"id":40,"conclusion":"failure","status":"completed","event":"push"}]}`))
			}
		case strings.Contains(r.URL.Path, "/artifacts"):
			w.Write([]byte(`{"artifacts":[{"name":"app","expired":false,"archive_download_url":"https://x/app"}]}`))
		}
	}))
	defer srv.Close()
	src := def.Source{Type: def.SourceGitHubActions, Repo: "o/r", Workflow: "mixed.yml", Status: "success", Artifact: "app"}
	res, err := GitHubActions{}.Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, nil)
	if err != nil || res.Version != "20|good" {
		t.Fatalf("mixed: err=%v res=%+v", err, res)
	}
	src.Status = "any" // suwayomi: take the newest run whatever its outcome
	if res, err = (GitHubActions{}).Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, nil); err != nil || res.Display != "30" {
		t.Fatalf("any: err=%v res=%+v", err, res)
	}
	src.Workflow, src.Status, src.Branch = "broken.yml", "success", "port"
	if res, err = (GitHubActions{}).Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, nil); err != nil || res.Version != "5|old" || len(queries) != 2 {
		t.Fatalf("fallback: err=%v res=%+v queries=%v", err, res, queries)
	}
}

func TestArtifactFilename(t *testing.T) {
	for name, want := range map[string]string{
		"melonDS-x86_64.AppImage":      "melonDS-x86_64.AppImage", // un-zipped upload
		"jar":                          "jar.zip",
		"cemu-appimage-x64":            "cemu-appimage-x64.zip",
		"Mesen (Linux x64 - AppImage)": "Mesen (Linux x64 - AppImage).zip",
		"build-v1.2 (x64)":             "build-v1.2 (x64).zip",
	} {
		if got := artifactFilename(name); got != want {
			t.Errorf("artifactFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

// The observed failure: the first answer names an old run whose artifact has
// expired, the second names the real latest. One re-ask fixes it.
func TestActionsAsksAgainWhenAnswerLooksStale(t *testing.T) {
	StaleRetryDelay = time.Millisecond
	listings := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/build.yml/runs"):
			if listings++; listings == 1 {
				w.Write([]byte(`{"workflow_runs":[{"id":7000,"head_sha":"old","status":"completed","conclusion":"success","event":"push"}]}`))
			} else {
				w.Write([]byte(`{"workflow_runs":[{"id":9001,"head_sha":"new","status":"completed","conclusion":"success","event":"push"}]}`))
			}
		case strings.HasSuffix(r.URL.Path, "/runs/7000/artifacts"):
			w.Write([]byte(`{"artifacts":[{"name":"app","expired":true,"archive_download_url":"https://x/old"}]}`))
		case strings.HasSuffix(r.URL.Path, "/runs/9001/artifacts"):
			w.Write([]byte(`{"artifacts":[{"name":"app","expired":false,"archive_download_url":"https://x/new"}]}`))
		}
	}))
	defer srv.Close()
	src := def.Source{Type: def.SourceGitHubActions, Repo: "o/r", Workflow: "build.yml", Status: "success", Artifact: "app"}
	res, err := GitHubActions{}.Resolve(context.Background(), client(srv.URL, "tok"), &def.App{Source: src}, nil)
	if err != nil || res.Display != "9001" || listings != 2 {
		t.Fatalf("err=%v res=%+v listings=%d", err, res, listings)
	}
}

func TestActionsVariants(t *testing.T) {
	srv, reqs := actionsServer(t)
	c := client(srv.URL, "tok")
	base := def.Source{Type: def.SourceGitHubActions, Repo: "o/mesen", Workflow: "build.yml", Branch: "main", Status: "success"}

	// A file name skips the workflow lookup; artifact_regex finds run-stamped names.
	src := base
	src.ArtifactRegex = "^86Box-Qt6-NDR-Dev-UbuntuJammy-x86_64-"
	res, err := GitHubActions{}.Resolve(context.Background(), c, &def.App{Source: src}, nil)
	if err != nil || res.Filename != "86Box-Qt6-NDR-Dev-UbuntuJammy-x86_64-b9001.zip" {
		t.Fatalf("regex: err=%v res=%+v", err, res)
	}
	for _, r := range *reqs {
		if r.URL.Path == "/repos/o/mesen/actions/workflows" {
			t.Error("a workflow file name shouldn't need the workflows listing")
		}
	}

	src = base
	src.Artifact = "nope"
	_, err = GitHubActions{}.Resolve(context.Background(), c, &def.App{Source: src}, nil)
	var soft *SoftError
	if err == nil || errors.As(err, &soft) || !strings.Contains(err.Error(), "Mesen (Windows)") {
		t.Errorf("missing artifact on a finished run should be a hard error listing what exists: %v", err)
	}

	// status: any may catch a run before it uploads (the suwayomi case): soft.
	src.Status = "any"
	if _, err = (GitHubActions{}).Resolve(context.Background(), c, &def.App{Source: src}, nil); !errors.As(err, &soft) {
		t.Errorf("status any + missing artifact should be soft: %v", err)
	}

	src = base
	src.Artifact = "old-thing"
	if _, err = (GitHubActions{}).Resolve(context.Background(), c, &def.App{Source: src}, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}

	src = base
	src.Workflow, src.Artifact = "No Such Workflow", "x"
	if _, err = (GitHubActions{}).Resolve(context.Background(), c, &def.App{Source: src}, nil); err == nil || !strings.Contains(err.Error(), "Build Mesen") {
		t.Errorf("unknown workflow should list the real ones: %v", err)
	}

	src = base
	src.Artifact = "x"
	if _, err = (GitHubActions{}).Resolve(context.Background(), client(srv.URL, ""), &def.App{Source: src}, nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("no token: %v", err)
	}
}

func TestForgejo(t *testing.T) {
	srv, _ := fixtureServer(t, map[string]string{"/api/v1/repos/eden-ci/nightly/releases": "forgejo_releases.json"})
	app := &def.App{Source: def.Source{Type: def.SourceForgejo, Host: srv.URL, Repo: "eden-ci/nightly"},
		Asset: &def.Asset{Regex: `Eden-Linux.*amd64-clang-pgo.AppImage$`, Pick: "first"}}
	res, err := Forgejo{}.Resolve(context.Background(), client("", ""), app, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Version is the URL (nightlies reuse tags); the tag is only for display.
	if res.Version != "https://git.example/dl/123/Eden-Linux-amd64-clang-pgo.AppImage" || res.Display != "v0.0.4-nightly.123" || res.URL != res.Version {
		t.Errorf("%+v", res)
	}
}

func TestGitLab(t *testing.T) {
	srv, reqs := fixtureServer(t, map[string]string{"/api/v4/projects/bighead.0%2Fladxhd_updated/releases": "gitlab_releases.json"})
	app := &def.App{Source: def.Source{Type: def.SourceGitLab, Host: srv.URL, Project: "bighead.0/ladxhd_updated"},
		Asset: &def.Asset{Regex: `Patcher[.-]Lite-Linux-x64$`, Pick: "first"}}
	res, err := GitLab{}.Resolve(context.Background(), client("", ""), app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "v1.6.2" || res.Filename != "Patcher.Lite-Linux-x64.7z" || !strings.Contains(res.URL, "/-/releases/v1.6.2/downloads/") {
		t.Errorf("%+v", res)
	}
	if got := (*reqs)[0].URL.EscapedPath(); !strings.Contains(got, "bighead.0%2Fladxhd_updated") {
		t.Errorf("project path must be URL-encoded, got %s", got)
	}
}

func TestHTML(t *testing.T) {
	srv, _ := fixtureServer(t, map[string]string{"/jaguar/index.php": "page.html"})
	page := srv.URL + "/jaguar/index.php?content=download"
	cases := map[string]struct {
		src                    def.Source
		version, url, filename string
	}{
		"relative href, deduped, basename version": {
			def.Source{Select: `a[href*="Linux64"]`, Attr: "href", ExcludeRegex: `\.sig$`},
			"BigPEmu_Linux64_v121.tar.gz", srv.URL + "/jaguar/files/BigPEmu_Linux64_v121.tar.gz", "BigPEmu_Linux64_v121.tar.gz"},
		"pick last with structural selector": {
			def.Source{Select: `tr.file:last-of-type > td:nth-child(1) > a:nth-child(1)`, Attr: "href"},
			"new.appImage", srv.URL + "/jaguar/new.appImage", "new.appImage"},
		"regex version from an absolute URL": {
			def.Source{Select: `a[href$="flycast-x86_64.AppImage"]`, Attr: "href", Version: `regex:master-([^/]+)/`},
			"abc1234", "https://cdn.example/builds/master-abc1234/flycast-x86_64.AppImage", "flycast-x86_64.AppImage"},
		"element text plus download template": {
			def.Source{Select: "#ver", Version: "full", Download: "https://dl.example/{{.Raw}}/app-{{.Version}}.tgz"},
			srv.URL + "/jaguar/4.2.1", "https://dl.example/4.2.1/app-" + srv.URL + "/jaguar/4.2.1.tgz", ""},
		"include filter": {
			def.Source{Select: "a", Attr: "href", IncludeRegex: `\.tar\.gz$`, Pick: "last"},
			"BigPEmu_Linux64_v121.tar.gz", srv.URL + "/jaguar/files/BigPEmu_Linux64_v121.tar.gz", "BigPEmu_Linux64_v121.tar.gz"},
	}
	for name, tc := range cases {
		tc.src.Type, tc.src.URL = def.SourceHTML, page
		res, err := HTML{}.Resolve(context.Background(), client("", ""), &def.App{Source: tc.src}, nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if res.Version != tc.version || res.URL != tc.url || (tc.filename != "" && res.Filename != tc.filename) {
			t.Errorf("%s:\n got version=%q url=%q file=%q\nwant version=%q url=%q file=%q", name, res.Version, res.URL, res.Filename, tc.version, tc.url, tc.filename)
		}
	}

	for name, src := range map[string]def.Source{
		"selector matches nothing": {Select: "a.download-button", Attr: "href"},
		"filters remove all":       {Select: "a", Attr: "href", IncludeRegex: `\.deb$`},
		"version regex no match":   {Select: "a", Attr: "href", Version: `regex:v(\d+\.\d+\.\d+)`},
	} {
		src.Type, src.URL = def.SourceHTML, page
		if _, err := (HTML{}).Resolve(context.Background(), client("", ""), &def.App{Source: src}, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestHTMLCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	src := def.Source{Type: def.SourceHTML, Command: "cat testdata/page.html", Select: `a[href$=".AppImage"]`, Attr: "href", Version: `regex:master-([^/]+)/`}
	res, err := HTML{}.Resolve(context.Background(), client("", ""), &def.App{Source: src}, nil)
	if err != nil || res.Version != "abc1234" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	src.Command = "echo 'render failed' >&2; exit 3"
	if _, err = (HTML{}).Resolve(context.Background(), client("", ""), &def.App{Source: src}, nil); err == nil || !strings.Contains(err.Error(), "render failed") {
		t.Errorf("a failing command should surface its stderr: %v", err)
	}
}

func TestHTTPETag(t *testing.T) {
	var method, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ua = r.Method, r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/etag/mGBA.appimage":
			w.Header().Set("ETag", `W/"5f3c-abc"`)
		case "/lastmod/app":
			w.Header().Set("Last-Modified", "Mon, 01 Sep 2026 10:00:00 GMT")
		}
	}))
	defer srv.Close()

	app := &def.App{Source: def.Source{Type: def.SourceHTTPETag, URL: srv.URL + "/etag/mGBA.appimage", Headers: map[string]string{"User-Agent": "curl/8"}}}
	res, err := HTTPETag{}.Resolve(context.Background(), client("", ""), app, nil)
	if err != nil || res.Version != "5f3c-abc" || res.URL != app.Source.URL || res.Filename != "mGBA.appimage" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if method != http.MethodHead || ua != "curl/8" {
		t.Errorf("method=%s ua=%s (definition headers should override the default UA)", method, ua)
	}

	app.Source.URL = srv.URL + "/lastmod/app"
	if res, err = (HTTPETag{}).Resolve(context.Background(), client("", ""), app, nil); err != nil || !strings.HasPrefix(res.Version, "Mon, 01 Sep") {
		t.Errorf("Last-Modified fallback: err=%v res=%+v", err, res)
	}
	app.Source.URL = srv.URL + "/neither"
	var soft *SoftError
	if _, err = (HTTPETag{}).Resolve(context.Background(), client("", ""), app, nil); !errors.As(err, &soft) {
		t.Errorf("no validators should be soft: %v", err)
	}
}

func TestYAMLAndJSONDocuments(t *testing.T) {
	srv, _ := fixtureServer(t, map[string]string{"/latest.yaml": "latest.yaml", "/releases.json": "releases.json"})
	c := client("", "")

	app := &def.App{Source: def.Source{Type: def.SourceYAML, URL: srv.URL + "/latest.yaml",
		Version:  `.latest[] | select(.name=="stable") | .version`,
		Download: "https://cdn.example/{{.Version}}/openttd-{{.Version}}-linux-generic-amd64.tar.xz"}}
	res, err := Document{YAML: true}.Resolve(context.Background(), c, app, nil)
	if err != nil || res.Version != "14.1" || res.URL != "https://cdn.example/14.1/openttd-14.1-linux-generic-amd64.tar.xz" || res.Filename != "openttd-14.1-linux-generic-amd64.tar.xz" {
		t.Fatalf("yaml: err=%v res=%+v", err, res)
	}
	// YAML dates decode to time.Time; jq must still be able to read them.
	app.Source.Version = `.latest[0].date`
	if res, err = (Document{YAML: true}).Resolve(context.Background(), c, app, nil); err != nil || !strings.HasPrefix(res.Version, "2026-09-01") {
		t.Errorf("yaml date: err=%v res=%+v", err, res)
	}

	app = &def.App{Source: def.Source{Type: def.SourceJSON, URL: srv.URL + "/releases.json",
		Version: `[.[] | select(.draft|not)][0].tag_name`,
		URLJQ:   `[.[] | select(.draft|not)][0].assets[] | select(.name|test("x86_64.AppImage$")) | .browser_download_url`}}
	res, err = Document{}.Resolve(context.Background(), c, app, nil)
	if err != nil || res.Version != "v2.0.0-beta1" || res.URL != "https://dl.example/beta/app-x86_64.AppImage" {
		t.Fatalf("json: err=%v res=%+v", err, res)
	}

	for name, expr := range map[string]string{"no output": `.nothing_here`, "syntax error": `.[`, "runtime error": `.[0] | error("boom")`} {
		app.Source.Version = expr
		if _, err := (Document{}).Resolve(context.Background(), c, app, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The Lua examples are real definitions (snes9x, flycast); run them against
// recorded upstream responses so they stay honest.
func exampleScript(t *testing.T, id, upstream, replacement string) *def.App {
	t.Helper()
	app, err := def.LoadFile("../../examples/apps.d/"+id+".yaml", def.Vars{AppDir: "/apps", AppImageDir: "/apps/appimages", Home: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	code := app.Source.Lua
	if app.Source.ScriptFile != "" {
		data, err := os.ReadFile(app.ScriptPath())
		if err != nil {
			t.Fatal(err)
		}
		code, app.Source.ScriptFile = string(data), ""
	}
	if !strings.Contains(code, upstream) {
		t.Fatalf("%s no longer mentions %s", id, upstream)
	}
	app.Source.Lua = strings.ReplaceAll(code, upstream, replacement)
	return app
}

// Eden publishes builds only as links in its release notes. The fixture is a
// real API response.
func TestScriptEdenExample(t *testing.T) {
	srv, _ := fixtureServer(t, map[string]string{"/api/v1/repos/eden-ci/nightly/releases": "eden_releases.json"})
	app := exampleScript(t, "eden", "https://git.eden-emu.dev", srv.URL)
	res, err := Script{}.Resolve(context.Background(), client("", ""), app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Version, "v") || !strings.HasPrefix(res.URL, "https://nightly.eden-emu.dev/"+res.Version+"/Eden-Linux-") ||
		!strings.HasSuffix(res.URL, "-amd64-clang-pgo.AppImage") || res.Filename != res.URL[strings.LastIndex(res.URL, "/")+1:] {
		t.Errorf("%+v", res)
	}
	// The legacy/steamdeck/aarch64 PGO builds end the same way; only amd64 may match.
	if strings.Contains(res.URL, "legacy") || strings.Contains(res.URL, "steamdeck") {
		t.Errorf("picked the wrong build: %s", res.URL)
	}

	// Upstream rewording the notes must be a loud failure, not a silent skip.
	app.Source.Lua = strings.Replace(app.Source.Lua, `"amd64%-clang%-pgo"`, `"riscv%-clang%-pgo"`, 1)
	var soft *SoftError
	if _, err := (Script{}).Resolve(context.Background(), client("", ""), app, nil); err == nil || errors.As(err, &soft) || !strings.Contains(err.Error(), "riscv-clang-pgo") {
		t.Errorf("err = %v", err)
	}
}

func TestScriptFlycastExample(t *testing.T) {
	srv, reqs := fixtureServer(t, map[string]string{"/": "flycast_bucket.xml"})
	app := exampleScript(t, "flycast", "https://flycast-builds.s3.fr-par.scw.cloud/", srv.URL+"/")
	res, err := Script{}.Resolve(context.Background(), client("", ""), app, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Newest by LastModified among master Linux builds: not first in the
	// listing, and not the Windows or dev-branch entries.
	const commit = "0abac3465dc9547dca5f30f3352fee10b67e34b2"
	if res.Version != commit || res.Display != "0abac34" || res.URL != srv.URL+"/linux/heads/master-"+commit+"/flycast-x86_64.AppImage" || res.Filename != "flycast-x86_64.AppImage" {
		t.Errorf("%+v", res)
	}
	if q := (*reqs)[0].URL.RawQuery; q != "prefix=linux/heads/master-" {
		t.Errorf("listing query = %q", q)
	}
}

func TestSkipUnmatched(t *testing.T) {
	c, _ := fakeGitHub(t)
	// Newest release (the beta) has no .deb... nor does any other: error names the newest.
	a := app(def.Source{Repo: "o/app", SkipUnmatched: true}, def.Asset{Glob: "*.deb"})
	if _, err := (GitHubRelease{}).Resolve(context.Background(), c, a, nil); err == nil || !strings.Contains(err.Error(), "none of the last") {
		t.Errorf("err = %v", err)
	}
	// Only v1.9.0 ships "app-1.9.0-*": looked past the beta to find it.
	a = app(def.Source{Repo: "o/app", SkipUnmatched: true, VersionFrom: "tag"}, def.Asset{Glob: "app-1.9.0-*"})
	res, err := GitHubRelease{}.Resolve(context.Background(), c, a, nil)
	if err != nil || res.Version != "v1.9.0" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	// Without the option the same definition fails on the newest release.
	a.Source.SkipUnmatched = false
	if _, err := (GitHubRelease{}).Resolve(context.Background(), c, a, nil); err == nil {
		t.Error("strict latest should fail when the newest release lacks the asset")
	}
}

func TestHTMLBackslashHref(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!-- <a href='../old/app-0.9b-linux-x64.tgz'>retired beta</a> -->
			<a href='releases\app-4.0-linux-x64.tgz?dl=a\b'>Download for x64 Linux</a>`))
	}))
	defer srv.Close()
	src := def.Source{Type: def.SourceHTML, URL: srv.URL + "/", Select: `a[href*="linux-x64"]`, Attr: "href"}
	res, err := HTML{}.Resolve(context.Background(), client("", ""), &def.App{Source: src}, nil)
	// Commented-out markup isn't part of the page; backslashes in the path
	// resolve as a browser would, and the query string is left alone.
	if err != nil || res.URL != srv.URL+`/releases/app-4.0-linux-x64.tgz?dl=a\b` || res.Version != "app-4.0-linux-x64.tgz" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

const dolphinRef = "[Flatpak Ref]\nName=org.DolphinEmu.dolphin-emu\nBranch=beta\nUrl=https://flatpak.dolphin-emu.org/dev\nSuggestRemoteName=dolphin-dev\n"

func TestFlatpakSource(t *testing.T) {
	fake := systest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(dolphinRef)) }))
	defer srv.Close()
	const ref = "org.DolphinEmu.dolphin-emu//beta"
	app := &def.App{Source: def.Source{Type: def.SourceFlatpak, Ref: srv.URL + "/dev.flatpakref"}, Install: def.Install{Type: def.InstallFlatpak}}
	resolve := func() *Result {
		t.Helper()
		res, err := Flatpak{UserScope: true}.Resolve(context.Background(), client("", ""), app, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Never installed: the remote it names isn't configured yet, so there's
	// nothing to ask; the ref file itself is the download.
	res := resolve()
	if res.Version != "not-installed:"+ref || res.URL != app.Source.Ref || res.Filename != "dev.flatpakref" || res.Installed != "" {
		t.Errorf("not installed: %+v", res)
	}

	// Installed and behind: the version is what the remote offers now, asked
	// of the remote it was actually installed from.
	fake.SetFlatpak(ref, "aaaa1111aaaa", "dolphin-dev")
	fake.SetRemote(ref, "bbbb2222bbbb", "2606-400")
	res = resolve()
	if res.Version != "bbbb2222bbbb" || res.Display != "2606-400 (bbbb2222bb)" || res.Installed != "aaaa1111aaaa" {
		t.Errorf("behind: %+v", res)
	}
	if calls := strings.Join(fake.Calls("flatpak"), "\n"); !strings.Contains(calls, "remote-info --user dolphin-dev "+ref) {
		t.Errorf("calls: %s", calls)
	}

	// An app on a configured remote needs no ref file and no download.
	fake.SetRemote("org.libretro.RetroArch//stable", "cccc3333", "1.22.2")
	app = &def.App{Source: def.Source{Type: def.SourceFlatpak, Remote: "flathub", App: "org.libretro.RetroArch"}, Install: def.Install{Type: def.InstallFlatpak, Scope: "system"}}
	res = resolve()
	if res.Version != "cccc3333" || res.URL != "" {
		t.Errorf("remote app: %+v", res)
	}
	if calls := strings.Join(fake.Calls("flatpak"), "\n"); !strings.Contains(calls, "remote-info --system flathub org.libretro.RetroArch//stable") {
		t.Errorf("install.scope should pick the installation: %s", calls)
	}
}
