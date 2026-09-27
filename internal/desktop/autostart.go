package desktop

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Login is how a program starts with the session.
type Login struct {
	Enabled bool
	Tray    bool   // started with --hidden
	Path    string // the autostart entry; "" when there is none
}

const hiddenFlag = "--hidden"

// AutostartDir is where the session looks for entries to start at login.
func AutostartDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart"), nil
}

// autostartEntry finds the entry in dir that starts the program named like
// exe, preferring an enabled one: the user may have made theirs by hand.
func autostartEntry(dir, exe string) *Entry {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.desktop"))
	sort.Strings(paths)
	var found *Entry
	for _, p := range paths {
		e, err := Open(p)
		if err != nil {
			continue
		}
		exec, _ := e.Get("Exec")
		if w := Words(exec); len(w) == 0 || filepath.Base(w[0]) != filepath.Base(exe) {
			continue
		}
		if enabled(e) {
			return e
		}
		if found == nil {
			found = e
		}
	}
	return found
}

func enabled(e *Entry) bool {
	if v, ok := e.Get("X-GNOME-Autostart-enabled"); ok && v == "false" {
		return false
	}
	return !e.Bool("Hidden")
}

// LoginState reports whether exe starts at login, going by the entries in dir.
func LoginState(dir, exe string) Login {
	e := autostartEntry(dir, exe)
	if e == nil {
		return Login{}
	}
	exec, _ := e.Get("Exec")
	l := Login{Enabled: enabled(e), Path: e.Path}
	for _, w := range Words(exec)[1:] {
		if w == hiddenFlag || w == "-hidden" {
			l.Tray = true
		}
	}
	return l
}

// SetLogin makes exe start at login or not. An entry that's already there is
// edited, whoever wrote it; turning it off deletes only one of ours and hides
// anyone else's, as the desktop's own settings do.
func SetLogin(dir, exe, name, comment, icon string, on, tray bool) error {
	e := autostartEntry(dir, exe)
	switch {
	case e == nil && !on:
		return nil
	case e == nil:
		e = New(filepath.Join(dir, filepath.Base(exe)+".desktop"))
		e.Set("Type", "Application")
		e.Set("Name", name)
		e.Set("Comment", comment)
		e.Set("Exec", Quote(exe))
		e.Set("Icon", icon)
		e.Set("Terminal", "false")
		e.Set(ManagedKey, "true")
	case !on && e.Managed():
		return os.Remove(e.Path)
	case !on:
		e.Set("Hidden", "true")
		return e.Save()
	}
	if _, ok := e.Get("Hidden"); ok {
		e.Set("Hidden", "false")
	}
	if _, ok := e.Get("X-GNOME-Autostart-enabled"); ok {
		e.Set("X-GNOME-Autostart-enabled", "true")
	}
	exec, _ := e.Get("Exec")
	var words []string
	for _, w := range strings.Fields(exec) {
		if w != hiddenFlag && w != "-hidden" {
			words = append(words, w)
		}
	}
	if tray {
		words = append(words, hiddenFlag)
	}
	e.Set("Exec", strings.Join(words, " "))
	return e.Save()
}
