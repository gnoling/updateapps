package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
)

const svg = `<svg xmlns="http://www.w3.org/2000/svg"/>`

func put(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func dirs(t *testing.T) Dirs {
	root := t.TempDir()
	return Dirs{Applications: filepath.Join(root, "applications"), Icons: filepath.Join(root, "icons")}
}

func extracted(t *testing.T) *def.App {
	dest := filepath.Join(t.TempDir(), "gear boy")
	put(t, filepath.Join(dest, "gearboy"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(dest, "Gearboy.svg"), svg, 0o644)
	return &def.App{
		ID: "gearboy", Name: "Gearboy", Description: "Game Boy\nemulator", Category: "emulators",
		Install: def.Install{Type: def.InstallExtract, Dest: dest, Executables: []string{"gearboy"}},
		Desktop: def.Desktop{Args: "%f"},
	}
}

func TestWriteExtracted(t *testing.T) {
	d, app := dirs(t), extracted(t)
	res, err := d.Write(context.Background(), app)
	if err != nil || res.Outcome != Written || res.Note != "" {
		t.Fatalf("%+v, %v", res, err)
	}
	dest := app.Install.Dest
	want := `[Desktop Entry]
Type=Application
Name=Gearboy
Comment=Game Boy emulator
Exec="` + dest + `/gearboy" %f
Path=` + dest + `
Icon=` + d.Icons + `/gearboy.svg
Terminal=false
Categories=Game;Emulator;
X-Updateapps-Managed=true
X-Updateapps-Id=gearboy
`
	if got := read(t, res.Path); got != want {
		t.Errorf("launcher:\n%s", got)
	}
	if got := read(t, filepath.Join(d.Icons, "gearboy.svg")); got != svg {
		t.Errorf("icon = %q", got)
	}
	if ids := d.Managed(); len(ids) != 1 || ids[0] != "gearboy" {
		t.Errorf("managed = %v", ids)
	}
	if found := d.Find(app); found.Outcome != Written {
		t.Errorf("find = %+v", found)
	}
	if ok, err := d.Remove("gearboy"); !ok || err != nil {
		t.Fatalf("remove: %v %v", ok, err)
	}
	for _, p := range []string{res.Path, filepath.Join(d.Icons, "gearboy.svg")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there", p)
		}
	}
}

func TestWriteAppImage(t *testing.T) {
	d := dirs(t)
	img, err := os.ReadFile("../appimage/testdata/tiny.AppImage")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "tiny.AppImage")
	put(t, dest, string(img), 0o755)
	app := &def.App{ID: "tiny", Name: "Tiny", Category: "tools", Install: def.Install{Type: def.InstallFile, Dest: dest}}
	res, err := d.Write(context.Background(), app)
	if err != nil || res.Outcome != Written {
		t.Fatalf("%+v, %v", res, err)
	}
	got := read(t, res.Path)
	for _, line := range []string{"Exec=" + dest, "Icon=" + d.Icons + "/tiny.png", "Categories=Game;Emulator;", "StartupWMClass=tiny-wm"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("no %q in:\n%s", line, got)
		}
	}
	if strings.Contains(got, "Path=") {
		t.Errorf("an AppImage needs no working directory:\n%s", got)
	}
}

