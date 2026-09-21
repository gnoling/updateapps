package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/source"
	"github.com/gnoling/updateapps/internal/state"
)

// ErrLocked means another updateapps (CLI or GUI) is mid-run on this state file.
var ErrLocked = errors.New("another updateapps run is in progress")

type Mode int

const (
	ModeUpdate Mode = iota // resolve, download, install, record
	ModeCheck              // resolve and report only
	// ModeMarkCurrent records the latest version of already-installed apps
	// without downloading.
	ModeMarkCurrent
)

type Options struct {
	Mode   Mode
	Jobs   int
	Force  bool // ignore recorded versions (still records after success)
	DryRun bool // touch nothing: no downloads, no state writes
}

type Engine struct {
	Client    *fetch.Client
	State     *state.Store
	StatePath string                     // the run lock lives beside it
	Vars      def.Vars                   // APPDIR etc. for post hooks
	System    install.System             // package managers and privilege
	Resolvers map[string]source.Resolver // nil = source.NewRegistry(...)
	TempDir   string                     // parent for the run's scratch dir; "" = os.TempDir()
}

// Run starts a run and returns its events, which the caller must drain; the
// channel closes after RunFinished. Cancelling ctx drops queued jobs and
// aborts in-flight ones. Nothing partial is recorded.
func (e *Engine) Run(ctx context.Context, apps []*def.App, opts Options) (<-chan Event, error) {
	if opts.Jobs < 1 {
		opts.Jobs = 1
	}
	if e.Resolvers == nil {
		e.Resolvers = source.NewRegistry(source.Options{Vars: e.Vars, FlatpakUser: e.System.FlatpakUser})
	}
	release := func() {}
	if opts.DryRun {
		e.State.SetReadOnly(true)
	} else {
		if err := os.MkdirAll(filepath.Dir(e.StatePath), 0o755); err != nil {
			return nil, err
		}
		var err error
		if release, err = acquireLock(e.StatePath + ".lock"); err != nil {
			return nil, err
		}
	}
	tmp, err := os.MkdirTemp(e.TempDir, "updateapps-run-")
	if err != nil {
		release()
		return nil, err
	}

	events := make(chan Event, 64)
	go func() {
		defer close(events)
		defer release()
		if opts.DryRun {
			defer e.State.SetReadOnly(false)
		}
		defer os.RemoveAll(tmp)

		start := time.Now()
		sum := Summary{Total: len(apps)}
		events <- Event{Kind: RunStarted, Summary: &Summary{Total: len(apps)}}
		for _, a := range apps {
			events <- Event{Kind: Queued, AppID: a.ID, Name: a.Name, Description: a.Description}
		}

		var mu sync.Mutex // guards sum
		count := func(kind EventKind) {
			mu.Lock()
			defer mu.Unlock()
			switch kind {
			case Installed, NewVersion, Marked:
				sum.Updated++
			case UpToDate:
				sum.UpToDate++
			case Failed:
				sum.Failed++
			case Skipped:
				sum.Skipped++
			case ActionNeeded:
				sum.Action++
			}
		}
		batch := &batch{}
		var wg sync.WaitGroup
		queue := make(chan *def.App)
		for i := 0; i < opts.Jobs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for app := range queue {
					j := &job{e: e, app: app, opts: opts, tmp: tmp, events: events, batch: batch}
					count(j.run(ctx))
				}
			}()
		}
	feed:
		for _, a := range apps {
			select {
			case queue <- a:
			case <-ctx.Done():
				break feed
			}
		}
		close(queue)
		wg.Wait()
		// System packages install here: one transaction per kind, nothing else
		// running.
		batch.finalize(ctx, e, events, count)

		sum.Cancelled = ctx.Err() != nil
		sum.Elapsed = time.Since(start)
		events <- Event{Kind: RunFinished, Summary: &sum}
	}()
	return events, nil
}

// job is one app's trip through resolve -> compare -> download -> install -> record.
type job struct {
	e      *Engine
	app    *def.App
	opts   Options
	tmp    string
	events chan<- Event
	batch  *batch
	log    []LogLine
	prev   string
}

// deferred is run's result for a parked job; finalize emits its final event.
const deferred EventKind = -1

