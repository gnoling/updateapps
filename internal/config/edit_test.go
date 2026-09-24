package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const handWritten = `# my settings
appdir: ~/apps

repositories:
  - name: main
    url: https://github.com/gnoling/updateapps-definitions
    default: disabled        # opt-in
  - name: private
    url: https://github.com/me/private
    trusted: true            # deploy hooks

enabled: []
disabled:
  - mesen                    # archived upstream
  - snes9x

jobs: 4                      # laptop
`

func edit(t *testing.T, before string, fn func(*Editor) error) string {
	t.Helper()
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if before != "" {
		os.WriteFile(path, []byte(before), 0o644)
	}
	e, err := OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(e); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	return string(after)
}

// changed returns the lines that differ, so a test can assert nothing else moved.
func changed(before, after string) (removed, added []string) {
	have := map[string]int{}
	for _, l := range strings.Split(before, "\n") {
		have[l]++
	}
	for _, l := range strings.Split(after, "\n") {
		if have[l] > 0 {
			have[l]--
		} else {
			added = append(added, l)
		}
	}
	for l, n := range have {
		for ; n > 0; n-- {
			removed = append(removed, l)
		}
	}
	return
}

// A hand-maintained file keeps its comments, blank lines and alignment: only
// the lines that must change do.
func TestEditsTouchOnlyWhatTheyMust(t *testing.T) {
	after := edit(t, handWritten, func(e *Editor) error {
		if err := e.SetListed("enabled", "dolphin", true); err != nil {
			return err
		}
		if err := e.SetListed("enabled", "rpcs3", true); err != nil {
			return err
		}
		if err := e.SetListed("disabled", "snes9x", false); err != nil {
			return err
		}
		return e.SetRepoDefault("main", "enabled")
	})
	removed, added := changed(handWritten, after)
	wantRemoved := "    default: disabled        # opt-in|  - snes9x|enabled: []"
	wantAdded := "    default: enabled        # opt-in|enabled:|  - dolphin|  - rpcs3"
	if got := strings.Join(sorted(removed), "|"); got != strings.Join(sorted(strings.Split(wantRemoved, "|")), "|") {
		t.Errorf("removed lines:\n%q", removed)
	}
	if got := strings.Join(sorted(added), "|"); got != strings.Join(sorted(strings.Split(wantAdded, "|")), "|") {
		t.Errorf("added lines:\n%q", added)
	}
	c, err := Load(writeTemp(t, after))
	if err != nil || strings.Join(c.Enabled, ",") != "dolphin,rpcs3" || strings.Join(c.Disabled, ",") != "mesen" || c.Repositories[0].Default != "enabled" {
		t.Errorf("err=%v enabled=%v disabled=%v repos=%+v\n%s", err, c.Enabled, c.Disabled, c.Repositories, after)
	}
}

