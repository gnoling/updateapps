package appimage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// testdata/tiny.AppImage is a 64-byte ELF header followed by a squashfs of:
// AppRun, tiny.desktop (a link out of the image, as some packagers leave it),
// usr/share/applications/tiny.desktop, a 2x2 tiny.png with .DirIcon linking
// to it, and a 16x16 tiny.png under usr/share/icons/hicolor/256x256/apps.
func TestRead(t *testing.T) {
	m, err := Read("testdata/tiny.AppImage")
	if err != nil {
		t.Fatal(err)
	}
	if m.Key("Name") != "Tiny" || m.Key("Categories") != "Game;Emulator;" || m.Key("Icon") != "tiny" {
		t.Errorf("launcher = %q", m.Desktop)
	}
	big, err := os.ReadFile("testdata/tiny.AppImage")
	if err != nil {
		t.Fatal(err)
	}
	if m.IconExt != ".png" || len(m.Icon) < 500 || len(m.Icon) > len(big) {
		t.Errorf("icon: %d bytes, %q; want the larger bitmap", len(m.Icon), m.IconExt)
	}
}

func TestNotAppImage(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"script": "#!/bin/sh\n",
		"dwarfs": "\x7fELF\x02\x01\x01" + string(make([]byte, 0x21)) + "\x40" + string(make([]byte, 23)) + "DWARFS",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(data), 0o755)
		if _, err := Read(p); !errors.Is(err, ErrNotAppImage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
