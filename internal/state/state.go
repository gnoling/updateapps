// Package state is the version store: one JSON file, one writer, atomic saves.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is what's recorded per app id.
type Entry struct {
	// Version is the opaque key compared against the resolved version;
	// Display is what people see (e.g. the tag).
	Version     string     `json:"version,omitempty"`
	Display     string     `json:"display,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastNotice  string     `json:"last_notice,omitempty"`

	// Conditional-request cache: the ETag of the source's last response and
	// the version that response resolved to.
	ETag        string `json:"etag,omitempty"`
	ETagVersion string `json:"etag_version,omitempty"`
}

type file struct {
	Apps map[string]*Entry `json:"apps"`
}

// Store serializes all access; every Update is saved before it returns.
type Store struct {
	mu       sync.Mutex
	path     string
	apps     map[string]*Entry
	readOnly bool
}

// Open loads path; a missing file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, apps: map[string]*Entry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.New(path + ": " + err.Error())
	}
	if f.Apps != nil {
		s.apps = f.Apps
	}
	return s, nil
}

// SetReadOnly makes Update change memory only (used by --dry-run).
func (s *Store) SetReadOnly(ro bool) {
	s.mu.Lock()
	s.readOnly = ro
	s.mu.Unlock()
}

// Get returns a copy of the entry for id (zero value if absent).
func (s *Store) Get(id string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.apps[id]; e != nil {
		return *e
	}
	return Entry{}
}

// Update applies fn to id's entry and saves the file.
func (s *Store) Update(id string, fn func(*Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.apps[id]
	if e == nil {
		e = &Entry{}
		s.apps[id] = e
	}
	fn(e)
	if s.readOnly {
		return nil
	}
	return s.save()
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(file{Apps: s.apps}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
