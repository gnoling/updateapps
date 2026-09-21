package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/gnoling/updateapps/internal/def"
)

type member struct {
	name, body string
	mode       int64
	typ        byte   // tar typeflag; 0 = regular
	link       string // symlink / hard link target
}

// pkg is the same tree the bzip2 fixture holds: pkg/README and an executable pkg/bin/run.
var pkg = []member{
	{name: "pkg/", typ: tar.TypeDir, mode: 0o755},
	{name: "pkg/README", body: "hello\n", mode: 0o644},
	{name: "pkg/bin/run", body: "#!/bin/sh\n", mode: 0o755},
}

func tarBytes(t *testing.T, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Mode: m.mode, Size: int64(len(m.body)), Typeflag: m.typ, Linkname: m.link, Uid: 0, Uname: "root"}
		if m.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, m.body)
	}
	tw.Close()
	return buf.Bytes()
}

func zipBytes(t *testing.T, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		mode := fs.FileMode(m.mode)
		if m.typ == tar.TypeSymlink {
			mode |= fs.ModeSymlink
			m.body = m.link
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, m.body)
	}
	zw.Close()
	return buf.Bytes()
}

func compress(t *testing.T, kind string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch kind {
	case "gz":
		w = gzip.NewWriter(&buf)
	case "xz":
		w, err = xz.NewWriter(&buf)
	case "zst":
		w, err = zstd.NewWriter(&buf)
	}
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// installArchive runs a full extract install of data into a fresh dest.
func installArchive(t *testing.T, data []byte, in def.Install) (dest string, logs string, err error) {
	t.Helper()
	base := t.TempDir()
	if in.Dest == "" {
		in.Dest = filepath.Join(base, "dest")
	}
	in.Type = def.InstallExtract
	app := &def.App{ID: "pkg", Install: in}
	stage, serr := StageDir(app, base)
	if serr != nil {
		t.Fatal(serr)
	}
	defer os.RemoveAll(stage)
	// Deliberately misleading name: detection must use content.
	file := filepath.Join(stage, "download.bin")
	os.WriteFile(file, data, 0o644)
	var log strings.Builder
	_, err = Install(context.Background(), app, file, stage, func(f string, a ...any) { log.WriteString(fmt.Sprintf(f, a...) + "\n") })
	return in.Dest, log.String(), err
}

func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestFormatsDetectedByContent(t *testing.T) {
	bz2, err := os.ReadFile("testdata/pkg.tar.bz2")
	if err != nil {
		t.Fatal(err)
	}
	raw := tarBytes(t, pkg)
	for name, data := range map[string][]byte{
		"tar": raw, "tar.gz": compress(t, "gz", raw), "tar.xz": compress(t, "xz", raw),
		"tar.zst": compress(t, "zst", raw), "tar.bz2": bz2, "zip": zipBytes(t, pkg),
	} {
		t.Run(name, func(t *testing.T) {
			dest, _, err := installArchive(t, data, def.Install{Strip: 1})
			if err != nil {
				t.Fatal(err)
			}
			if body, _ := os.ReadFile(filepath.Join(dest, "README")); string(body) != "hello\n" {
				t.Errorf("README = %q", body)
			}
			// Executable bits survive; nothing else about the archive's modes does.
			if m := mode(t, filepath.Join(dest, "bin/run")); m != 0o755 {
				t.Errorf("bin/run mode = %o", m)
			}
			if m := mode(t, filepath.Join(dest, "README")); m != 0o644 {
				t.Errorf("README mode = %o", m)
			}
		})
	}
}

func TestUnknownFormat(t *testing.T) {
	if _, _, err := installArchive(t, []byte("<html>502 Bad Gateway</html>"), def.Install{}); err == nil {
		t.Error("an HTML error page must not install")
	}
}

func TestStripDropsShallowEntries(t *testing.T) {
	data := tarBytes(t, append([]member{{name: "toplevel.txt", body: "x", mode: 0o644}}, pkg...))
	dest, _, err := installArchive(t, data, def.Install{Strip: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "toplevel.txt")); err == nil {
		t.Error("entry shallower than strip should be dropped")
	}
	if _, err := os.Stat(filepath.Join(dest, "bin/run")); err != nil {
		t.Error(err)
	}
	if _, _, err := installArchive(t, data, def.Install{Strip: 5}); err == nil {
		t.Error("stripping everything away should be an error, not an empty install")
	}
}

// Archives made with `tar -C dir .` name members "./x". tar and bsdtar count
// that "./" when stripping; RSDKv4's real release depends on it.
func TestStripCountsLeadingDotLikeTar(t *testing.T) {
	data := tarBytes(t, []member{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./RSDKv4", body: "bin", mode: 0o755},
		{name: "./data/./x", body: "x", mode: 0o644},
	})
	for strip, want := range map[int][]string{0: {"RSDKv4", "data/x"}, 1: {"RSDKv4", "data/x"}, 2: {"x"}} {
		dest, _, err := installArchive(t, data, def.Install{Strip: strip})
		if err != nil {
			t.Fatalf("strip %d: %v", strip, err)
		}
		for _, f := range want {
			if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
				t.Errorf("strip %d: %v", strip, err)
			}
		}
	}
}

