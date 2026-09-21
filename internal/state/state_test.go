package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update("a", func(e *Entry) { e.Version, e.Display = "k1", "v1" }); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get("a"); got.Version != "k1" || got.Display != "v1" {
		t.Errorf("reopened entry = %+v", got)
	}
	if got := s2.Get("missing"); got.Version != "" {
		t.Errorf("missing entry = %+v", got)
	}
}

// The bash script lost updates when jobs finished together; every one of
// these concurrent writers must survive.
func TestConcurrentUpdatesAllSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Update(fmt.Sprintf("app%d", i), func(e *Entry) { e.Version = "v" })
		}()
	}
	wg.Wait()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if s2.Get(fmt.Sprintf("app%d", i)).Version != "v" {
			t.Fatalf("app%d was lost", i)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) > 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestReadOnlyWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	s.SetReadOnly(true)
	s.Update("a", func(e *Entry) { e.Version = "v" })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("read-only store wrote %s", path)
	}
}

func TestCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := Open(path); err == nil {
		t.Error("corrupt state should not be silently treated as empty")
	}
}
