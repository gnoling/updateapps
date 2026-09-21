package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/repo"
)

func render(verbose int, mode engine.Mode, evs ...engine.Event) string {
	var buf bytes.Buffer
	ch := make(chan engine.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	(&Renderer{Out: &buf, Verbose: verbose, Mode: mode}).Drain(ch)
	return buf.String()
}

// The output format is the bash script's, line for line (see SPEC "Output format").
func TestOutputMatchesBashFormat(t *testing.T) {
	got := render(0, engine.ModeUpdate,
		engine.Event{Kind: engine.Queued, Name: "ignored"},
		engine.Event{Kind: engine.Downloading, Name: "ignored", Bytes: 5, Total: 10},
		engine.Event{Kind: engine.UpToDate, Final: true, Name: "quiet", Version: "v1"},
		engine.Event{Kind: engine.Installed, Final: true, Name: "gearboy", Version: "v3.7.1", Description: "Game Boy emulator",
			Log: []engine.LogLine{{Level: 1, Text: "hidden without -v"}}},
		engine.Event{Kind: engine.Installed, Final: true, Name: "LADXHD", Version: "v1.6.2", Description: "Link's Awakening DX HD remake",
			Message: "run /home/u/apps/ladxhd/Patcher-Lite\nchoose platform Linux + target OpenGL, then delete the patcher when done"},
		engine.Event{Kind: engine.Failed, Final: true, Name: "fbneo", Message: "download failed: 502 Bad Gateway", Err: errors.New("x")},
		engine.Event{Kind: engine.Skipped, Final: true, Name: "dolphin", Message: "release \"continuous\" not found; will retry next run"},
		engine.Event{Kind: engine.RunFinished, Summary: &engine.Summary{Total: 118, Updated: 7, Failed: 1, Elapsed: 72 * time.Second}},
	)
	want := `gearboy (v3.7.1) — Game Boy emulator
LADXHD (v1.6.2) — Link's Awakening DX HD remake ** ACTION NEEDED: run /home/u/apps/ladxhd/Patcher-Lite
  choose platform Linux + target OpenGL, then delete the patcher when done
fbneo ** download failed: 502 Bad Gateway
dolphin ** release "continuous" not found; will retry next run
Updated 7 out of 118 apps in 1 minute(s), 12 second(s) (1 failed)
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestVerboseLevels(t *testing.T) {
	ev := engine.Event{Kind: engine.UpToDate, Final: true, Name: "rmg", Version: "v0.9.0",
		Log: []engine.LogLine{{Level: 2, Text: "GET https://api.github.com/x -> 304"}, {Level: 1, Text: "not modified (ETag)"}}}
	if got := render(0, engine.ModeUpdate, ev); got != "" {
		t.Errorf("up-to-date apps print nothing by default, got %q", got)
	}
	if got := render(1, engine.ModeUpdate, ev); got != "rmg (v0.9.0) [up to date]\n  not modified (ETag)\n" {
		t.Errorf("-v: %q", got)
	}
	if got := render(2, engine.ModeUpdate, ev); !strings.Contains(got, "  GET https://") {
		t.Errorf("-vv: %q", got)
	}
}

func TestMarkCurrentOutput(t *testing.T) {
	got := render(0, engine.ModeMarkCurrent,
		engine.Event{Kind: engine.Marked, Final: true, Name: "gearboy", Version: "3.8.15"},
		engine.Event{Kind: engine.Skipped, Final: true, Name: "ymir", Version: "nightly", Message: "not marked: /apps/ymir doesn't exist, so the next update will install it"},
		engine.Event{Kind: engine.RunFinished, Summary: &engine.Summary{Total: 2, Updated: 1, Skipped: 1, Elapsed: time.Second}})
	want := "gearboy (3.8.15) [marked current]\nymir (nightly) ** not marked: /apps/ymir doesn't exist, so the next update will install it\n" +
		"Marked 1 out of 2 apps as current in 0 minute(s), 1 second(s) (1 left unmarked)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSystemPackageOutput(t *testing.T) {
	got := render(0, engine.ModeUpdate,
		engine.Event{Kind: engine.Pending, Name: "bat"},
		engine.Event{Kind: engine.Finalizing, Message: "Installing 2 system packages: bat, fd"},
		engine.Event{Kind: engine.Installed, Final: true, Name: "bat", Version: "v0.25.0"},
		engine.Event{Kind: engine.ActionNeeded, Final: true, Name: "fd", Version: "v10.2.0", Message: "fd 10.2.0 is downloaded but installing it needs root:\nsudo apt-get install /home/u/.cache/updateapps/pending/fd_10.2.0_amd64.deb"},
		engine.Event{Kind: engine.RunFinished, Summary: &engine.Summary{Total: 2, Updated: 1, Action: 1, Elapsed: time.Second}})
	want := `Installing 2 system packages: bat, fd
bat (v0.25.0)
fd (v10.2.0) ** ACTION NEEDED: fd 10.2.0 is downloaded but installing it needs root:
  sudo apt-get install /home/u/.cache/updateapps/pending/fd_10.2.0_amd64.deb
Updated 1 out of 2 apps in 0 minute(s), 1 second(s) (1 need you to finish the install)
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCheckSummary(t *testing.T) {
	got := render(0, engine.ModeCheck, engine.Event{Kind: engine.RunFinished, Summary: &engine.Summary{Total: 9, Updated: 2, Elapsed: time.Second}})
	if got != "2 out of 9 apps have updates; checked in 0 minute(s), 1 second(s)\n" {
		t.Errorf("%q", got)
	}
}

// Commands that need no network, run through the real cobra tree.
func TestOfflineCommands(t *testing.T) {
	dir := t.TempDir()
	defs := filepath.Join(dir, "mydefs")
	os.Mkdir(defs, 0o755)
	os.WriteFile(filepath.Join(defs, "good.yaml"), []byte("source: {type: github-release, repo: o/r}\n"), 0o644)
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("appdir: "+dir+"/apps\nstate: "+dir+"/state.json\nrepositories: [{name: t, path: "+defs+"}]\n"), 0o644)
	for _, k := range []string{"APPDIR", "APPIMAGEDIR", "MAXJOBS", "FORCE", "VERBOSE"} {
		t.Setenv(k, "")
	}

	if code := Main([]string{"--config", cfg, "validate"}); code != ExitOK {
		t.Errorf("validate = %d", code)
	}
	if code := Main([]string{"--config", cfg, "list"}); code != ExitOK {
		t.Errorf("list = %d", code)
	}
	if code := Main([]string{"--config", cfg, "show", "good"}); code != ExitOK {
		t.Errorf("show = %d", code)
	}
	if code := Main([]string{"--config", cfg, "show", "nope"}); code != ExitConfig {
		t.Errorf("show missing = %d", code)
	}
	if code := Main([]string{"--config", cfg, "no-such-app"}); code != ExitConfig {
		t.Errorf("filter matching nothing = %d, want %d", code, ExitConfig)
	}

	os.WriteFile(filepath.Join(defs, "bad.yaml"), []byte("source: {type: github-release}\n"), 0o644)
	if code := Main([]string{"--config", cfg, "validate"}); code != ExitConfig {
		t.Errorf("validate with a bad definition = %d, want %d", code, ExitConfig)
	}
	if code := Main([]string{"--config", filepath.Join(dir, "missing.yaml"), "--defs", filepath.Join(dir, "nowhere"), "validate"}); code != ExitConfig {
		t.Errorf("missing defs dir = %d", code)
	}
}

func TestRepositoriesFromConfig(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"APPDIR", "APPIMAGEDIR", "MAXJOBS", "FORCE", "VERBOSE"} {
		t.Setenv(k, "")
	}
	mine, theirs := filepath.Join(dir, "mine"), filepath.Join(dir, "theirs", "apps.d")
	os.MkdirAll(mine, 0o755)
	os.MkdirAll(theirs, 0o755)
	os.WriteFile(filepath.Join(mine, "a.yaml"), []byte("source: {type: github-release, repo: o/r}\n"), 0o644)
	os.WriteFile(filepath.Join(theirs, "hooks.yaml"), []byte("source: {type: github-release, repo: o/r}\npost: [true]\n"), 0o644)
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("appdir: "+dir+"/apps\nstate: "+dir+"/state.json\nrepositories:\n"+
		"  - {name: theirs, path: "+filepath.Dir(theirs)+"}\n  - {name: mine, path: "+mine+", trusted: true}\n"+
		"  - {name: remote, url: 'https://github.com/o/defs'}\n"), 0o644)

	// An unfetched repository is reported (validate exits 2) but doesn't stop
	// the rest loading.
	if code := Main([]string{"--config", cfg, "validate"}); code != ExitConfig {
		t.Errorf("validate = %d, want %d (one repository isn't fetched)", code, ExitConfig)
	}
	if code := Main([]string{"--config", cfg, "list"}); code != ExitOK {
		t.Errorf("list = %d", code)
	}
	if code := Main([]string{"--config", cfg, "repos"}); code != ExitOK {
		t.Errorf("repos = %d; listing repositories must work even when one is unusable", code)
	}
	if code := Main([]string{"--config", cfg, "show", "hooks"}); code != ExitOK {
		t.Errorf("show = %d", code)
	}
	// --defs replaces the configured repositories with one trusted folder.
	if code := Main([]string{"--config", cfg, "--defs", mine, "validate"}); code != ExitOK {
		t.Errorf("--defs validate = %d", code)
	}
}