// exclude protects user settings: '*.json' must match at any depth, like
// bsdtar --exclude, and an existing file must be left alone.
func TestExcludeProtectsExistingFiles(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	os.MkdirAll(filepath.Join(dest, "cfg"), 0o755)
	os.WriteFile(filepath.Join(dest, "cfg/settings.json"), []byte("mine"), 0o644)

	data := tarBytes(t, []member{
		{name: "app/cfg/settings.json", body: "upstream default", mode: 0o644},
		{name: "app/top.json", body: "upstream default", mode: 0o644},
		{name: "app/game", body: "bin", mode: 0o755},
	})
	if _, _, err := installArchive(t, data, def.Install{Dest: dest, Strip: 1, Exclude: []string{"*.json"}}); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "cfg/settings.json")); string(body) != "mine" {
		t.Errorf("settings.json = %q", body)
	}
	if _, err := os.Stat(filepath.Join(dest, "top.json")); err == nil {
		t.Error("top.json should have been excluded")
	}
}

func TestOverlayKeepsUserFilesAndReplacesOld(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	os.MkdirAll(filepath.Join(dest, "saves"), 0o755)
	os.WriteFile(filepath.Join(dest, "saves/slot1"), []byte("progress"), 0o644)
	os.WriteFile(filepath.Join(dest, "README"), []byte("old"), 0o644)

	if _, _, err := installArchive(t, tarBytes(t, pkg), def.Install{Dest: dest, Strip: 1}); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "saves/slot1")); string(body) != "progress" {
		t.Error("user file was removed")
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "README")); string(body) != "hello\n" {
		t.Errorf("README not replaced: %q", body)
	}
	if left, _ := filepath.Glob(filepath.Join(base, ".updateapps-*")); len(left) > 1 {
		t.Errorf("staging left behind: %v", left)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	for name, members := range map[string][]member{
		"dotdot":         {{name: "../evil", body: "x", mode: 0o644}},
		"nested dotdot":  {{name: "a/../../evil", body: "x", mode: 0o644}},
		"absolute":       {{name: "/tmp/evil", body: "x", mode: 0o644}},
		"windows drive":  {{name: `C:\evil`, body: "x", mode: 0o644}},
		"backslash dots": {{name: `..\evil`, body: "x", mode: 0o644}},
	} {
		for kind, data := range map[string][]byte{"tar": tarBytes(t, members), "zip": zipBytes(t, members)} {
			dest, _, err := installArchive(t, data, def.Install{})
			if err == nil {
				t.Errorf("%s/%s: expected rejection", name, kind)
			}
			if _, serr := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); serr == nil {
				t.Errorf("%s/%s: file escaped the destination", name, kind)
			}
			// A rejected archive installs nothing at all.
			if _, serr := os.Stat(dest); serr == nil {
				t.Errorf("%s/%s: dest was created despite rejection", name, kind)
			}
		}
	}
}

func TestSymlinks(t *testing.T) {
	members := []member{
		{name: "lib/libfoo.so.1", body: "elf", mode: 0o644},
		{name: "lib/libfoo.so", typ: tar.TypeSymlink, link: "libfoo.so.1", mode: 0o777},
		{name: "etc", typ: tar.TypeSymlink, link: "/etc", mode: 0o777},
		{name: "up", typ: tar.TypeSymlink, link: "../../..", mode: 0o777},
	}
	for kind, data := range map[string][]byte{"tar": tarBytes(t, members), "zip": zipBytes(t, members)} {
		dest, logs, err := installArchive(t, data, def.Install{})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if target, _ := os.Readlink(filepath.Join(dest, "lib/libfoo.so")); target != "libfoo.so.1" {
			t.Errorf("%s: internal symlink = %q", kind, target)
		}
		for _, bad := range []string{"etc", "up"} {
			if _, err := os.Lstat(filepath.Join(dest, bad)); err == nil {
				t.Errorf("%s: escaping symlink %q was created", kind, bad)
			}
		}
		if !strings.Contains(logs, "skipped symlink") {
			t.Errorf("%s: skip wasn't logged", kind)
		}
	}
}

