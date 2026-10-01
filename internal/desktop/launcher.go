package desktop

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gnoling/updateapps/internal/appimage"
	"github.com/gnoling/updateapps/internal/def"
)

// IDKey names the definition a managed launcher belongs to.
const IDKey = "X-Updateapps-Id"

const maxIcon = 8 << 20

// Dirs is where launchers and their icons go.
type Dirs struct {
	Applications string
	Icons        string
	// Download fetches a desktop.icon URL; nil leaves such an icon out.
	Download func(ctx context.Context, url, file string) error
	// DryRun reports what Write and Remove would do.
	DryRun bool
}

// DefaultDirs follows XDG_DATA_HOME.
func DefaultDirs() (Dirs, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dirs{}, err
		}
		data = filepath.Join(home, ".local", "share")
	}
	return Dirs{
		Applications: filepath.Join(data, "applications"),
		Icons:        filepath.Join(data, "updateapps", "icons"),
	}, nil
}

type Outcome int

const (
	// None: the app gets no launcher; Note says why.
	None Outcome = iota
	// Kept: the user has a launcher of their own for it, at Path.
	Kept
	Written
)

type Result struct {
	Outcome Outcome
	Path    string
	Note    string
}

func (r Result) String() string {
	switch r.Outcome {
	case Written:
		return "launcher written: " + r.Path
	case Kept:
		return "launcher left alone, it's yours: " + r.Path
	}
	return "no launcher: " + r.Note
}

// Path is the launcher this program writes for an app.
func (d Dirs) Path(id string) string {
	return filepath.Join(d.Applications, "updateapps-"+id+".desktop")
}

// Find reports the launcher an app has, without changing anything: the
// user's own (Kept), one of ours (Written), or none.
func (d Dirs) Find(app *def.App) Result {
	if theirs := d.theirs(app); theirs != "" {
		return Result{Outcome: Kept, Path: theirs}
	}
	if e, err := Open(d.Path(app.ID)); err == nil {
		if e.Managed() {
			return Result{Outcome: Written, Path: e.Path}
		}
		return Result{Outcome: Kept, Path: e.Path}
	}
	return Result{}
}

