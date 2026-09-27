// Package desktop reads and writes freedesktop .desktop files.
package desktop

import (
	"os"
	"path/filepath"
	"strings"
)

// ManagedKey marks a file this program wrote and may replace or delete.
const ManagedKey = "X-Updateapps-Managed"

const mainGroup = "[Desktop Entry]"

// Entry is a .desktop file kept as lines, so an edit changes only the keys
// it sets: these files are often hand-made.
type Entry struct {
	Path  string
	lines []string
}

// New is an empty entry to fill with Set.
func New(path string) *Entry {
	return &Entry{Path: path, lines: []string{mainGroup}}
}

func Open(path string) (*Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &Entry{Path: path, lines: strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")}, nil
}

// group returns the line range of [Desktop Entry]'s keys; start is -1 when
// the file has no such group.
func (e *Entry) group() (start, end int) {
	start = -1
	for i, l := range e.lines {
		l = strings.TrimSpace(l)
		switch {
		case start < 0 && l == mainGroup:
			start = i + 1
		case start >= 0 && strings.HasPrefix(l, "["):
			return start, i
		}
	}
	return start, len(e.lines)
}

func (e *Entry) find(key string) int {
	start, end := e.group()
	for i := start; start >= 0 && i < end; i++ {
		if k, _, ok := strings.Cut(e.lines[i], "="); ok && strings.TrimSpace(k) == key {
			return i
		}
	}
	return -1
}

// Get returns a key of the main group.
func (e *Entry) Get(key string) (string, bool) {
	i := e.find(key)
	if i < 0 {
		return "", false
	}
	_, v, _ := strings.Cut(e.lines[i], "=")
	return strings.TrimSpace(v), true
}

// Bool is a key's value as a boolean; false when unset.
func (e *Entry) Bool(key string) bool {
	v, _ := e.Get(key)
	return v == "true"
}

// Managed reports whether this program wrote the file.
func (e *Entry) Managed() bool { return e.Bool(ManagedKey) }

// Set replaces a key of the main group or adds it after the group's last key.
func (e *Entry) Set(key, value string) {
	line := key + "=" + value
	if i := e.find(key); i >= 0 {
		e.lines[i] = line
		return
	}
	start, end := e.group()
	if start < 0 {
		e.lines = append([]string{mainGroup, line}, e.lines...)
		return
	}
	for end > start && strings.TrimSpace(e.lines[end-1]) == "" {
		end--
	}
	e.lines = append(e.lines[:end], append([]string{line}, e.lines[end:]...)...)
}

// Save writes the file through a rename.
func (e *Entry) Save() error {
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
		return err
	}
	tmp := e.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(e.lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.Path)
}

// Quote makes s one argument of an Exec line.
func Quote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\><~|&;$*?#()`%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// Words splits an Exec line into arguments.
func Words(exec string) []string {
	var out []string
	var cur strings.Builder
	quoted, has := false, false
	for i := 0; i < len(exec); i++ {
		c := exec[i]
		switch {
		case quoted && c == '\\' && i+1 < len(exec):
			i++
			cur.WriteByte(exec[i])
		case c == '"':
			quoted, has = !quoted, true
		case !quoted && (c == ' ' || c == '\t'):
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}
