package gui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/repo"
	"github.com/gnoling/updateapps/internal/state"
)

var modeNames = map[engine.Mode]string{engine.ModeUpdate: "Update", engine.ModeCheck: "Check", engine.ModeMarkCurrent: "Mark current"}

// source is what started a run; it decides when to notify.
type source int

const (
	fromWindow source = iota
	fromTray          // always notify: the window may not be up
	fromTimer         // notify only about updates not announced before
)

// run starts an engine run for apps.
func (u *ui) run(mode engine.Mode, apps []*def.App, from source) {
	if u.running || u.cfg == nil {
		return
	}
	if len(apps) == 0 {
		if from != fromTimer {
			dialog.NewInformation("Nothing to run", "No apps are selected. Tick some, or enable them.", u.win).Show()
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.running, u.cancel = true, cancel
	u.model.ClearLive(apps)
	u.refreshList()
	u.updateStatus()
	u.renderDetails()
	ids := make([]string, len(apps))
	for i, a := range apps {
		ids[i] = a.ID
	}
	how := ""
	if from == fromTimer {
		how = " (scheduled)"
	}
	u.logf("%s %s%s: %d apps", time.Now().Format("15:04"), modeNames[mode], how, len(apps))
	cfg := u.cfg

	go func() {
		defer cancel()
		if cfg.AutoPull && mode == engine.ModeUpdate && cfg.DefsOverride == "" {
			// Best effort, as the CLI does: a failed pull mustn't block the update.
			u.pullSync(cfg, nil)
			fresh, set, st, err := u.load()
			if err == nil {
				fyne.DoAndWait(func() { u.apply(fresh, set, st) })
				cfg, apps = fresh, pick(set.Apps, ids)
			}
		}
		st, err := state.Open(cfg.State)
		if err != nil {
			fyne.Do(func() { u.finishRun(mode, from, nil, nil, err) })
			return
		}
		eng := &engine.Engine{
			Client:    fetch.New(fetch.GitHubToken(cfg.GitHubToken)),
			State:     st,
			StatePath: cfg.State,
			Vars:      cfg.Vars(),
			System: install.System{
				Privilege:   cfg.Privilege,
				GUI:         true, // pkexec can put up a dialog; nobody's at a terminal
				PendingDir:  config.PendingDir(),
				FlatpakUser: cfg.FlatpakScope == "user",
			},
		}
		events, err := eng.Run(ctx, apps, engine.Options{Mode: mode, Jobs: cfg.Jobs, Force: cfg.Force})
		if err != nil {
			if errors.Is(err, engine.ErrLocked) {
				err = errors.New("another updateapps run is in progress (the command line, perhaps); try again when it's done")
			}
			fyne.Do(func() { u.finishRun(mode, from, nil, nil, err) })
			return
		}
		r := u.renderer(0, mode, false)
		var sum engine.Summary
		// Events arrive faster than the list wants repainting: batch them.
		var pending []engine.Event
		flush := func() {
			if len(pending) == 0 {
				return
			}
			batch := pending
			pending = nil
			fyne.Do(func() { u.applyEvents(batch) })
		}
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					flush()
					fyne.Do(func() {
						u.finishRun(mode, from, apps, st, nil)
						u.notify(mode, sum, from)
					})
					return
				}
				if ev.Kind == engine.RunFinished {
					sum = *ev.Summary
					u.lastRun = summaryLine(mode, sum)
				}
				r.Handle(ev)
				pending = append(pending, ev)
			case <-tick.C:
				flush()
			}
		}
	}()
}

// applyEvents puts a batch of events on screen; UI goroutine only.
func (u *ui) applyEvents(batch []engine.Event) {
	touched := false
	for _, ev := range batch {
		u.model.Apply(ev)
		if ev.AppID == u.selected && ev.Final {
			touched = true
		}
	}
	u.refreshList() // a result can move a row into or out of the filter
	u.flushLog()
	if touched {
		u.renderDetails()
	}
}

func (u *ui) finishRun(mode engine.Mode, from source, apps []*def.App, st *state.Store, err error) {
	u.running, u.cancel = false, nil
	if st != nil {
		u.model.Refresh(st)
	}
	if mode != engine.ModeCheck && err == nil {
		u.model.UntickDone(apps)
	}
	if mode == engine.ModeCheck && err == nil {
		if n := u.model.TickUpdates(apps); n > 0 {
			u.logf("%d with updates ticked; review them, then Update checked", n)
			if from != fromTimer { // don't yank a filter the user is browsing with
				u.show.SetSelected(showUpdates)
			}
		}
	}
	if err != nil {
		u.logf("%v", err)
		dialog.NewError(err, u.win).Show()
	}
	u.refreshList()
	u.flushLog()
	u.updateStatus()
	u.renderDetails()
}

