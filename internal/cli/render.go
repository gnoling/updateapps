// Package cli is the terminal front end.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/engine"
)

// Renderer prints one block per finished job, in completion order. One
// goroutine drives it, so blocks never interleave.
type Renderer struct {
	Out     io.Writer
	Verbose int
	Color   bool
	Mode    engine.Mode
	DryRun  bool
}

// Drain consumes a run's events and returns its summary.
func (r *Renderer) Drain(events <-chan engine.Event) engine.Summary {
	var sum engine.Summary
	for ev := range events {
		if ev.Kind == engine.RunFinished {
			sum = *ev.Summary
		}
		r.Handle(ev)
	}
	return sum
}

// Handle prints what one event warrants: a block for a finished job, the
// summary at the end, nothing for progress. The GUI's log pane uses it too.
func (r *Renderer) Handle(ev engine.Event) {
	switch {
	case ev.Kind == engine.RunFinished:
		r.summary(*ev.Summary)
	case ev.Kind == engine.Finalizing:
		// Shown at once: a sudo prompt may be next, and it needs context.
		fmt.Fprintln(r.Out, r.paint(bold, ev.Message))
	case ev.Final:
		r.block(ev)
	}
}

func (r *Renderer) block(ev engine.Event) {
	head := r.paint(bold, ev.Name)
	if ev.Version != "" {
		head += " (" + r.paint(green, ev.Version) + ")"
	}
	if ev.Description != "" {
		head += " — " + ev.Description
	}

	var msg string
	switch ev.Kind {
	case engine.UpToDate:
		if r.Verbose == 0 {
			return
		}
		head += " [up to date]"
	case engine.NewVersion:
		if ev.Previous != "" && r.Verbose > 0 {
			head += fmt.Sprintf(" [installed: %s]", ev.Previous)
		}
		msg = ev.Message
	case engine.Marked:
		head += " [marked current]"
		msg = ev.Message
	case engine.Installed, engine.ActionNeeded:
		if ev.Message != "" {
			msg = "ACTION NEEDED: " + ev.Message
		}
	default:
		msg = ev.Message
	}

	lines := []string{head}
	if msg != "" {
		color := yellow
		if ev.Kind == engine.Failed {
			color = red
		}
		parts := strings.Split(strings.TrimRight(msg, "\n"), "\n")
		lines[0] += " " + r.paint(color, "** "+parts[0])
		for _, p := range parts[1:] {
			lines = append(lines, "  "+p)
		}
	}
	for _, l := range ev.Log {
		if l.Level <= r.Verbose {
			lines = append(lines, "  "+r.paint(dim, l.Text))
		}
	}
	fmt.Fprintln(r.Out, strings.Join(lines, "\n"))
}

func (r *Renderer) summary(s engine.Summary) {
	elapsed := s.Elapsed.Round(time.Second)
	took := fmt.Sprintf("%d minute(s), %d second(s)", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	var line string
	if r.Mode == engine.ModeCheck {
		line = fmt.Sprintf("%d out of %d apps have updates; checked in %s", s.Updated, s.Total, took)
	} else if r.Mode == engine.ModeMarkCurrent {
		line = fmt.Sprintf("Marked %d out of %d apps as current in %s", s.Updated, s.Total, took)
		if s.Skipped > 0 {
			line += fmt.Sprintf(" (%d left unmarked)", s.Skipped)
		}
	} else if r.DryRun {
		line = fmt.Sprintf("Dry run: would update %d out of %d apps; checked in %s", s.Updated, s.Total, took)
	} else {
		line = fmt.Sprintf("Updated %d out of %d apps in %s", s.Updated, s.Total, took)
	}
	if s.Action > 0 {
		line += fmt.Sprintf(" (%d need you to finish the install)", s.Action)
	}
	if s.Failed > 0 {
		line += fmt.Sprintf(" (%d failed)", s.Failed)
	}
	if s.Cancelled {
		line += " (cancelled)"
	}
	fmt.Fprintln(r.Out, line)
}

const (
	bold   = "1"
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
)

func (r *Renderer) paint(code, s string) string {
	if !r.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// useColor: only when stdout is a terminal and NO_COLOR isn't set.
func useColor(f *os.File) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && isTerminal(f)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
