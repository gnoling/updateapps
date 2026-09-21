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