// The classic attack: plant a link, then write a later member through it.
func TestWriteThroughSymlinkBlocked(t *testing.T) {
	data := tarBytes(t, []member{
		{name: "real/keep", body: "k", mode: 0o644},
		{name: "alias", typ: tar.TypeSymlink, link: "real", mode: 0o777},
		{name: "alias/injected", body: "x", mode: 0o644},
	})
	if _, _, err := installArchive(t, data, def.Install{}); err == nil || !strings.Contains(err.Error(), "through symlink") {
		t.Errorf("err = %v", err)
	}
}

func TestExecutablesGlob(t *testing.T) {
	data := zipBytes(t, []member{ // zips made on Windows carry no exec bits
		{name: "game.x86_64", body: "bin", mode: 0o644},
		{name: "bin/helper", body: "bin", mode: 0o644},
		{name: "data.pak", body: "d", mode: 0o644},
	})
	dest, logs, err := installArchive(t, data, def.Install{Executables: []string{"*.x86_64", "bin/*", "not-there"}})
	if err != nil {
		t.Fatal(err)
	}
	if mode(t, filepath.Join(dest, "game.x86_64"))&0o111 == 0 || mode(t, filepath.Join(dest, "bin/helper"))&0o111 == 0 {
		t.Error("executables not marked")
	}
	if mode(t, filepath.Join(dest, "data.pak"))&0o111 != 0 {
		t.Error("data.pak should not be executable")
	}
	if !strings.Contains(logs, "nothing matches") {
		t.Error("a glob matching nothing should be logged, not fatal")
	}
}

func TestSetuidAndOwnershipDropped(t *testing.T) {
	data := tarBytes(t, []member{{name: "suid", body: "x", mode: 0o4755}})
	dest, _, err := installArchive(t, data, def.Install{})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dest, "suid"))
	if info.Mode()&fs.ModeSetuid != 0 || info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode())
	}
}

func TestFileInstallReplacesAndMarksExecutable(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "appimages", "Tool.AppImage")
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.WriteFile(dest, []byte("old"), 0o644)
	held, _ := os.Open(dest) // stands in for a running AppImage
	defer held.Close()

	app := &def.App{ID: "tool", Install: def.Install{Type: def.InstallFile, Dest: dest}}
	stage, err := StageDir(app, base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(stage) != filepath.Dir(dest) {
		t.Errorf("stage %s isn't beside dest", stage)
	}
	file := filepath.Join(stage, "download")
	os.WriteFile(file, []byte("new"), 0o600)
	if _, err := Install(context.Background(), app, file, stage, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(dest); string(body) != "new" || mode(t, dest) != 0o755 {
		t.Errorf("dest = %q mode %o", body, mode(t, dest))
	}
	if old, _ := io.ReadAll(held); string(old) != "old" {
		t.Errorf("the open handle should still see the old file, got %q", old)
	}
}

// run installs data with an arbitrary install block and returns $FILE.
func run(t *testing.T, data []byte, in def.Install) (file string, err error) {
	t.Helper()
	base := t.TempDir()
	app := &def.App{ID: "pkg", Install: in}
	stage, serr := os.MkdirTemp(base, "stage-")
	if serr != nil {
		t.Fatal(serr)
	}
	dl := filepath.Join(stage, "asset.bin")
	os.WriteFile(dl, data, 0o644)
	return Install(context.Background(), app, dl, stage, func(string, ...any) {})
}

func Test7z(t *testing.T) {
	data, err := os.ReadFile("testdata/pkg.7z")
	if err != nil {
		t.Fatal(err)
	}
	dest, _, err := installArchive(t, data, def.Install{Strip: 1})
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "README.txt")); string(body) != "readme\n" {
		t.Errorf("README.txt = %q", body)
	}
}