func (j *job) logf(level int) func(string, ...any) {
	return func(format string, args ...any) {
		j.log = append(j.log, LogLine{Level: level, Text: fmt.Sprintf(format, args...)})
	}
}

func (j *job) emit(ev Event) {
	ev.AppID, ev.Name, ev.Description, ev.Previous = j.app.ID, j.app.Name, j.app.Description, j.prev
	j.events <- ev
}

// finish emits the job's final event and returns its kind.
func (j *job) finish(ev Event) EventKind {
	ev.Final, ev.Log = true, j.log
	j.emit(ev)
	return ev.Kind
}

func (j *job) fail(version string, err error) EventKind {
	var soft *source.SoftError
	if errors.As(err, &soft) {
		return j.finish(Event{Kind: Skipped, Version: version, Message: soft.Msg})
	}
	if errors.Is(err, context.Canceled) {
		// Not the app's fault and nothing to retry differently: not a failure.
		return j.finish(Event{Kind: Skipped, Version: version, Message: "cancelled"})
	}
	msg := err.Error()
	j.e.State.Update(j.app.ID, func(s *state.Entry) { s.LastError = msg })
	return j.finish(Event{Kind: Failed, Version: version, Message: msg, Err: err})
}

func (j *job) run(ctx context.Context) EventKind {
	app, st := j.app, j.e.State.Get(j.app.ID)
	j.prev = st.Display
	ctx = fetch.WithLogger(ctx, func(level int, format string, args ...any) { j.logf(level)(format, args...) })

	if app.Blocked != "" {
		return j.finish(Event{Kind: Skipped, Message: "not run: " + app.Blocked})
	}
	resolver := j.e.Resolvers[app.Source.Type]
	if resolver == nil {
		return j.fail("", fmt.Errorf("source type %s: not implemented yet", app.Source.Type))
	}
	client := j.e.Client
	if app.Insecure {
		client = client.Insecure()
	}

	j.emit(Event{Kind: Checking})
	// A forced run passes no cache, so resolvers must return a URL.
	cache := &source.Cache{}
	if !j.opts.Force {
		cache.Installed, cache.ETag, cache.Version = st.Version, st.ETag, st.ETagVersion
	}
	res, err := resolver.Resolve(ctx, client, app, cache)
	if err == nil && res.NotModified && res.Version != st.Version {
		// Upstream is unchanged but that version isn't installed (a failed
		// install): resolve in full to get the URL.
		j.logf(1)("not modified upstream, but %q isn't installed; resolving in full", res.Version)
		res, err = resolver.Resolve(ctx, client, app, &source.Cache{Installed: st.Version})
	}
	if err != nil {
		return j.fail("", err)
	}
	if res.Display == "" {
		res.Display = res.Version
	}
	now := time.Now()
	j.e.State.Update(app.ID, func(s *state.Entry) {
		s.LastChecked = &now
		if res.Cache != nil {
			s.ETag, s.ETagVersion = res.Cache.ETag, res.Cache.Version
		}
	})

	if !j.opts.Force && res.Installed != "" && res.Installed == res.Version && st.Version != res.Version && j.opts.Mode == ModeUpdate && !j.opts.DryRun {
		// The system already has the latest (updated outside updateapps): just
		// record it.
		j.logf(1)("already at the latest build; recording it")
		j.e.State.Update(app.ID, func(s *state.Entry) { s.Version, s.Display, s.LastError = res.Version, res.Display, "" })
		return j.finish(Event{Kind: UpToDate, Version: res.Display})
	}
	if !j.opts.Force && res.Version == st.Version {
		if res.NotModified {
			j.logf(1)("not modified (ETag)")
		}
		return j.finish(Event{Kind: UpToDate, Version: st.Display})
	}
	j.logf(1)("recorded %q, latest %q", st.Version, res.Version)
	if res.URL != "" {
		j.logf(1)("asset %s (%s)", res.Filename, res.URL)
	}

	if j.opts.Mode == ModeCheck {
		return j.finish(Event{Kind: NewVersion, Version: res.Display})
	}
	if j.opts.Mode == ModeMarkCurrent {
		return j.markCurrent(res)
	}
	if j.opts.DryRun {
		return j.finish(Event{Kind: NewVersion, Version: res.Display,
			Message: fmt.Sprintf("dry run: would download %s -> %s", res.URL, app.Install.Dest)})
	}
	j.emit(Event{Kind: NewVersion, Version: res.Display})
	if app.Install.System() {
		return j.park(ctx, client, res)
	}

	stage, err := install.StageDir(app, j.tmp)
	if err != nil {
		return j.fail(res.Display, err)
	}
	defer os.RemoveAll(stage)

	// Keep the real file name: post hooks see it as $FILE.
	name := filepath.Base(filepath.FromSlash(res.Filename))
	if name == "." || name == string(filepath.Separator) {
		name = "download"
	}
	dlDir := filepath.Join(stage, "dl")
	if err := os.Mkdir(dlDir, 0o755); err != nil {
		return j.fail(res.Display, err)
	}
	file := filepath.Join(dlDir, name)
	if err := j.download(ctx, client, res, file); err != nil {
		return j.fail(res.Display, err)
	}

	if t := app.Install.Type; t == def.InstallExtract || t == def.InstallExtractOne || app.Install.Nested {
		j.emit(Event{Kind: Extracting, Version: res.Display})
	}
	file, err = install.Install(ctx, app, file, stage, j.logf(1))
	if err != nil {
		return j.fail(res.Display, fmt.Errorf("install failed: %w", err))
	}
	if len(app.Post) > 0 {
		j.emit(Event{Kind: PostHooks, Version: res.Display})
		env := install.HookEnv{File: file, Version: res.Display, Vars: j.e.Vars}
		if err := install.RunPost(ctx, app, env, stage, j.logf(1)); err != nil {
			// Not recorded, so the whole install (hooks included) reruns next time.
			return j.fail(res.Display, err)
		}
	}
	// A cancel landing now doesn't undo a completed install; record it.

	installed := time.Now()
	err = j.e.State.Update(app.ID, func(s *state.Entry) {
		s.Version, s.Display = res.Version, res.Display
		s.InstalledAt = &installed
		s.LastError, s.LastNotice = "", app.Notice
	})
	if err != nil {
		return j.fail(res.Display, fmt.Errorf("installed, but recording the version failed: %w", err))
	}
	if app.Notice != "" {
		j.emit(Event{Kind: Notice, Version: res.Display, Message: app.Notice})
	}
	return j.finish(Event{Kind: Installed, Version: res.Display, Message: app.Notice})
}

