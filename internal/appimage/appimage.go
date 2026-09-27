// Package appimage reads the launcher and icon an AppImage carries.
package appimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/CalebQ42/squashfs"
)

// ErrNotAppImage means the file isn't an ELF with a squashfs image after it.
// AppImages built on DwarFS are among them: nothing here reads that.
var ErrNotAppImage = errors.New("not a squashfs AppImage")

// maxFile bounds what's read into memory; icons and launchers are small.
const maxFile = 8 << 20

// Meta is what an AppImage says about itself.
type Meta struct {
	Desktop []byte // its .desktop file; nil if it has none
	Icon    []byte // nil if it has none
	IconExt string // ".png", ".svg" or ".xpm"
}

// payloadOffset is where the image starts: right after the ELF's section
// headers, as the AppImage runtime itself works it out.
func payloadOffset(f io.ReaderAt) (int64, error) {
	h := make([]byte, 64)
	if _, err := f.ReadAt(h, 0); err != nil || string(h[:4]) != "\x7fELF" {
		return 0, ErrNotAppImage
	}
	var order binary.ByteOrder = binary.LittleEndian
	if h[5] == 2 {
		order = binary.BigEndian
	}
	if h[4] == 1 { // 32-bit
		return int64(order.Uint32(h[0x20:])) + int64(order.Uint16(h[0x2e:]))*int64(order.Uint16(h[0x30:])), nil
	}
	return int64(order.Uint64(h[0x28:])) + int64(order.Uint16(h[0x3a:]))*int64(order.Uint16(h[0x3c:])), nil
}

// Read opens the AppImage at file and returns its launcher and icon.
func Read(file string) (meta *Meta, err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	off, err := payloadOffset(f)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	if _, err := f.ReadAt(magic, off); err != nil || string(magic) != "hsqs" {
		return nil, ErrNotAppImage
	}
	// The reader panics on some damaged images.
	defer func() {
		if r := recover(); r != nil {
			meta, err = nil, fmt.Errorf("%s: unreadable squashfs: %v", file, r)
		}
	}()
	r, err := squashfs.NewReaderAtOffset(f, off)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return read(r)
}

func read(root fs.FS) (*Meta, error) {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, err
	}
	m := &Meta{}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if len(m.Desktop) == 0 && strings.HasSuffix(e.Name(), ".desktop") {
			if m.Desktop, _ = readFile(root, e.Name()); len(m.Desktop) == 0 {
				// A link that pointed into the packager's build tree.
				m.Desktop, _ = readFile(root, "usr/share/applications/"+e.Name())
			}
		}
	}
	// .DirIcon is often a thumbnail, so look at what else there is: a vector
	// icon wins, then the largest bitmap.
	var candidates []string
	if icon := desktopKey(m.Desktop, "Icon"); icon != "" && !strings.Contains(icon, "/") {
		candidates = append(candidates, "usr/share/icons/hicolor/scalable/apps/"+icon+".svg", icon+".svg")
		for _, size := range []string{"512x512", "256x256", "128x128"} {
			candidates = append(candidates, "usr/share/icons/hicolor/"+size+"/apps/"+icon+".png")
		}
		candidates = append(candidates, icon+".png", icon+".xpm", icon)
	}
	candidates = append(candidates, ".DirIcon")
	for _, n := range names {
		if ext := path.Ext(n); ext == ".svg" || ext == ".png" {
			candidates = append(candidates, n)
		}
	}
	for _, c := range candidates {
		data, err := readFile(root, c)
		if err != nil {
			continue
		}
		ext := IconExt(data)
		if ext == ".svg" {
			m.Icon, m.IconExt = data, ext
			break
		}
		if ext != "" && len(data) > len(m.Icon) {
			m.Icon, m.IconExt = data, ext
		}
	}
	return m, nil
}

// readFile follows symlinks itself, relative to the image's root: an
// absolute link target means a path inside the image.
func readFile(root fs.FS, name string) ([]byte, error) {
	for hops := 0; hops < 8; hops++ {
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		if name == "" {
			return nil, fs.ErrInvalid
		}
		f, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			f.Close()
			target, err := readLink(root, name, info)
			if err != nil {
				return nil, err
			}
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(name), target)
			}
			name = target
			continue
		}
		defer f.Close()
		if info.IsDir() || info.Size() > maxFile {
			return nil, fs.ErrInvalid
		}
		return io.ReadAll(io.LimitReader(f, maxFile))
	}
	return nil, errors.New("too many symlinks")
}

func readLink(root fs.FS, name string, info fs.FileInfo) (string, error) {
	if l, ok := info.(interface{ SymlinkPath() string }); ok {
		return l.SymlinkPath(), nil
	}
	if l, ok := root.(fs.ReadLinkFS); ok {
		return l.ReadLink(name)
	}
	return "", fs.ErrInvalid
}

// IconExt is the extension for icon data of a kind desktops show, or "".
func IconExt(data []byte) string {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG")):
		return ".png"
	case bytes.Contains(head, []byte("<svg")) || bytes.HasPrefix(bytes.TrimSpace(head), []byte("<?xml")):
		return ".svg"
	case bytes.Contains(head, []byte("XPM")):
		return ".xpm"
	}
	return ""
}

// desktopKey reads one key of a launcher's [Desktop Entry] group.
func desktopKey(data []byte, key string) string {
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == "[Desktop Entry]"
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && in && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Key reads one key of the AppImage's launcher.
func (m *Meta) Key(key string) string { return desktopKey(m.Desktop, key) }
