package install

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
)

// fakeTools puts stand-in pkg-config and dpkg-query scripts first on PATH:
// modules and packages named in have are present, the rest aren't.
func fakeTools(t *testing.T, have ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	bin := t.TempDir()
	known := strings.Join(have, " ")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg-config", `for k in `+known+`; do [ "$2" = "$k" ] && exit 0; done; exit 1`)
	write("dpkg-query", `for k in `+known+`; do [ "$3" = "$k" ] && { echo "install ok installed"; exit 0; }; done; echo "unknown ok not-installed"; exit 1`)
	write("have-cmd", "exit 0")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMissing(t *testing.T) {
	fakeTools(t, "sdl2", "libsdl2-dev")
	app := &def.App{ID: "x", Install: def.Install{Type: def.InstallBuild}}
	if got := Missing(context.Background(), app); got != "" {
		t.Errorf("no needs: %q", got)
	}
	app.Install.Needs = def.Needs{Commands: []string{"have-cmd", "sh"}, PkgConfig: []string{"sdl2"}, Apt: []string{"libsdl2-dev"}}
	if got := Missing(context.Background(), app); got != "" {
		t.Errorf("all present: %q", got)
	}
	app.Install.Needs = def.Needs{Commands: []string{"have-cmd", "no-such-tool-xyz"}, PkgConfig: []string{"sdl2", "gtkmm-3.0"}, Apt: []string{"libsdl2-dev", "libgtkmm-3.0-dev"}}
	got := Missing(context.Background(), app)
	for _, want := range []string{"commands: no-such-tool-xyz", "pkg-config modules: gtkmm-3.0", "packages: libgtkmm-3.0-dev", "sudo apt-get install libgtkmm-3.0-dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "libsdl2-dev") || strings.Contains(got, "have-cmd") {
		t.Errorf("present things reported:\n%s", got)
	}
}
