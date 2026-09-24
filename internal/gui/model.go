// Package gui is the Fyne front end. It drives the same engine, config and
// state as the CLI; nothing here is imported by the CLI.
package gui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/state"
)

// row is one app as the table shows it: the definition, what state recorded,
// and what the last run said. Only the UI goroutine touches rows.
type row struct {
	App     *def.App
	Entry   state.Entry
	Checked bool

	// Live is the last event of the app's most recent run; Final its last
	// final event, for the details pane's log.
	Live  *engine.Event
	Final *engine.Event
	// HadUpdate: the last finished run found a new version. It's only
	// recomputed when a run ends, so a row stays in the "Updates available"
	// filter while the update it was found by is being applied.
	HadUpdate bool
	sawNew    bool // this run emitted NewVersion
}

// Status is the table's status column.
func (r *row) Status() string {
	if ev := r.Live; ev != nil {
		switch ev.Kind {
		case engine.Queued:
			return "queued"
		case engine.Checking:
			return "checking"
		case engine.NewVersion:
			return "new: " + ev.Version
		case engine.Downloading:
			if ev.Total > 0 {
				return fmt.Sprintf("downloading %d%%", ev.Bytes*100/ev.Total)
			}
			return "downloading " + humanBytes(ev.Bytes)
		case engine.Extracting:
			return "extracting"
		case engine.Pending:
			return "waiting to install"
		case engine.PostHooks:
			return "running post hooks"
		case engine.UpToDate:
			return "up to date"
		case engine.Installed:
			return "installed " + ev.Version
		case engine.Marked:
			return "marked current"
		case engine.Failed:
			return "failed: " + firstLine(ev.Message)
		case engine.Skipped:
			return "skipped: " + firstLine(ev.Message)
		case engine.ActionNeeded:
			return "action needed"
		}
	}
	a, e := r.App, r.Entry
	switch {
	case a.Blocked != "":
		return "needs trust"
	case !a.IsEnabled():
		return "disabled"
	case e.LastError != "":
		return "error: " + firstLine(e.LastError)
	case e.LastNotice != "":
		return "action needed"
	case e.Version == "":
		return "never installed"
	}
	return "ok"
}

// Version is the recorded display version.
func (r *row) Version() string {
	if r.Entry.Display == "" {
		return "-"
	}
	return r.Entry.Display
}

// Attention reports whether the row wants the user's eye: a failure, an
// available update or a pending action.
func (r *row) Attention() bool {
	if ev := r.Live; ev != nil {
		return ev.Kind == engine.Failed || ev.Kind == engine.ActionNeeded || (ev.Kind == engine.NewVersion && ev.Final)
	}
	return r.Entry.LastError != "" || r.Entry.LastNotice != ""
}

// Model is every row plus the filters that pick the visible ones.
type Model struct {
	Rows []*row // sorted by id
	byID map[string]*row

	Search   string
	Category string // "" = all
	Show     string // one of showOptions
}

const (
	showAll        = "All apps"
	showEnabled    = "Enabled"
	showDisabled   = "Disabled"
	showUpdates    = "Updates available"
	showAttention  = "Failed or action needed"
	showNeedsTrust = "Needs trust"
)

var showOptions = []string{showAll, showEnabled, showDisabled, showUpdates, showAttention, showNeedsTrust}

// Reset replaces the definitions and state, keeping check marks and the last
// run's results for ids that still exist.
func (m *Model) Reset(apps []*def.App, st *state.Store) {
	old := m.byID
	m.Rows, m.byID = nil, map[string]*row{}
	for _, a := range apps {
		r := &row{App: a}
		if st != nil {
			r.Entry = st.Get(a.ID)
		}
		if prev := old[a.ID]; prev != nil {
			r.Checked, r.Final, r.Live, r.HadUpdate, r.sawNew = prev.Checked, prev.Final, prev.Live, prev.HadUpdate, prev.sawNew
		}
		m.Rows = append(m.Rows, r)
		m.byID[a.ID] = r
	}
}

// Refresh rereads state for every row after a run.
func (m *Model) Refresh(st *state.Store) {
	for _, r := range m.Rows {
		r.Entry = st.Get(r.App.ID)
	}
}

// Apply records an event on its row.
func (m *Model) Apply(ev engine.Event) {
	r := m.byID[ev.AppID]
	if r == nil {
		return
	}
	e := ev
	r.Live = &e
	if ev.Final {
		r.Final = &e
	}
	if ev.Kind == engine.NewVersion {
		r.sawNew = true
	}
	if ev.Final {
		r.HadUpdate = r.sawNew
	}
}

