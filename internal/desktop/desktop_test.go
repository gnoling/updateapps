package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const handMade = `[Desktop Entry]
Encoding=UTF-8
Type=Application
Name=UpdateApps
Exec=/home/u/.local/bin/updateapps-gui
OnlyShowIn=XFCE;
Hidden=false

[Desktop Action x]
Exec=other
`

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWords(t *testing.T) {
	got := Words(`"/opt/my app/run" --flag "a \"b\"" c`)
	want := []string{"/opt/my app/run", "--flag", `a "b"`, "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Words = %q, want %q", got, want)
	}
	if q := Quote("/opt/my app/run"); !reflect.DeepEqual(Words(q), []string{"/opt/my app/run"}) {
		t.Errorf("Quote doesn't round-trip: %s", q)
	}
}

func TestLoginEditsAHandMadeEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "UpdateApps.desktop")
	if err := os.WriteFile(path, []byte(handMade), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := "/elsewhere/bin/updateapps-gui"
	if l := LoginState(dir, exe); !l.Enabled || l.Tray || l.Path != path {
		t.Fatalf("state = %+v", l)
	}
	if err := SetLogin(dir, exe, "n", "c", "i", true, true); err != nil {
		t.Fatal(err)
	}
	want := `[Desktop Entry]
Encoding=UTF-8
Type=Application
Name=UpdateApps
Exec=/home/u/.local/bin/updateapps-gui --hidden
OnlyShowIn=XFCE;
Hidden=false

[Desktop Action x]
Exec=other
`
	if got := read(t, path); got != want {
		t.Errorf("after tray on:\n%s", got)
	}
	if l := LoginState(dir, exe); !l.Enabled || !l.Tray {
		t.Errorf("state = %+v", l)
	}
	if err := SetLogin(dir, exe, "n", "c", "i", false, false); err != nil {
		t.Fatal(err)
	}
	if e, err := Open(path); err != nil || !e.Bool("Hidden") {
		t.Errorf("someone else's entry should be hidden, not deleted: %v", err)
	}
	if l := LoginState(dir, exe); l.Enabled {
		t.Errorf("state = %+v", l)
	}
	if err := SetLogin(dir, exe, "n", "c", "i", true, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != handMade {
		t.Errorf("back on without the tray:\n%s", got)
	}
}

func TestLoginWritesAndRemovesItsOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "autostart")
	exe := "/home/u/my apps/updateapps-gui"
	if err := SetLogin(dir, exe, "updateapps", "Keeps apps current", "updateapps", true, true); err != nil {
		t.Fatal(err)
	}
	l := LoginState(dir, exe)
	if !l.Enabled || !l.Tray || filepath.Base(l.Path) != "updateapps-gui.desktop" {
		t.Fatalf("state = %+v\n%s", l, read(t, l.Path))
	}
	if e, _ := Open(l.Path); !e.Managed() {
		t.Error("not marked as managed")
	}
	if err := SetLogin(dir, exe, "", "", "", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Path); !os.IsNotExist(err) {
		t.Error("our own entry should be deleted")
	}
}
