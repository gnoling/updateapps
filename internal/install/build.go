package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/sh"
)

// Build is where build installs keep things: a persistent work tree per app
// (so builds are incremental) and a log per app (steps print far more than
// the event log should carry).
type Build struct {
	SourceDir string
	LogDir    string
}

func (b Build) WorkTree(app *def.App) string { return filepath.Join(b.SourceDir, app.ID) }
func (b Build) LogPath(app *def.App) string  { return filepath.Join(b.LogDir, app.ID+".log") }

// Tail keeps the last few lines written through it, for error messages.
type Tail struct {
	mu    sync.Mutex
	lines []string
	part  string
	keep  int
}

func NewTail(keep int) *Tail { return &Tail{keep: keep} }

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part += string(p)
	for {
		line, rest, ok := strings.Cut(t.part, "\n")
		if !ok {
			break
		}
		t.part = rest
		t.lines = append(t.lines, line)
		if len(t.lines) > t.keep {
			t.lines = t.lines[1:]
		}
	}
	return len(p), nil
}

// Lines returns what was kept, including an unterminated last line.
func (t *Tail) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.lines...)
	if t.part != "" {
		out = append(out, t.part)
	}
	return out
}

// Checkout brings the work tree to commit: a clone the first time, a fetch
// after. It needs the git command; checking doesn't. Local changes to
// tracked files are discarded; untracked files (build directories) stay.
func Checkout(ctx context.Context, app *def.App, work, commit string, out io.Writer) error {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return errors.New("building from source needs the git command, which isn't installed")
	}
	src := app.Source
	git := func(dir string, args ...string) error {
		cmd := sh.Exec(ctx, gitPath, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		cmd.Stdout, cmd.Stderr = out, out
		fmt.Fprintf(out, "$ git %s\n", strings.Join(args, " "))
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
			return err
		}
		if err := git("", "clone", "--no-checkout", src.URL, work); err != nil {
			return err
		}
	} else {
		if err := git(work, "remote", "set-url", "origin", src.URL); err != nil {
			return err
		}
		fetch := []string{"fetch", "--quiet", "--prune", "origin"}
		if src.Branch != "" {
			fetch = append(fetch, "refs/heads/"+src.Branch+":refs/remotes/origin/"+src.Branch)
		}
		if err := git(work, fetch...); err != nil {
			return err
		}
	}
	if err := git(work, "checkout", "--quiet", "--force", "--detach", commit); err != nil {
		return fmt.Errorf("%w (the branch may have been rewritten since the check; try again)", err)
	}
	if src.Submodules {
		if err := git(work, "submodule", "update", "--init", "--recursive", "--quiet"); err != nil {
			return err
		}
	}
	return nil
}

// RunStep runs one build step with sh -c in the work tree. Output goes to
// out; the error carries the last lines through tail.
func RunStep(ctx context.Context, app *def.App, step, work, version string, vars def.Vars, out io.Writer) error {
	cmd := sh.Command(ctx, step)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"SRC="+work, "DEST="+app.Install.Dest, "VERSION="+version, "NAME="+app.ID,
		"APPDIR="+vars.AppDir, "APPIMAGEDIR="+vars.AppImageDir, "JOBS="+strconv.Itoa(runtime.NumCPU()))
	tail := NewTail(10)
	// One writer for both streams: exec then shares a pipe, so lines stay
	// whole and nothing writes to out concurrently.
	both := io.MultiWriter(out, tail)
	cmd.Stdout, cmd.Stderr = both, both
	fmt.Fprintf(out, "$ %s\n", step)
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return fmt.Errorf("step failed (%v): %s\n%s", err, step, strings.Join(tail.Lines(), "\n"))
	}
	return nil
}

// Artifacts copies install.artifacts (paths or globs in the work tree) into
// dest by base name, staging them under stage first so a half-copied build
// never lands. Nothing in dest is deleted.
func Artifacts(app *def.App, work, stage string, logf func(string, ...any)) error {
	in := app.Install
	out := filepath.Join(stage, "artifacts")
	if err := os.Mkdir(out, 0o755); err != nil {
		return err
	}
	n := 0
	for _, art := range in.Artifacts {
		matches, err := filepath.Glob(filepath.Join(work, filepath.FromSlash(art.From)))
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return fmt.Errorf("artifact %q: nothing matches in the work tree", art.From)
		}
		if art.To != "" && len(matches) > 1 {
			return fmt.Errorf("artifact %q: %d matches, but to: %s names one file", art.From, len(matches), art.To)
		}
		for _, m := range matches {
			target := filepath.Join(out, filepath.Base(m))
			if art.To != "" {
				target = filepath.Join(out, filepath.FromSlash(art.To))
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return err
				}
			}
			if err := copyTree(m, target); err != nil {
				return fmt.Errorf("artifact %s: %w", art.From, err)
			}
			n++
		}
	}
	logf("installed %d artifact%s into %s", n, map[bool]string{true: "", false: "s"}[n == 1], in.Dest)
	if err := os.MkdirAll(in.Dest, 0o755); err != nil {
		return err
	}
	return overlay(out, in.Dest)
}

// copyTree copies a file, symlink or directory, keeping permissions.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s: not a regular file", src)
	}
	data, err := os.Open(src)
	if err != nil {
		return err
	}
	defer data.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
