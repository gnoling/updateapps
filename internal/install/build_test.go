package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
)

// gitRepo makes a repository with one commit and returns its path and head.
func gitRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil || runtime.GOOS == "windows" {
		t.Skip("needs git and sh")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	gitCommit(t, dir, files)
	return dir, run("rev-parse", "HEAD")
}

func gitCommit(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestBuild(t *testing.T) {
	repo, head := gitRepo(t, map[string]string{"src/main.c": "v1", "data/a.txt": "a", "README.md": "r"})
	app := &def.App{ID: "x", Source: def.Source{Type: def.SourceGit, URL: repo, Branch: "main"},
		Install: def.Install{Type: def.InstallBuild, Dest: filepath.Join(t.TempDir(), "x"),
			Steps:     []string{`mkdir -p build && printf '%s|%s|%s|%s' "$SRC" "$DEST" "$VERSION" "$NAME" > build/x-bin && test "$JOBS" -ge 1`},
			Artifacts: []def.Artifact{{From: "build/x-bin"}, {From: "data"}, {From: "*.md"}}}}
	work := filepath.Join(t.TempDir(), "src", "x")
	var log strings.Builder
	logf := func(f string, a ...any) { log.WriteString("\n") }
	if err := Checkout(context.Background(), app, work, head, &log); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(work, "src", "main.c")); string(body) != "v1" {
		t.Fatalf("checkout: main.c = %q", body)
	}
	for _, step := range app.Install.Steps {
		if err := RunStep(context.Background(), app, step, work, "main@1234567", def.Vars{AppDir: "/apps"}, &log); err != nil {
			t.Fatal(err)
		}
	}
	stage := t.TempDir()
	if err := Artifacts(app, work, stage, logf); err != nil {
		t.Fatal(err)
	}
	dest := app.Install.Dest
	if got, _ := os.ReadFile(filepath.Join(dest, "x-bin")); string(got) != work+"|"+dest+"|main@1234567|x" {
		t.Errorf("x-bin = %q", got)
	}
	for _, f := range []string{"data/a.txt", "README.md"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("artifact %s: %v", f, err)
		}
	}

	// A second commit: the fetch path, with a stale local edit discarded and
	// the build directory kept.
	head2 := gitCommit(t, repo, map[string]string{"src/main.c": "v2"})
	os.WriteFile(filepath.Join(work, "README.md"), []byte("edited"), 0o644)
	if err := Checkout(context.Background(), app, work, head2, &log); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(work, "src", "main.c")); string(body) != "v2" {
		t.Errorf("after fetch: main.c = %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(work, "README.md")); string(body) != "r" {
		t.Errorf("local edit survived: %q", body)
	}
	if _, err := os.Stat(filepath.Join(work, "build", "x-bin")); err != nil {
		t.Error("the build directory should survive a checkout")
	}
	// A commit the remote doesn't have.
	if err := Checkout(context.Background(), app, work, strings.Repeat("0", 40), &log); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("unknown commit: %v", err)
	}

	// A failing step reports its last lines; a missing artifact fails too.
	err := RunStep(context.Background(), app, "echo one; echo 'error: no such file' >&2; exit 3", work, "v", def.Vars{}, &log)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("failing step: %v", err)
	}
	app.Install.Artifacts = []def.Artifact{{From: "build/nope"}}
	if err := Artifacts(app, work, t.TempDir(), logf); err == nil || !strings.Contains(err.Error(), "nothing matches") {
		t.Errorf("missing artifact: %v", err)
	}
	// to: renames, and refuses a glob that matches more than one file.
	app.Install.Artifacts = []def.Artifact{{From: "build/x-*", To: "bin/x"}, {From: "data/*.txt", To: "a.txt"}}
	app.Install.Dest = filepath.Join(t.TempDir(), "renamed")
	if err := Artifacts(app, work, t.TempDir(), logf); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bin/x", "a.txt"} {
		if _, err := os.Stat(filepath.Join(app.Install.Dest, f)); err != nil {
			t.Errorf("renamed artifact %s: %v", f, err)
		}
	}
	os.WriteFile(filepath.Join(work, "data", "b.txt"), []byte("b"), 0o644)
	if err := Artifacts(app, work, t.TempDir(), logf); err == nil || !strings.Contains(err.Error(), "2 matches") {
		t.Errorf("ambiguous to: %v", err)
	}
	if !strings.Contains(log.String(), "$ git clone") || !strings.Contains(log.String(), "$ git fetch") {
		t.Errorf("git commands should be logged:\n%s", log.String())
	}
}

func TestTail(t *testing.T) {
	tail := NewTail(2)
	tail.Write([]byte("a\nb\nc"))
	tail.Write([]byte("c\nd"))
	if got := strings.Join(tail.Lines(), ","); got != "b,cc,d" {
		t.Errorf("tail = %q", got)
	}
}