// download fetches res to file with progress events and digest verification.
func (j *job) download(ctx context.Context, client *fetch.Client, res *source.Result, file string) error {
	var last time.Time
	err := client.Download(ctx, res.URL, file, fetch.DownloadOpts{
		Headers: res.Headers,
		SHA256:  res.SHA256,
		Progress: func(done, total int64) {
			if t := time.Now(); t.Sub(last) > 100*time.Millisecond || done == total {
				last = t
				j.emit(Event{Kind: Downloading, Version: res.Display, Bytes: done, Total: total})
			}
		},
	})
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if res.SHA256 != "" {
		j.logf(1)("sha256 verified")
	}
	return nil
}

// markCurrent records the latest version for an app that's already installed.
func (j *job) markCurrent(res *source.Result) EventKind {
	app := j.app
	if res.Installed != "" && res.Installed != res.Version {
		return j.finish(Event{Kind: Skipped, Version: res.Display,
			Message: "not marked: the installed build isn't the latest"})
	}
	if app.Install.System() {
		if why := j.e.System.NotAdoptable(context.Background(), app, res.Version); why != "" {
			return j.finish(Event{Kind: Skipped, Version: res.Display, Message: "not marked: " + why})
		}
	} else if app.Install.Type != def.InstallNone {
		if _, err := os.Stat(app.Install.Dest); err != nil {
			return j.finish(Event{Kind: Skipped, Version: res.Display,
				Message: fmt.Sprintf("not marked: %s doesn't exist", app.Install.Dest)})
		}
	}
	if j.opts.DryRun {
		return j.finish(Event{Kind: Marked, Version: res.Display, Message: "dry run: would mark as current"})
	}
	err := j.e.State.Update(app.ID, func(s *state.Entry) {
		s.Version, s.Display, s.LastError = res.Version, res.Display, ""
	})
	if err != nil {
		return j.fail(res.Display, err)
	}
	return j.finish(Event{Kind: Marked, Version: res.Display})
}
