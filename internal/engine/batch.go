package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/source"
	"github.com/gnoling/updateapps/internal/state"
)

// batch holds jobs whose install goes through a system package manager, parked
// after download.
type batch struct {
	mu    sync.Mutex
	items []*parked
}

type parked struct {
	job  *job
	res  *source.Result
	file string // "" when there's nothing to download (a flatpak on a remote)
}

// park downloads what the job needs and queues it for finalize.
func (j *job) park(ctx context.Context, client *fetch.Client, res *source.Result) EventKind {
	p := &parked{job: j, res: res}
	if res.URL != "" {
		name := filepath.Base(filepath.FromSlash(res.Filename))
		if name == "." || name == string(filepath.Separator) {
			name = "download"
		}
		// A download left from a run that couldn't get root is still good.
		if kept := filepath.Join(j.e.System.PendingDir, name); j.e.System.PendingDir != "" && sameDownload(kept, res) {
			j.logf(1)("using the copy already downloaded to %s", kept)
			p.file = kept
		} else {
			dir := filepath.Join(j.tmp, "batch", j.app.ID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return j.fail(res.Display, err)
			}
			p.file = filepath.Join(dir, name)
			if err := j.download(ctx, client, res, p.file); err != nil {
				return j.fail(res.Display, err)
			}
		}
		if j.app.Install.Nested {
			inner, err := install.Unwrap(ctx, p.file, filepath.Join(j.tmp, "batch", j.app.ID+"-outer"), j.logf(1))
			if err != nil {
				return j.fail(res.Display, fmt.Errorf("install failed: %w", err))
			}
			p.file = inner
		}
	}
	j.batch.mu.Lock()
	j.batch.items = append(j.batch.items, p)
	j.batch.mu.Unlock()
	j.emit(Event{Kind: Pending, Version: res.Display})
	return deferred
}

// sameDownload reports whether path already holds what res points at.
func sameDownload(path string, res *source.Result) bool {
	info, err := os.Stat(path)
	return err == nil && res.Size > 0 && info.Size() == res.Size
}

// finalize installs the parked jobs, one transaction per kind, and emits their
// final events.
func (b *batch) finalize(ctx context.Context, e *Engine, events chan<- Event, count func(EventKind)) {
	if len(b.items) == 0 {
		return
	}
	kinds := []struct {
		typ, label string
		run        func(context.Context, []install.Item, func(int) func(string, ...any)) []install.Outcome
	}{
		{def.InstallDeb, "system package", e.System.InstallDebs},
		{def.InstallFlatpak, "flatpak", e.System.InstallFlatpaks},
	}
	for _, k := range kinds {
		var group []*parked
		for _, p := range b.items {
			if p.job.app.Install.Type == k.typ {
				group = append(group, p)
			}
		}
		if len(group) == 0 {
			continue
		}
		if ctx.Err() != nil {
			for _, p := range group {
				count(p.job.fail(p.res.Display, ctx.Err()))
			}
			continue
		}
		names := make([]string, len(group))
		items := make([]install.Item, len(group))
		for i, p := range group {
			names[i] = p.job.app.Name
			items[i] = install.Item{App: p.job.app, File: p.file}
		}
		plural := "s"
		if len(group) == 1 {
			plural = ""
		}
		events <- Event{Kind: Finalizing, Message: fmt.Sprintf("Installing %d %s%s: %s", len(group), k.label, plural, strings.Join(names, ", "))}

		outcomes := k.run(ctx, items, func(i int) func(string, ...any) { return group[i].job.logf(1) })
		for i, p := range group {
			count(p.job.settle(ctx, p, outcomes[i]))
		}
	}
}

// settle emits a parked job's final event. On success it runs post hooks and
// records state.
func (j *job) settle(ctx context.Context, p *parked, o install.Outcome) EventKind {
	res, app := p.res, j.app
	switch {
	case o.Err != nil:
		return j.fail(res.Display, fmt.Errorf("install failed: %w", o.Err))
	case o.ActionNeeded != "":
		// Nothing recorded: the next run finds the kept download and tries again.
		return j.finish(Event{Kind: ActionNeeded, Version: res.Display, Message: o.ActionNeeded})
	}
	if o.Version != "" {
		res.Version, res.Display = o.Version, o.Display
	}
	if len(app.Post) > 0 && !o.Already {
		j.emit(Event{Kind: PostHooks, Version: res.Display})
		env := install.HookEnv{File: p.file, Version: res.Display, Vars: j.e.Vars}
		if err := install.RunPost(ctx, app, env, j.tmp, j.logf(1)); err != nil {
			return j.fail(res.Display, err)
		}
	}
	if dir := j.e.System.PendingDir; dir != "" && p.file != "" && filepath.Dir(p.file) == dir {
		os.Remove(p.file) // a kept download that finally went in
	}
	now := time.Now()
	err := j.e.State.Update(app.ID, func(s *state.Entry) {
		s.Version, s.Display = res.Version, res.Display
		s.InstalledAt = &now
		s.LastError, s.LastNotice = "", app.Notice
	})
	if err != nil {
		return j.fail(res.Display, fmt.Errorf("installed, but recording the version failed: %w", err))
	}
	if o.Already {
		j.logf(1)("already at this version; recorded without reinstalling")
		return j.finish(Event{Kind: UpToDate, Version: res.Display})
	}
	return j.finish(Event{Kind: Installed, Version: res.Display, Message: app.Notice})
}