func TestListShapes(t *testing.T) {
	for name, tc := range map[string]struct{ before, key, value, want string }{
		"no key yet":            {"jobs: 4\n", "enabled", "a", "jobs: 4\n\nenabled:\n  - a\n"},
		"no file at all":        {"", "enabled", "a", "enabled:\n  - a\n"},
		"bare key":              {"enabled:\njobs: 4\n", "enabled", "a", "enabled:\n  - a\njobs: 4\n"},
		"empty flow + comment":  {"enabled: []   # mine\n", "enabled", "a", "enabled:   # mine\n  - a\n"},
		"flow list stays flow":  {"enabled: [a, b]\n", "enabled", "c", "enabled: [a, b, c]\n"},
		"unindented block list": {"enabled:\n- a\n", "enabled", "b", "enabled:\n- a\n- b\n"},
		"already present":       {"enabled:\n  - a\n", "enabled", "a", "enabled:\n  - a\n"},
	} {
		got := edit(t, tc.before, func(e *Editor) error { return e.SetListed(tc.key, tc.value, true) })
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct{ before, want string }{
		"last item leaves []": {"disabled:   # off here\n  - a\njobs: 4\n", "disabled: []   # off here\njobs: 4\n"},
		"middle item":         {"disabled:\n  - x\n  - a   # why\n  - y\n", "disabled:\n  - x\n  - y\n"},
		"from a flow list":    {"disabled: [x, a]\n", "disabled: [x]\n"},
		"absent is a no-op":   {"disabled:\n  - x\n", "disabled:\n  - x\n"},
		"no such key is fine": {"jobs: 4\n", "jobs: 4\n"},
	} {
		got := edit(t, tc.before, func(e *Editor) error { return e.SetListed("disabled", "a", false) })
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

func TestRepoDefault(t *testing.T) {
	// No repositories: key means the built-in default is in effect; changing
	// it has to write it out.
	got := edit(t, "appdir: ~/apps\n", func(e *Editor) error { return e.SetRepoDefault("main", "enabled") })
	want := "appdir: ~/apps\n\nrepositories:\n  - name: main\n    url: " + DefaultRepository.URL + "\n    default: enabled\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// A repository with no default: line gets one, indented like its siblings.
	got = edit(t, "repositories:\n  - name: mine\n    trusted: true\njobs: 4\n", func(e *Editor) error { return e.SetRepoDefault("mine", "disabled") })
	if got != "repositories:\n  - name: mine\n    trusted: true\n    default: disabled\njobs: 4\n" {
		t.Errorf("got %q", got)
	}
	e, _ := OpenEditor(writeTemp(t, handWritten))
	if err := e.SetRepoDefault("nope", "enabled"); err == nil {
		t.Error("unknown repository should be an error")
	}
}

// An edit that would leave an unloadable config is never written.
func TestSaveRefusesToBreakTheConfig(t *testing.T) {
	clearEnv(t)
	path := writeTemp(t, "repositories:\n  - name: a\n    url: https://x/o/r\n")
	e, _ := OpenEditor(path)
	if err := e.SetRepoDefault("a", "sometimes"); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err == nil {
		t.Error("an invalid default: should not be saved")
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "sometimes") {
		t.Error("the broken edit reached the file")
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(body), 0o644)
	return path
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestScalars(t *testing.T) {
	const before = `appdir: ~/apps
jobs: 4                      # laptop
# privilege: auto        # how .deb installs get root
# state defaults to ~/.local/state/updateapps/state.json
`
	got := edit(t, before, func(e *Editor) error {
		for _, step := range []struct {
			key   string
			value any
		}{
			{"jobs", 8}, {"privilege", "sudo"}, {"appdir", nil}, {"github_token", "x#y"}, {"auto_pull", true}, {"missing", nil},
		} {
			if err := e.SetScalar(step.key, step.value); err != nil {
				return err
			}
		}
		return nil
	})
	want := `jobs: 8                      # laptop
privilege: sudo        # how .deb installs get root
# state defaults to ~/.local/state/updateapps/state.json
github_token: x#y
auto_pull: true
`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	c, err := Load(writeTemp(t, got))
	if err != nil || c.Jobs != 8 || c.Privilege != "sudo" || c.GitHubToken != "x#y" || !c.AutoPull {
		t.Errorf("err=%v %+v", err, c)
	}
	e, _ := OpenEditor(writeTemp(t, "notes: |\n  two\n  lines\n"))
	if err := e.SetScalar("notes", "x"); err == nil {
		t.Error("a multi-line value should refuse")
	}
}

func TestSetApp(t *testing.T) {
	got := edit(t, "enabled:\n  - a\ndisabled:\n  - main/b\n  - c\n", func(e *Editor) error {
		if err := e.SetApp("b", "main", true); err != nil {
			return err
		}
		return e.SetApp("a", "main", false)
	})
	if got != "enabled:\n  - b\ndisabled:\n  - c\n  - a\n" {
		t.Errorf("got %q", got)
	}
}

func TestRepositoryEdits(t *testing.T) {
	after := edit(t, handWritten, func(e *Editor) error {
		if err := e.SetRepoField("main", "trusted", true); err != nil {
			return err
		}
		if err := e.SetRepoField("private", "trusted", nil); err != nil {
			return err
		}
		if err := e.SetRepoField("private", "branch", "dev"); err != nil {
			return err
		}
		return e.AddRepository(Repository{Name: "work", URL: "https://x/o/r", Default: "disabled"})
	})
	removed, added := changed(handWritten, after)
	wantRemoved := "    trusted: true            # deploy hooks"
	wantAdded := "    trusted: true|    branch: dev|  - name: work|    url: https://x/o/r|    default: disabled"
	if got := strings.Join(sorted(removed), "|"); got != strings.Join(sorted(strings.Split(wantRemoved, "|")), "|") {
		t.Errorf("removed lines:\n%q", removed)
	}
	if got := strings.Join(sorted(added), "|"); got != strings.Join(sorted(strings.Split(wantAdded, "|")), "|") {
		t.Errorf("added lines:\n%q\n%s", added, after)
	}
	c, err := Load(writeTemp(t, after))
	if err != nil || len(c.Repositories) != 3 || !c.Repositories[0].Trusted || c.Repositories[1].Trusted ||
		c.Repositories[1].Branch != "dev" || c.Repositories[2].Name != "work" {
		t.Errorf("err=%v repos=%+v\n%s", err, c.Repositories, after)
	}
	// The new one goes after the last entry, not after its comment block.
	if !strings.HasSuffix(after, "  - name: work\n    url: https://x/o/r\n    default: disabled\n\nenabled: []\ndisabled:\n  - mesen                    # archived upstream\n  - snes9x\n\njobs: 4                      # laptop\n") {
		t.Errorf("placement:\n%s", after)
	}

	// Removing entries; the last one leaves [] so the built-in default doesn't
	// come back.
	got := edit(t, after, func(e *Editor) error {
		for _, n := range []string{"private", "main", "work"} {
			if err := e.RemoveRepository(n); err != nil {
				return err
			}
		}
		return nil
	})
	if !strings.Contains(got, "\nrepositories: []\n\nenabled: []\n") || strings.Contains(got, "name:") {
		t.Errorf("got %q", got)
	}
	if c, err := Load(writeTemp(t, got)); err != nil || len(c.Repositories) != 0 {
		t.Errorf("err=%v repos=%+v", err, c.Repositories)
	}
	// And adding to [] grows a block list again.
	got = edit(t, got, func(e *Editor) error { return e.AddRepository(Repository{Name: "n", Path: "~/defs", Trusted: true}) })
	if !strings.Contains(got, "\nrepositories:\n  - name: n\n    path: ~/defs\n    trusted: true\n\nenabled: []\n") {
		t.Errorf("got %q", got)
	}

	// No repositories: key means the built-in default is in effect; any edit
	// writes it out first so it isn't lost.
	got = edit(t, "jobs: 4\n", func(e *Editor) error { return e.AddRepository(Repository{Name: "n", URL: "https://x/o/r"}) })
	if got != "jobs: 4\n\nrepositories:\n  - name: main\n    url: "+DefaultRepository.URL+"\n    default: disabled\n  - name: n\n    url: https://x/o/r\n" {
		t.Errorf("got %q", got)
	}
	e, _ := OpenEditor(writeTemp(t, handWritten))
	if err := e.AddRepository(Repository{Name: "main", URL: "https://x"}); err == nil {
		t.Error("duplicate name should be an error")
	}
	if err := e.SetRepoField("main", "name", "other"); err == nil {
		t.Error("renaming should be an error")
	}
}