func TestTheirLaunchersAreLeftAlone(t *testing.T) {
	d, app := dirs(t), extracted(t)
	// Their launcher starts another program in the app's folder, through env.
	theirs := filepath.Join(d.Applications, "My Gearboy.desktop")
	body := "[Desktop Entry]\nName=mine\nExec=env X=1 \"" + app.Install.Dest + "/gearboy-sdl\" %f\n"
	put(t, theirs, body, 0o644)
	res, err := d.Write(context.Background(), app)
	if err != nil || res.Outcome != Kept || res.Path != theirs {
		t.Fatalf("%+v, %v", res, err)
	}
	if _, err := os.Stat(d.Path(app.ID)); !os.IsNotExist(err) {
		t.Error("wrote a second launcher")
	}
	os.Remove(theirs)

	// A single file in a folder named for the app, started through a script.
	one := &def.App{ID: "crash", Install: def.Install{Type: def.InstallFile, Dest: filepath.Join(app.Install.Dest, "Crash", "game.bin")}}
	put(t, one.Install.Dest, "x", 0o755)
	put(t, theirs, "[Desktop Entry]\nExec="+Quote(filepath.Dir(one.Install.Dest)+"/launch")+"\n", 0o644)
	if res, err := d.Write(context.Background(), one); err != nil || res.Outcome != Kept {
		t.Fatalf("%+v, %v", res, err)
	}
	os.Remove(theirs)

	// One of ours they took over by removing the marker.
	put(t, d.Path(app.ID), "[Desktop Entry]\nName=edited\nExec=/elsewhere\n", 0o644)
	if res, err := d.Write(context.Background(), app); err != nil || res.Outcome != Kept {
		t.Fatalf("%+v, %v", res, err)
	}
	if ok, _ := d.Remove(app.ID); ok {
		t.Error("removed a launcher that isn't ours")
	}
	if got := read(t, d.Path(app.ID)); !strings.Contains(got, "Name=edited") {
		t.Errorf("overwritten:\n%s", got)
	}
}

func TestNoLauncher(t *testing.T) {
	d, app := dirs(t), extracted(t)
	for name, change := range map[string]func(a *def.App){
		"off":           func(a *def.App) { a.Desktop.Off = true },
		"no program":    func(a *def.App) { a.Install.Executables = nil },
		"not installed": func(a *def.App) { a.Install.Dest = filepath.Join(t.TempDir(), "missing") },
		"deb":           func(a *def.App) { a.Install = def.Install{Type: def.InstallDeb} },
	} {
		a := *app
		change(&a)
		if res, err := d.Write(context.Background(), &a); err != nil || res.Outcome != None || res.Note == "" {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
	}
	d.DryRun = true
	if res, err := d.Write(context.Background(), app); err != nil || res.Outcome != Written {
		t.Errorf("dry run: %+v, %v", res, err)
	}
	if _, err := os.Stat(d.Applications); !os.IsNotExist(err) {
		t.Error("the dry run wrote something")
	}
}

func TestIconFromDefinition(t *testing.T) {
	d, app := dirs(t), extracted(t)
	put(t, filepath.Join(app.Install.Dest, "share", "big.svg"), svg+"<!-- big -->", 0o644)
	app.Desktop.Icon = "share/big.svg"
	if _, err := d.Write(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(d.Icons, "gearboy.svg")); !strings.Contains(got, "big") {
		t.Errorf("icon = %q", got)
	}

	app.Desktop.Icon = "https://x.example/icon"
	d.Download = func(_ context.Context, url, file string) error {
		return os.WriteFile(file, []byte("\x89PNG\r\n\x1a\nfrom "+url), 0o644)
	}
	res, err := d.Write(context.Background(), app)
	if err != nil || res.Note != "" {
		t.Fatalf("%+v, %v", res, err)
	}
	if got := read(t, filepath.Join(d.Icons, "gearboy.png")); !strings.Contains(got, "x.example") {
		t.Errorf("icon = %q", got)
	}
	if _, err := os.Stat(filepath.Join(d.Icons, "gearboy.svg")); !os.IsNotExist(err) {
		t.Error("the old icon is still there")
	}

	d.Download = func(_ context.Context, _, file string) error { return os.WriteFile(file, []byte("<html>"), 0o644) }
	res, err = d.Write(context.Background(), app)
	if err != nil || res.Outcome != Written || !strings.Contains(res.Note, "icon") {
		t.Fatalf("a bad icon should only be noted: %+v, %v", res, err)
	}
	if got := read(t, res.Path); strings.Contains(got, "Icon=") {
		t.Errorf("launcher names an icon that wasn't written:\n%s", got)
	}
}
