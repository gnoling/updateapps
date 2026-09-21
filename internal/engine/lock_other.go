//go:build !unix

package engine

import (
	"errors"
	"io/fs"
	"os"
)

// acquireLock falls back to an exclusive-create lock file. Unlike flock it can
// go stale after a crash; delete the file to recover.
func acquireLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, err
	}
	f.Close()
	return func() { os.Remove(path) }, nil
}
