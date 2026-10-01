package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/source"
)

// build is a job handed to the build lane after its source resolved.
type build struct {
	job *job
	res *source.Result
}

// build checks out the resolved commit, runs the steps and installs the
// artifacts. Everything the tools print goes to the app's log file; the
// event log gets the step list and, on failure, the last lines.
func (j *job) build(ctx context.Context, res *source.Result) EventKind {
	app, in := j.app, j.app.Install
	if ctx.Err() != nil {
		return j.fail(res.Display, ctx.Err())
	}
	if missing := install.Missing(ctx, app); missing != "" {
		// Nothing recorded: the next run checks again.
		return j.finish(Event{Kind: ActionNeeded, Version: res.Display, Message: missing})
	}
	work := j.e.Build.WorkTree(app)
	logPath := j.e.Build.LogPath(app)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return j.fail(res.Display, err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return j.fail(res.Display, err)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "== %s %s (%s) in %s\n", app.ID, res.Display, time.Now().Format(time.RFC3339), work)
	j.logf(1)("work tree %s; full output in %s", work, logPath)

	timeout := in.BuildTimeout()
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A timeout is the build's fault; a cancel from outside isn't.
	wrap := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("build timed out after %s (install.timeout); log: %s", timeout, logPath)
		}
		return err
	}

	start := time.Now()
	j.emit(Event{Kind: Building, Version: res.Display, Message: "fetching source"})
	tail := install.NewTail(10)
	if err := install.Checkout(bctx, app, work, res.Version, io.MultiWriter(logFile, tail)); err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w\n%s\nlog: %s", err, strings.Join(tail.Lines(), "\n"), logPath)
		}
		return j.fail(res.Display, wrap(err))
	}
	j.logf(1)("checked out %s (%s)", res.Display, time.Since(start).Round(time.Second))

	for i, step := range in.Steps {
		j.emit(Event{Kind: Building, Version: res.Display, Message: fmt.Sprintf("step %d/%d: %s", i+1, len(in.Steps), step)})
		began := time.Now()
		if err := install.RunStep(bctx, app, step, work, res.Display, j.e.Vars, logFile); err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("%w\nlog: %s", err, logPath)
			}
			return j.fail(res.Display, wrap(err))
		}
		j.logf(1)("step %d/%d done in %s: %s", i+1, len(in.Steps), time.Since(began).Round(time.Second), step)
	}

	j.emit(Event{Kind: Building, Version: res.Display, Message: "installing artifacts"})
	stage, err := install.StageDir(app, j.tmp)
	if err != nil {
		return j.fail(res.Display, err)
	}
	defer os.RemoveAll(stage)
	if err := install.Artifacts(app, work, stage, j.logf(1)); err != nil {
		return j.fail(res.Display, fmt.Errorf("install failed: %w", err))
	}
	if len(app.Post) > 0 {
		j.emit(Event{Kind: PostHooks, Version: res.Display})
		env := install.HookEnv{File: work, Version: res.Display, Vars: j.e.Vars}
		if err := install.RunPost(ctx, app, env, stage, j.logf(1)); err != nil {
			return j.fail(res.Display, err)
		}
	}
	j.logf(1)("built in %s", time.Since(start).Round(time.Second))
	return j.record(ctx, res)
}