func summaryLine(mode engine.Mode, s engine.Summary) string {
	took := s.Elapsed.Round(time.Second).String()
	var line string
	switch mode {
	case engine.ModeCheck:
		line = fmt.Sprintf("%d of %d apps have updates (checked in %s)", s.Updated, s.Total, took)
	case engine.ModeMarkCurrent:
		line = fmt.Sprintf("marked %d of %d apps as current in %s", s.Updated, s.Total, took)
	default:
		line = fmt.Sprintf("updated %d of %d apps in %s", s.Updated, s.Total, took)
	}
	if s.Action > 0 {
		line += fmt.Sprintf(", %d need you to finish the install", s.Action)
	}
	if s.Failed > 0 {
		line += fmt.Sprintf(", %d failed", s.Failed)
	}
	if s.Cancelled {
		line += " (cancelled)"
	}
	return line
}

// notify sends a desktop notification when the run found or did something,
// or always when it ran from the tray. A scheduled check only speaks up for
// updates it hasn't announced yet.
func (u *ui) notify(mode engine.Mode, s engine.Summary, from source) {
	noteworthy := s.Updated > 0 || s.Failed > 0 || s.Action > 0
	if !noteworthy && (from == fromTimer || (from == fromWindow && u.shown)) {
		return
	}
	var names []string
	fresh := false
	for _, r := range u.model.Rows {
		ev := r.Live
		if ev == nil || !ev.Final {
			continue
		}
		switch ev.Kind {
		case engine.NewVersion:
			names = append(names, r.App.Name)
			if !u.announced[r.App.ID] {
				fresh = true
			}
			u.announced[r.App.ID] = true
		case engine.Installed:
			names = append(names, r.App.Name)
			delete(u.announced, r.App.ID)
		case engine.UpToDate, engine.Marked:
			delete(u.announced, r.App.ID)
		}
	}
	if from == fromTimer && !fresh && s.Failed == 0 && s.Action == 0 {
		return
	}
	title := "updateapps"
	switch {
	case mode == engine.ModeCheck && s.Updated > 0:
		title = fmt.Sprintf("%d update%s available", s.Updated, plural(s.Updated))
	case mode == engine.ModeUpdate && s.Updated > 0:
		title = fmt.Sprintf("Updated %d app%s", s.Updated, plural(s.Updated))
	case s.Failed > 0:
		title = fmt.Sprintf("%d app%s failed", s.Failed, plural(s.Failed))
	case !noteworthy:
		title = "Everything is up to date"
	}
	body := summaryLine(mode, s)
	if len(names) > 0 {
		list := names
		if len(list) > 6 {
			list = append(list[:6:6], fmt.Sprintf("and %d more", len(names)-6))
		}
		body = strings.Join(list, ", ") + "\n" + body
	}
	u.sendNotification(title, body)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// armTimer schedules the next check_interval check; the first one comes a
// minute after start, so a login autostart reports soon.
func (u *ui) armTimer() {
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	every := u.cfg.CheckEvery
	if every <= 0 || u.cfg.DefsOverride != "" {
		return
	}
	delay := every
	if !u.timerArmed {
		u.timerArmed = true
		delay = min(every, time.Minute)
	}
	u.timer = time.AfterFunc(delay, func() { fyne.Do(u.timedCheck) })
}

func (u *ui) timedCheck() {
	if u.cfg == nil {
		return
	}
	u.run(engine.ModeCheck, u.model.Runnable(), fromTimer)
	u.armTimer()
}

// pullSync fetches url: repositories (the named ones, or all) and logs what
// changed. It runs off the UI goroutine.
func (u *ui) pullSync(cfg *config.Config, names []string) (failed int) {
	client := fetch.New(fetch.GitHubToken(cfg.GitHubToken))
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, r := range repo.Repositories(cfg) {
		if r.URL == "" || (len(names) > 0 && !want[r.Name]) {
			continue
		}
		change, err := repo.Pull(context.Background(), client, cfg, r)
		switch {
		case err != nil:
			failed++
			u.logf("repository %s: %v", r.Name, err)
		case change.UpToDate || len(change.Added)+len(change.Changed)+len(change.Removed) == 0:
			if len(names) > 0 {
				u.logf("%s: up to date", r.Name)
			}
		default:
			u.logf("%s: updated%s: %d new, %d changed, %d removed", r.Name, at(change.Commit), len(change.Added), len(change.Changed), len(change.Removed))
			if len(change.NowNeedTrust) > 0 && !r.Trusted {
				u.logf("  these now need trust and won't run unless you grant it: %s", strings.Join(change.NowNeedTrust, ", "))
			}
		}
	}
	return failed
}

func at(commit string) string {
	if len(commit) < 7 {
		return ""
	}
	return " (" + commit[:7] + ")"
}

// pull is the menu action: fetch, then reload, then run then.
func (u *ui) pull(names []string, then func()) {
	cfg := u.cfg
	if cfg == nil || cfg.DefsOverride != "" {
		return
	}
	u.pullSync(cfg, names)
	u.reload(then)
}
