package gui

import (
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
)

func apps() []*def.App {
	off := false
	return []*def.App{
		{ID: "a", Name: "Alpha", Category: "tools", Source: def.Source{Type: def.SourceGitHubRelease, Repo: "o/alpha"}},
		{ID: "b", Name: "Beta", Category: "emulators", Enabled: &off},
		{ID: "c", Name: "Gamma", Category: "emulators", Blocked: "runs shell commands (post); repository x isn't trusted"},
	}
}

func TestStatusFollowsEventsThenState(t *testing.T) {
	var m Model
	m.Reset(apps(), nil)
	want := map[string]string{"a": "never installed", "b": "disabled", "c": "needs trust"}
	for id, w := range want {
		if got := m.byID[id].Status(); got != w {
			t.Errorf("%s: got %q, want %q", id, got, w)
		}
	}
	m.Apply(engine.Event{AppID: "a", Kind: engine.Downloading, Bytes: 50, Total: 200})
	if got := m.byID["a"].Status(); got != "downloading 25%" {
		t.Errorf("got %q", got)
	}
	m.Apply(engine.Event{AppID: "a", Kind: engine.Failed, Final: true, Message: "download failed: 502\nmore"})
	if got := m.byID["a"].Status(); got != "failed: download failed: 502" || !m.byID["a"].Attention() {
		t.Errorf("got %q", got)
	}
	m.Apply(engine.Event{AppID: "nope", Kind: engine.Installed}) // unknown ids are ignored
	m.ClearLive(apps())
	if got := m.byID["a"].Status(); got != "never installed" {
		t.Errorf("after clear: got %q", got)
	}
}

func TestFilters(t *testing.T) {
	var m Model
	m.Reset(apps(), nil)
	m.Apply(engine.Event{AppID: "a", Kind: engine.NewVersion, Final: true, Version: "2"})
	ids := func() string {
		var out []string
		for _, r := range m.Visible() {
			out = append(out, r.App.ID)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ search, category, show, want string }{
		{"", "", showAll, "a,b,c"},
		{"", "emulators", showAll, "b,c"},
		{"", "", showEnabled, "a"},
		{"", "", showDisabled, "b"},
		{"", "", showUpdates, "a"},
		{"", "", showNeedsTrust, "c"},
		{"alph", "", showAll, "a"},
		{"o/al", "", showAll, "a"}, // the upstream counts, as on the command line
		{"GAM", "tools", showAll, ""},
	} {
		m.Search, m.Category, m.Show = tc.search, tc.category, tc.show
		if got := ids(); got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
	if got := strings.Join(m.Categories(), ","); got != "emulators,tools" {
		t.Errorf("categories: %q", got)
	}
	if n := len(m.Runnable()); n != 1 {
		t.Errorf("runnable: %d", n)
	}
	m.byID["b"].Checked = true
	if n := m.TickUpdates(apps()); n != 1 || !m.byID["a"].Checked || m.byID["b"].Checked {
		t.Errorf("tick updates: n=%d a=%v b=%v", n, m.byID["a"].Checked, m.byID["b"].Checked)
	}
	m.byID["b"].Checked = true
	m.Reset(apps(), nil) // a reload keeps ticks and results
	if len(m.Checked()) != 2 || m.byID["a"].Live == nil {
		t.Error("reset lost the check mark or the last result")
	}
	// An update being applied, and its result, stay in the updates filter
	// until the app's next run.
	m.Search, m.Category, m.Show = "", "", showUpdates
	m.ClearLive([]*def.App{{ID: "a"}}) // the update run starts: still listed
	for _, k := range []engine.EventKind{engine.Checking, engine.NewVersion, engine.Downloading, engine.Installed} {
		m.Apply(engine.Event{AppID: "a", Kind: k, Final: k == engine.Installed})
		if got := ids(); got != "a" {
			t.Errorf("%s: %q", k, got)
		}
	}
	m.byID["a"].Checked, m.byID["b"].Checked = true, true
	m.Apply(engine.Event{AppID: "b", Kind: engine.Failed, Final: true})
	m.UntickDone(apps())
	if m.byID["a"].Checked || !m.byID["b"].Checked {
		t.Error("installed should untick, failed should stay ticked")
	}
	m.ClearLive([]*def.App{{ID: "a"}}) // a later check finds it current: dropped
	m.Apply(engine.Event{AppID: "a", Kind: engine.UpToDate, Final: true})
	if got := ids(); got != "" {
		t.Errorf("after up to date: %q", got)
	}
}

func TestSummaries(t *testing.T) {
	a := &def.App{Source: def.Source{Type: def.SourceGitLab, Project: "g/p", Tag: "v1"}, Install: def.Install{Type: def.InstallExtract, Dest: "/x"}}
	if got := sourceSummary(a); got != "gitlab: gitlab.com/g/p (tag v1)" {
		t.Errorf("got %q", got)
	}
	if got := installSummary(a); got != "extract to /x" {
		t.Errorf("got %q", got)
	}
}
