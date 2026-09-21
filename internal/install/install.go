// Package install places a download: a file at a path, or an archive extracted
// into a directory. Everything is staged, then moved into place.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gnoling/updateapps/internal/def"
)

// StageDir creates a staging directory beside dest, so the final move is a
// rename, or under fallback.
func StageDir(app *def.App, fallback string) (string, error) {
	if app.Install.Dest == "" { // install: none
		return os.MkdirTemp(fallback, app.ID+"-")
	}
	// Absolute, because post hooks get paths under it while running elsewhere.
	parent, err := filepath.Abs(filepath.Dir(filepath.Clean(app.Install.Dest)))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err == nil {
		if dir, err := os.MkdirTemp(parent, ".updateapps-stage-"+app.ID+"-"); err == nil {
			return dir, nil
		}
	}
	return os.MkdirTemp(fallback, app.ID+"-")
}

// Install installs file per app.Install, using stage as scratch space. It
// returns where the download ended up, for post hooks' $FILE.
func Install(ctx context.Context, app *def.App, file, stage string, logf func(string, ...any)) (string, error) {
	in := app.Install
	if in.Nested {
		var err error
		if file, err = Unwrap(ctx, file, filepath.Join(stage, "outer"), logf); err != nil {
			return "", err
		}
	}

	switch in.Type {
	case def.InstallNone:
		return file, nil

	case def.InstallFile:
		return in.Dest, placeFile(file, in.Dest, logf)

	case def.InstallExtractOne:
		w, err := extractTo(ctx, file, filepath.Join(stage, "extract"), extractOpts{}, logf)
		if err != nil {
			return "", err
		}
		member, err := findMember(w, in.Member, in.MemberCI)
		if err != nil {
			return "", err
		}
		logf("member %s", member[len(w.root)+1:])
		return in.Dest, placeFile(member, in.Dest, logf)

	case def.InstallExtract:
		w, err := extractTo(ctx, file, filepath.Join(stage, "extract"), extractOpts{strip: in.Strip, exclude: in.Exclude}, logf)
		if err != nil {
			return "", err
		}
		logf("extracted %d entries into %s", w.n, in.Dest)
		if err := overlay(w.root, in.Dest); err != nil {
			return "", err
		}
		return file, markExecutable(in.Dest, in.Executables, logf)
	}
	return "", fmt.Errorf("unknown install type %q", in.Type)
}

// Unwrap handles install.nested: it extracts the outer archive into dir and
// returns the file inside.
func Unwrap(ctx context.Context, file, dir string, logf func(string, ...any)) (string, error) {
	w, err := extractTo(ctx, file, dir, extractOpts{}, logf)
	if err != nil {
		return "", fmt.Errorf("outer archive: %w", err)
	}
	logf("nested archive: %s", filepath.Base(w.first))
	return w.first, nil
}

func extractTo(ctx context.Context, file, root string, opts extractOpts, logf func(string, ...any)) (*writer, error) {
	if err := os.Mkdir(root, 0o755); err != nil {
		return nil, err
	}
	w, err := extract(ctx, file, root, opts, logf)
	if err != nil {
		return nil, err
	}
	if w.n == 0 || w.first == "" {
		return nil, errors.New("archive produced no files (check install.strip / install.exclude)")
	}
	return w, nil
}

// findMember picks the extract-one member: the archive's first file, or the
// first path matching glob (the whole path, with * crossing directories, or
// the file name).
func findMember(w *writer, glob string, ci bool) (string, error) {
	if glob == "" {
		return w.first, nil
	}
	re := globRegexpCI(glob, ci)
	var found string
	var all []string
	err := filepath.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel := filepath.ToSlash(p[len(w.root)+1:])
		all = append(all, rel)
		if found == "" && (re.MatchString(rel) || re.MatchString(filepath.Base(p))) {
			found = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		if len(all) > 20 {
			all = append(all[:20], fmt.Sprintf("... and %d more", len(all)-20))
		}
		return "", fmt.Errorf("no archive member matches %q; it has:\n%s", glob, strings.Join(all, "\n"))
	}
	return found, nil
}

func placeFile(src, dest string, logf func(string, ...any)) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(src, 0o755); err != nil {
		return err
	}
	logf("install %s", dest)
	return moveFile(src, dest)
}

// moveFile renames src over dst, which is safe while dst is running. Across
// filesystems it copies, then renames within dst's directory.
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		return os.Symlink(target, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".updateapps-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// overlay moves src's contents into dst, replacing existing files and deleting
// nothing (user saves, configs, mods).
func overlay(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if info, err := os.Lstat(target); err == nil && !info.IsDir() {
				// A file (or symlink) where the new version has a directory.
				if info.Mode()&fs.ModeSymlink == 0 {
					if err := os.Remove(target); err != nil {
						return err
					}
				}
			}
			return os.MkdirAll(target, 0o755)
		}
		if info, err := os.Lstat(target); err == nil && info.IsDir() {
			return fmt.Errorf("%s is a directory but the archive has a file there", target)
		}
		return moveFile(p, target)
	})
}

func markExecutable(dest string, globs []string, logf func(string, ...any)) error {
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(dest, filepath.FromSlash(g)))
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			logf("executables: nothing matches %q", g)
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			if err := os.Chmod(m, info.Mode().Perm()|0o111); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExtractArchive safely unpacks file into dir, which must not exist.
func ExtractArchive(ctx context.Context, file, dir string, logf func(string, ...any)) error {
	_, err := extractTo(ctx, file, dir, extractOpts{}, logf)
	return err
}