// `repos pull NAME` touches only that repository.
func TestReposPullByName(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("repositories:\n  - {name: a, url: '"+srv.URL+"/a.tar.gz'}\n  - {name: b, url: '"+srv.URL+"/b.tar.gz'}\n"), 0o644)
	if code := Main([]string{"--config", cfg, "repos", "pull", "a"}); code != ExitAppFailed {
		t.Errorf("exit = %d (the fake host 404s, so the pull fails)", code)
	}
	for _, h := range hits {
		if h != "/a.tar.gz" {
			t.Errorf("pulling a also requested %s", h)
		}
	}
	if code := Main([]string{"--config", cfg, "repos", "pull", "nope"}); code != ExitAppFailed {
		t.Errorf("unknown repository: exit = %d", code)
	}
}

func TestEnableDisable(t *testing.T) {
	for _, k := range []string{"APPDIR", "APPIMAGEDIR", "MAXJOBS", "FORCE", "VERBOSE"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	defs := filepath.Join(dir, "theirs")
	os.MkdirAll(defs, 0o755)
	plain := "source: {type: github-release, repo: o/r}\n"
	for name, body := range map[string]string{"a": plain, "b": plain, "dead": "enabled: false\n" + plain, "hooks": plain + "post: [true]\n"} {
		os.WriteFile(filepath.Join(defs, name+".yaml"), []byte(body), 0o644)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("state: "+dir+"/state.json\nrepositories:\n  - name: theirs\n    path: "+defs+"\n    default: disabled\n"), 0o644)
	run := func(args ...string) int { return Main(append([]string{"--config", cfgPath}, args...)) }
	enabled := func() string {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var on []string
		for _, a := range repo.Load(cfg).Apps {
			if a.IsEnabled() {
				on = append(on, a.ID)
			}
		}
		return strings.Join(on, ",")
	}

	if run("enable", "a", "theirs/b") != ExitOK || enabled() != "a,b" {
		t.Fatalf("after enable a b: %q", enabled())
	}
	if run("disable", "b") != ExitOK || enabled() != "a" {
		t.Fatalf("after disable b: %q", enabled())
	}
	if cfg, _ := config.Load(cfgPath); strings.Join(cfg.Enabled, ",") != "a" || strings.Join(cfg.Disabled, ",") != "b" {
		t.Errorf("an app is only ever in one list: enabled=%v disabled=%v", cfg.Enabled, cfg.Disabled)
	}
	if run("enable", "nope") != ExitConfig || run("enable") != ExitConfig {
		t.Error("an unknown app, or no app at all, is an error")
	}

	// --all flips the repository default, so it covers future definitions. A
	// choice made by name survives it, and so does a definition that disables itself.
	if run("enable", "--all") != ExitOK || enabled() != "a,hooks" {
		t.Errorf("after enable --all: %q (b is off by name, dead disables itself)", enabled())
	}
	if run("disable", "--all", "theirs") != ExitOK || enabled() != "a" {
		t.Errorf("after disable --all: %q (a is on by name)", enabled())
	}
	if run("enable", "--all", "nope") != ExitConfig {
		t.Error("unknown repository should be an error")
	}
	if run("--defs", defs, "enable", "a") != ExitConfig {
		t.Error("--defs has no config to edit")
	}
}