// The LADXHD case: a 7z whose binary sits in a folder under a name that drifts
// between releases ("Patcher-Lite" / "Patcher.Lite").
func TestExtractOneByCaseInsensitiveGlob(t *testing.T) {
	data, _ := os.ReadFile("testdata/pkg.7z")
	dest := filepath.Join(t.TempDir(), "ladxhd", "Patcher-Lite")
	file, err := run(t, data, def.Install{Type: def.InstallExtractOne, Member: "*patcher*lite*", MemberCI: true, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(dest); string(body) != "patcher-binary\n" || mode(t, dest) != 0o755 || file != dest {
		t.Errorf("dest=%q mode=%o file=%s", body, mode(t, dest), file)
	}
	// Case-sensitive by default.
	if _, err := run(t, data, def.Install{Type: def.InstallExtractOne, Member: "*patcher*lite*", Dest: dest}); err == nil || !strings.Contains(err.Error(), "LADXHD/README.txt") {
		t.Errorf("want a no-match error listing members, got %v", err)
	}
}

func TestExtractOneMemberForms(t *testing.T) {
	members := []member{
		{name: "docs/README", body: "r", mode: 0o644},
		{name: "bin/86Box", body: "emulator", mode: 0o644},
		{name: "Cemu-2.6-x86_64.AppImage", body: "cemu", mode: 0o644},
	}
	for name, tc := range map[string]struct{ member, want string }{
		"default is the first file in archive order": {"", "r"},
		"exact path":                {"bin/86Box", "emulator"},
		"glob on the file name":     {"Cemu-*.AppImage", "cemu"},
		"glob crossing directories": {"*86Box", "emulator"},
	} {
		for kind, data := range map[string][]byte{"tar.gz": compress(t, "gz", tarBytes(t, members)), "zip": zipBytes(t, members)} {
			dest := filepath.Join(t.TempDir(), "out")
			if _, err := run(t, data, def.Install{Type: def.InstallExtractOne, Member: tc.member, Dest: dest}); err != nil {
				t.Errorf("%s/%s: %v", name, kind, err)
				continue
			}
			if body, _ := os.ReadFile(dest); string(body) != tc.want || mode(t, dest)&0o111 == 0 {
				t.Errorf("%s/%s: got %q mode %o", name, kind, body, mode(t, dest))
			}
		}
	}
}

// Recomp projects ship a tarball inside a zip (bash gh_dir_nested); strip
// applies to the inner archive.
func TestNested(t *testing.T) {
	inner := compress(t, "gz", tarBytes(t, pkg))
	outer := zipBytes(t, []member{{name: "pkg-linux.tar.gz", body: string(inner), mode: 0o644}})

	dest := filepath.Join(t.TempDir(), "dest")
	if _, err := run(t, outer, def.Install{Type: def.InstallExtract, Nested: true, Strip: 1, Dest: dest}); err != nil {
		t.Fatal(err)
	}
	if mode(t, filepath.Join(dest, "bin/run")) != 0o755 {
		t.Error("inner archive wasn't extracted with its modes")
	}

	one := filepath.Join(t.TempDir(), "run")
	if _, err := run(t, outer, def.Install{Type: def.InstallExtractOne, Nested: true, Member: "*/bin/run", Dest: one}); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(one); string(body) != "#!/bin/sh\n" {
		t.Errorf("nested extract-one = %q", body)
	}
}

func TestInstallNoneKeepsTheDownloadForHooks(t *testing.T) {
	file, err := run(t, []byte("jar bytes"), def.Install{Type: def.InstallNone})
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(file); string(body) != "jar bytes" || filepath.Base(file) != "asset.bin" {
		t.Errorf("file=%s body=%q", file, body)
	}
}

func TestPostHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "app", "tool")
	os.MkdirAll(filepath.Dir(dest), 0o755)
	app := &def.App{ID: "tool", Install: def.Install{Type: def.InstallFile, Dest: dest}, Post: []string{
		`printf '%s|%s|%s|%s|%s|%s' "$FILE" "$DEST" "$VERSION" "$NAME" "$APPDIR" "$APPIMAGEDIR" > env.txt`,
		`pwd > cwd.txt; echo hook output`,
	}}
	var log strings.Builder
	logf := func(f string, a ...any) { log.WriteString(fmt.Sprintf(f, a...) + "\n") }
	env := HookEnv{File: dest, Version: "v1.2", Vars: def.Vars{AppDir: "/apps", AppImageDir: "/apps/ai"}}
	if err := RunPost(context.Background(), app, env, dir, logf); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app", "env.txt")); string(got) != dest+"|"+dest+"|v1.2|tool|/apps|/apps/ai" {
		t.Errorf("hook env = %q", got)
	}
	if !strings.Contains(log.String(), "hook output") {
		t.Errorf("hook output should reach the job log: %q", log.String())
	}

	// First failure stops the chain, and its output is in the error.
	app.Post = []string{"echo 'scp: connection refused' >&2; exit 7", "touch should-not-run"}
	err := RunPost(context.Background(), app, env, dir, logf)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("err = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "app", "should-not-run")); serr == nil {
		t.Error("hooks after a failure must not run")
	}

	old := HookTimeout
	HookTimeout = 50 * time.Millisecond
	defer func() { HookTimeout = old }()
	app.Post = []string{"sleep 5"}
	if err := RunPost(context.Background(), app, env, dir, logf); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("hung hook: %v", err)
	}
}