// Write creates or refreshes the app's launcher and icon. It never touches a
// launcher it didn't write, and writes none where the user has their own.
func (d Dirs) Write(ctx context.Context, app *def.App) (Result, error) {
	if app.Desktop.Off {
		return Result{Note: "the definition asks for none"}, nil
	}
	prog := app.Program()
	switch {
	case app.Install.System() || app.Install.Type == def.InstallNone:
		return Result{Note: "install type " + app.Install.Type + " brings its own or has nothing to start"}, nil
	case prog == "":
		return Result{Note: "the definition doesn't name the program (desktop.exec)"}, nil
	}
	if _, err := os.Stat(prog); err != nil {
		return Result{Note: prog + " isn't installed"}, nil
	}
	if found := d.Find(app); found.Outcome == Kept {
		return found, nil
	}

	var meta *appimage.Meta
	if app.Install.Type != def.InstallExtract {
		m, err := appimage.Read(prog)
		if err != nil && !errors.Is(err, appimage.ErrNotAppImage) {
			return Result{}, err
		}
		meta = m
	}
	if meta == nil {
		meta = &appimage.Meta{}
	}
	res := Result{Outcome: Written, Path: d.Path(app.ID)}
	if d.DryRun {
		return res, nil
	}
	icon, err := d.icon(ctx, app, meta)
	if err != nil {
		// A launcher without its icon still starts the app.
		res.Note = "icon: " + err.Error()
	}

	e := New(res.Path)
	e.Set("Type", "Application")
	e.Set("Name", oneLine(app.Name))
	if app.Description != "" {
		e.Set("Comment", oneLine(app.Description))
	}
	exec := Quote(prog)
	if app.Desktop.Args != "" {
		exec += " " + app.Desktop.Args
	}
	e.Set("Exec", exec)
	if app.Install.Type == def.InstallExtract {
		e.Set("Path", filepath.Dir(prog))
	}
	if icon != "" {
		e.Set("Icon", icon)
	}
	terminal := meta.Key("Terminal") == "true"
	if app.Desktop.Terminal != nil {
		terminal = *app.Desktop.Terminal
	}
	e.Set("Terminal", fmt.Sprint(terminal))
	e.Set("Categories", categories(app, meta))
	if class := first(app.Desktop.WMClass, meta.Key("StartupWMClass")); class != "" {
		e.Set("StartupWMClass", oneLine(class))
	}
	e.Set(ManagedKey, "true")
	e.Set(IDKey, app.ID)
	return res, e.Save()
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func categories(app *def.App, meta *appimage.Meta) string {
	c := first(app.Desktop.Categories, meta.Key("Categories"))
	if c == "" {
		switch app.Category {
		case "emulators":
			c = "Game;Emulator"
		case "games", "ports":
			c = "Game"
		default:
			c = "Utility"
		}
	}
	return strings.TrimSuffix(oneLine(c), ";") + ";"
}

// icon puts the app's icon in d.Icons and returns its path; "" with no error
// means the app has none to offer.
func (d Dirs) icon(ctx context.Context, app *def.App, meta *appimage.Meta) (string, error) {
	var data []byte
	var err error
	switch want := app.Desktop.Icon; {
	case strings.HasPrefix(want, "https://"):
		if d.Download == nil {
			return "", nil
		}
		tmp := filepath.Join(d.Icons, app.ID+".download")
		if err := os.MkdirAll(d.Icons, 0o755); err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		if err := d.Download(ctx, want, tmp); err != nil {
			return "", err
		}
		data, err = readIcon(tmp)
	case want != "":
		data, err = readIcon(filepath.Join(app.Install.Dest, filepath.FromSlash(want)))
	case meta.Icon != nil:
		data = meta.Icon
	case app.Install.Type == def.InstallExtract:
		data = findIcon(app)
	}
	if err != nil || data == nil {
		return "", err
	}
	ext := appimage.IconExt(data)
	if ext == "" {
		return "", errors.New("not a PNG, SVG or XPM image")
	}
	if err := os.MkdirAll(d.Icons, 0o755); err != nil {
		return "", err
	}
	d.removeIcons(app.ID)
	file := filepath.Join(d.Icons, app.ID+ext)
	return file, os.WriteFile(file, data, 0o644)
}

func readIcon(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxIcon {
		return nil, fmt.Errorf("%s: not a file of a sensible size", file)
	}
	return os.ReadFile(file)
}

// findIcon looks for an icon among an extracted app's top-level files.
func findIcon(app *def.App) []byte {
	prog := filepath.Base(app.Program())
	stems := []string{app.ID, strings.TrimSuffix(prog, filepath.Ext(prog)), app.Name, "icon", "logo"}
	entries, _ := os.ReadDir(app.Install.Dest)
	for _, ext := range []string{".svg", ".png"} {
		for _, stem := range stems {
			for _, e := range entries {
				if strings.EqualFold(e.Name(), stem+ext) {
					if data, err := readIcon(filepath.Join(app.Install.Dest, e.Name())); err == nil {
						return data
					}
				}
			}
		}
	}
	return nil
}

func (d Dirs) removeIcons(id string) {
	for _, ext := range []string{".png", ".svg", ".xpm"} {
		os.Remove(filepath.Join(d.Icons, id+ext))
	}
}

// theirs finds a launcher the user made for the app: one that starts its
// program or anything else in its folder (a wrapper script, another binary).
// A single file has a folder of its own only when it's named for the app.
func (d Dirs) theirs(app *def.App) string {
	prog := app.Program()
	folder := ""
	switch dest := filepath.Clean(app.Install.Dest); {
	case app.Install.Type == def.InstallExtract:
		folder = dest + string(filepath.Separator)
	case app.Install.Type == def.InstallBuild:
		// A build into a shared directory (the AppImage dir) owns no folder.
		if strings.EqualFold(filepath.Base(dest), app.ID) {
			folder = dest + string(filepath.Separator)
		}
	case app.Install.Dest != "" && strings.EqualFold(filepath.Base(filepath.Dir(dest)), app.ID):
		folder = filepath.Dir(dest) + string(filepath.Separator)
	}
	if prog == "" && folder == "" {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(d.Applications, "*.desktop"))
	sort.Strings(paths)
	for _, p := range paths {
		e, err := Open(p)
		if err != nil || e.Managed() {
			continue
		}
		exec, _ := e.Get("Exec")
		try, _ := e.Get("TryExec")
		for _, w := range append(Words(exec), try) {
			if !filepath.IsAbs(w) {
				continue
			}
			w = filepath.Clean(w)
			if w == prog || (folder != "" && strings.HasPrefix(w, folder)) {
				return p
			}
		}
	}
	return ""
}

// Remove deletes the launcher and icon written for id. A launcher that isn't
// marked as ours stays.
func (d Dirs) Remove(id string) (bool, error) {
	e, err := Open(d.Path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !e.Managed() {
		return false, nil
	}
	if d.DryRun {
		return true, nil
	}
	d.removeIcons(id)
	return true, os.Remove(e.Path)
}

// Managed lists the ids of the launchers this program wrote.
func (d Dirs) Managed() []string {
	paths, _ := filepath.Glob(filepath.Join(d.Applications, "updateapps-*.desktop"))
	var ids []string
	for _, p := range paths {
		if e, err := Open(p); err == nil && e.Managed() {
			if id, _ := e.Get(IDKey); id != "" && d.Path(id) == p {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}
