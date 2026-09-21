package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/systest"
)

func TestReadDeb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.deb")
	os.WriteFile(path, systest.Deb("bat", "0.25.0-1", "amd64"), 0o644)
	c, err := ReadDeb(path)
	if err != nil || c.Package != "bat" || c.Version != "0.25.0-1" || c.Architecture != "amd64" {
		t.Fatalf("err=%v control=%+v", err, c)
	}
	os.WriteFile(path, []byte("<html>404</html>"), 0o644)
	if _, err := ReadDeb(path); err == nil {
		t.Error("an HTML error page isn't a .deb")
	}
}

type debFixture struct {
	sys   System
	fake  *systest.Fake
	dir   string
	logs  map[int]*strings.Builder
	items []Item
}

func newDebFixture(t *testing.T, debs ...string) *debFixture {
	f := &debFixture{fake: systest.Install(t), dir: t.TempDir(), logs: map[int]*strings.Builder{}}
	f.sys = System{Privilege: "auto", PendingDir: filepath.Join(f.dir, "pending")}
	for i, name := range debs { // "pkg_version_arch"
		parts := strings.Split(name, "_")
		file := filepath.Join(f.dir, name+".deb")
		os.WriteFile(file, systest.Deb(strings.TrimSuffix(parts[0], "-broken"), parts[1], parts[2]), 0o644)
		f.items = append(f.items, Item{File: file, App: &def.App{ID: parts[0]}})
		f.logs[i] = &strings.Builder{}
	}
	return f
}

func (f *debFixture) run() []Outcome {
	return f.sys.InstallDebs(context.Background(), f.items, func(i int) func(string, ...any) {
		return func(format string, a ...any) { fmt.Fprintf(f.logs[i], format+"\n", a...) }
	})
}

// The point of batching: however many packages, one apt transaction, so one
// password prompt and one hold on the dpkg lock.
func TestDebsInstallInOneTransaction(t *testing.T) {
	f := newDebFixture(t, "bat_0.25.0_amd64", "fd_10.2.0_amd64", "docs_1.0_all")
	for i, o := range f.run() {
		if o.Err != nil || o.ActionNeeded != "" || o.Already {
			t.Errorf("item %d: %+v", i, o)
		}
	}
	if calls := f.fake.Calls("apt-get"); len(calls) != 1 || strings.Count(calls[0], ".deb") != 3 || !strings.HasPrefix(calls[0], "install -y /") {
		t.Errorf("apt-get calls = %q", calls)
	}
	if sudo := f.fake.Calls("sudo"); len(sudo) != 1 || !strings.HasPrefix(sudo[0], "-n -- apt-get") {
		t.Errorf("not at a terminal, so sudo must be non-interactive: %q", sudo)
	}
	if f.fake.DpkgVersion("bat") != "0.25.0" || f.fake.DpkgVersion("fd") != "10.2.0" {
		t.Error("packages weren't installed")
	}
}

func TestDebAlreadyInstalledAndWrongArch(t *testing.T) {
	f := newDebFixture(t, "bat_0.25.0_amd64", "tool_1.0_arm64")
	f.fake.SetDpkg("bat", "0.25.0")
	out := f.run()
	if !out[0].Already || out[0].Err != nil {
		t.Errorf("same version already installed: %+v", out[0])
	}
	if out[1].Err == nil || !strings.Contains(out[1].Err.Error(), "built for arm64 but this system is amd64") {
		t.Errorf("wrong architecture: %+v", out[1])
	}
	if calls := f.fake.Calls("apt-get"); calls != nil {
		t.Errorf("nothing needed installing, but apt-get ran: %q", calls)
	}
}

// Headless with no NOPASSWD rule: not a failure. The download is kept and the
// user is told exactly what to run.
func TestDebWithoutRootIsActionNeeded(t *testing.T) {
	f := newDebFixture(t, "bat_0.25.0_amd64")
	f.fake.Deny()
	o := f.run()[0]
	kept := filepath.Join(f.sys.PendingDir, "bat_0.25.0_amd64.deb")
	if o.Err != nil || !strings.Contains(o.ActionNeeded, "sudo apt-get install "+kept) {
		t.Fatalf("%+v", o)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("download wasn't kept: %v", err)
	}
	if f.fake.DpkgVersion("bat") != "" {
		t.Error("installed without root?")
	}

	f.sys.Privilege = "none"
	f.items[0].File = kept
	if o := f.run()[0]; o.ActionNeeded == "" {
		t.Errorf("privilege: none should also end in action-needed: %+v", o)
	}
}

// One bad package fails the whole transaction; the rest still go in and the
// culprit is named.
func TestDebBatchFailureIsolatesTheCulprit(t *testing.T) {
	f := newDebFixture(t, "bat_0.25.0_amd64", "evil-broken_1.0_amd64", "fd_10.2.0_amd64")
	out := f.run()
	if out[0].Err != nil || out[2].Err != nil {
		t.Errorf("good packages should still install: %v / %v", out[0].Err, out[2].Err)
	}
	if out[1].Err == nil || !strings.Contains(out[1].Err.Error(), "returned an error code") {
		t.Errorf("the broken one: %+v", out[1])
	}
	if calls := f.fake.Calls("apt-get"); len(calls) != 4 {
		t.Errorf("want 1 batch + 3 single transactions, got %q", calls)
	}
	if f.fake.DpkgVersion("bat") != "0.25.0" || f.fake.DpkgVersion("fd") != "10.2.0" || f.fake.DpkgVersion("evil") != "" {
		t.Error("wrong set of packages installed")
	}
}