// TickUpdates ticks the checked apps that have a new version and unticks
// the rest, so "Update checked" after a check is the reviewed list.
func (m *Model) TickUpdates(apps []*def.App) (n int) {
	for _, a := range apps {
		if r := m.byID[a.ID]; r != nil {
			r.Checked = r.Live != nil && r.Live.Final && r.Live.Kind == engine.NewVersion
			if r.Checked {
				n++
			}
		}
	}
	return n
}

// UntickDone clears the ticks of apps an update run finished with, leaving
// failures and pending actions ticked for a retry.
func (m *Model) UntickDone(apps []*def.App) {
	for _, a := range apps {
		r := m.byID[a.ID]
		if r == nil || r.Live == nil || !r.Live.Final {
			continue
		}
		if k := r.Live.Kind; k == engine.Installed || k == engine.UpToDate || k == engine.Marked {
			r.Checked = false
		}
	}
}

// ClearLive forgets in-progress statuses (before a new run).
func (m *Model) ClearLive(apps []*def.App) {
	for _, a := range apps {
		if r := m.byID[a.ID]; r != nil {
			r.Live, r.sawNew = nil, false
		}
	}
}

// Categories lists the ones in use, for the filter.
func (m *Model) Categories() []string {
	seen := map[string]bool{}
	for _, r := range m.Rows {
		if r.App.Category != "" {
			seen[r.App.Category] = true
		}
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Visible applies the filters. Search is the CLI's filter rule: a substring
// of the id, name or upstream.
func (m *Model) Visible() []*row {
	var out []*row
	for _, r := range m.Rows {
		if m.matches(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *Model) matches(r *row) bool {
	a := r.App
	if m.Category != "" && a.Category != m.Category {
		return false
	}
	switch m.Show {
	case showEnabled:
		if !a.IsEnabled() || a.Blocked != "" {
			return false
		}
	case showDisabled:
		if a.IsEnabled() {
			return false
		}
	case showUpdates:
		if !r.HadUpdate {
			return false
		}
	case showAttention:
		if !r.Attention() {
			return false
		}
	case showNeedsTrust:
		if a.Blocked == "" {
			return false
		}
	}
	if q := strings.ToLower(strings.TrimSpace(m.Search)); q != "" {
		hay := strings.ToLower(strings.Join([]string{a.ID, a.Name, a.Source.Repo, a.Source.URL, a.Source.Host, a.Source.Project}, "\x00"))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// Checked returns the ticked apps in table order.
func (m *Model) Checked() []*def.App {
	var out []*def.App
	for _, r := range m.Rows {
		if r.Checked {
			out = append(out, r.App)
		}
	}
	return out
}

// Runnable returns the apps a bare `updateapps` would run.
func (m *Model) Runnable() []*def.App {
	var out []*def.App
	for _, r := range m.Rows {
		if r.App.IsEnabled() && r.App.Blocked == "" {
			out = append(out, r.App)
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 80 {
		line = line[:77] + "..."
	}
	return line
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func when(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// sourceSummary is one line saying where an app comes from.
func sourceSummary(a *def.App) string {
	s := a.Source
	var where string
	switch s.Type {
	case def.SourceGitHubRelease, def.SourceGitHubActions:
		where = "github.com/" + s.Repo
	case def.SourceForgejo:
		where = strings.TrimPrefix(strings.TrimPrefix(s.Host, "https://"), "http://") + "/" + s.Repo
	case def.SourceGitLab:
		host := s.Host
		if host == "" {
			host = "gitlab.com"
		}
		where = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://") + "/" + s.Project
	case def.SourceFlatpak:
		where = s.Ref
		if where == "" {
			where = s.App + " on " + s.Remote
		}
	case def.SourceScript:
		where = "Lua"
		if s.ScriptFile != "" {
			where += " (" + s.ScriptFile + ")"
		}
	default:
		where = s.URL
	}
	out := s.Type + ": " + where
	var extra []string
	if s.Channel != "" {
		extra = append(extra, "channel "+s.Channel)
	}
	if s.Tag != "" {
		extra = append(extra, "tag "+s.Tag)
	}
	if s.Workflow != "" {
		extra = append(extra, "workflow "+s.Workflow)
	}
	if s.Branch != "" {
		extra = append(extra, "branch "+s.Branch)
	}
	if len(extra) > 0 {
		out += " (" + strings.Join(extra, ", ") + ")"
	}
	return out
}

// installSummary says where an app lands.
func installSummary(a *def.App) string {
	switch a.Install.Type {
	case def.InstallNone:
		return "nothing; the download is handed to post hooks"
	case def.InstallDeb:
		return "system package via apt (needs root)"
	case def.InstallFlatpak:
		scope := a.Install.Scope
		if scope == "" {
			scope = "default scope"
		}
		return "flatpak, " + scope
	}
	return a.Install.Type + " to " + a.Install.Dest
}