func TestPrivilegeModes(t *testing.T) {
	systest.Install(t)
	for name, tc := range map[string]struct {
		sys  System
		want string
	}{
		"terminal":   {System{Privilege: "auto", Interactive: true}, "sudo -p"},
		"headless":   {System{Privilege: "auto"}, "sudo -n --"},
		"gui":        {System{Privilege: "auto", GUI: true}, "pkexec apt-get"},
		"forced":     {System{Privilege: "pkexec", Interactive: true}, "pkexec apt-get"},
		"sudo + gui": {System{Privilege: "sudo", GUI: true}, "sudo -n --"},
	} {
		cmd, err := tc.sys.asRoot(context.Background(), "apt-get", "install")
		if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), tc.want) {
			t.Errorf("%s: err=%v args=%q, want %q", name, err, cmd.Args, tc.want)
		}
	}
	if _, err := (System{Privilege: "none"}).asRoot(context.Background(), "apt-get"); err != ErrNoPrivilege {
		t.Errorf("none: %v", err)
	}
}

const refFile = "[Flatpak Ref]\nName=org.DolphinEmu.dolphin-emu\nBranch=beta\nUrl=https://flatpak.dolphin-emu.org/dev\nSuggestRemoteName=dolphin-dev\nIsRuntime=false\n"

func flatpakItem(t *testing.T, src def.Source, in def.Install, content string) Item {
	it := Item{App: &def.App{ID: "app", Source: src, Install: in}}
	if content != "" {
		it.File = filepath.Join(t.TempDir(), "dev.flatpakref")
		os.WriteFile(it.File, []byte(content), 0o644)
	}
	return it
}

func runFlatpak(sys System, it Item) Outcome {
	return sys.InstallFlatpaks(context.Background(), []Item{it}, func(int) func(string, ...any) { return func(string, ...any) {} })[0]
}

func TestFlatpakRefInstallsThenUpdates(t *testing.T) {
	fake := systest.Install(t)
	sys := System{FlatpakUser: true}
	const ref = "org.DolphinEmu.dolphin-emu//beta"
	fake.SetRemote(ref, "aaaa1111aaaa1111", "2606-318")

	it := flatpakItem(t, def.Source{Type: def.SourceFlatpak, Ref: "https://x/dev.flatpakref"}, def.Install{Type: def.InstallFlatpak}, refFile)
	o := runFlatpak(sys, it)
	if o.Err != nil || o.Version != "aaaa1111aaaa1111" || o.Display != "2606-318 (aaaa1111aa)" {
		t.Fatalf("first install: %+v", o)
	}
	if calls := fake.Calls("flatpak"); !strings.Contains(strings.Join(calls, "\n"), "install --user -y --noninteractive --from") {
		t.Errorf("first time should install from the ref file: %q", calls)
	}

	// Later the remote has a new build: same ref file, but now it's an update.
	fake.SetRemote(ref, "bbbb2222bbbb2222", "2606-400")
	o = runFlatpak(sys, it)
	if o.Err != nil || o.Version != "bbbb2222bbbb2222" || fake.FlatpakCommit(ref) != "bbbb2222bbbb2222" {
		t.Fatalf("update: %+v", o)
	}
	if calls := fake.Calls("flatpak"); !strings.Contains(strings.Join(calls, "\n"), "update --user -y --noninteractive "+ref) {
		t.Errorf("an installed app should be updated, not reinstalled: %q", calls)
	}
}

func TestFlatpakRemoteAppBundleAndScope(t *testing.T) {
	fake := systest.Install(t)
	fake.SetRemote("org.libretro.RetroArch//stable", "cccc3333cccc3333", "1.22.2")

	// An app on an already-configured remote: nothing is downloaded at all.
	it := flatpakItem(t, def.Source{Type: def.SourceFlatpak, Remote: "flathub", App: "org.libretro.RetroArch"}, def.Install{Type: def.InstallFlatpak, Scope: "system"}, "")
	if o := runFlatpak(System{FlatpakUser: true}, it); o.Err != nil || o.Version != "cccc3333cccc3333" {
		t.Fatalf("remote app: %+v", o)
	}
	if calls := strings.Join(fake.Calls("flatpak"), "\n"); !strings.Contains(calls, "install --system -y --noninteractive flathub org.libretro.RetroArch//stable") {
		t.Errorf("install.scope should override the default: %q", calls)
	}

	// flatpak elevates through polkit by itself; headless, that's refused.
	fake.Deny()
	fake.SetRemote("org.other.App//stable", "dddd", "1")
	it = flatpakItem(t, def.Source{Type: def.SourceFlatpak, Remote: "flathub", App: "org.other.App"}, def.Install{Type: def.InstallFlatpak, Scope: "system"}, "")
	if o := runFlatpak(System{}, it); o.Err != nil || !strings.Contains(o.ActionNeeded, "flatpak update --system org.other.App//stable") {
		t.Errorf("unauthorized system install should be action-needed: %+v", o)
	}
	fake.Allow()

	// A .flatpak bundle from a release: the release is the version.
	it = flatpakItem(t, def.Source{Type: def.SourceGitHubRelease}, def.Install{Type: def.InstallFlatpak}, "\x00binary bundle")
	if o := runFlatpak(System{FlatpakUser: true}, it); o.Err != nil || o.Version != "" {
		t.Errorf("bundle: %+v", o)
	}
	if calls := strings.Join(fake.Calls("flatpak"), "\n"); !strings.Contains(calls, "--reinstall --bundle") {
		t.Errorf("bundle install: %q", calls)
	}
}
