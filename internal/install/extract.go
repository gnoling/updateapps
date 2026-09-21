package install

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

type extractOpts struct {
	strip   int
	exclude []string
}

// extract unpacks file into root, an empty staging directory. The format comes
// from the content, never the name.
func extract(ctx context.Context, file, root string, opts extractOpts, logf func(string, ...any)) (*writer, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	head, _ := br.Peek(8)

	w := &writer{ctx: ctx, root: root, opts: opts, logf: logf}
	for _, g := range opts.exclude {
		w.exclude = append(w.exclude, globRegexp(g))
	}

	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return nil, err
		}
		return w, w.zip(zr)
	case bytes.HasPrefix(head, []byte("7z\xbc\xaf\x27\x1c")):
		zr, err := sevenzip.OpenReader(file)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return w, w.sevenzip(&zr.Reader)
	}

	r, closeR, err := decompress(br)
	if err != nil {
		return nil, err
	}
	defer closeR()
	tbr := bufio.NewReaderSize(r, 1<<20)
	if block, _ := tbr.Peek(512); len(block) < 512 || !bytes.HasPrefix(block[257:], []byte("ustar")) {
		return nil, errors.New("unrecognized archive format (not zip, 7z, or a tar stream)")
	}
	return w, w.tar(tar.NewReader(tbr))
}

// decompress unwraps gzip, bzip2, xz or zstd by magic bytes; anything else
// passes through.
func decompress(br *bufio.Reader) (io.Reader, func(), error) {
	head, _ := br.Peek(6)
	switch {
	case bytes.HasPrefix(head, []byte("\x1f\x8b")):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return gz, func() { gz.Close() }, nil
	case bytes.HasPrefix(head, []byte("BZh")):
		return bzip2.NewReader(br), func() {}, nil
	case bytes.HasPrefix(head, []byte("\xfd7zXZ\x00")):
		r, err := xz.NewReader(br)
		return r, func() {}, err
	case bytes.HasPrefix(head, []byte("\x28\xb5\x2f\xfd")):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return zr, zr.Close, nil
	}
	return br, func() {}, nil
}

// writer places archive members under root, applying strip and exclude, never
// outside root.
type writer struct {
	ctx     context.Context
	root    string
	opts    extractOpts
	exclude []*regexp.Regexp
	logf    func(string, ...any)
	n       int
	first   string // first regular file written, in archive order
}

var errSkip = errors.New("skip member")

// target maps a member name to its path under root: errSkip if stripped or
// excluded, an error if it would escape.
func (w *writer) target(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		return "", fmt.Errorf("archive member %q has an absolute path", name)
	}
	// tar and bsdtar count a leading "./" when stripping, and definitions'
	// strip values assume that.
	var parts []string
	for i, p := range strings.Split(name, "/") {
		switch {
		case p == "..":
			return "", fmt.Errorf("archive member %q escapes the destination", name)
		case p == "" || (p == "." && i > 0):
		default:
			parts = append(parts, p)
		}
	}
	if len(parts) <= w.opts.strip {
		return "", errSkip
	}
	parts = parts[w.opts.strip:]
	if parts[0] == "." {
		if parts = parts[1:]; len(parts) == 0 {
			return "", errSkip // the archive's own "./" entry
		}
	}
	rel := path.Join(parts...)
	for _, re := range w.exclude {
		if re.MatchString(rel) || re.MatchString(path.Base(rel)) {
			w.logf("excluded %s", rel)
			return "", errSkip
		}
	}
	dst := filepath.Join(w.root, filepath.FromSlash(rel))
	// An earlier member may have planted a symlink; never write through one.
	for dir := filepath.Dir(dst); len(dir) > len(w.root); dir = filepath.Dir(dir) {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("archive member %q would be written through symlink %s", name, dir)
		}
	}
	return dst, nil
}

func (w *writer) mkdir(dst string) error { return os.MkdirAll(dst, 0o755) }

func (w *writer) file(dst string, mode fs.FileMode, r io.Reader) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.Remove(dst) // duplicate member, or a symlink planted at this path
	// Keep the archive's executable bits; drop ownership, setuid and friends.
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	w.n++
	if w.first == "" {
		w.first = dst
	}
	return f.Close()
}

// symlink creates the link only if it resolves inside root; others are
// skipped.
func (w *writer) symlink(dst, linkname string) error {
	resolved := filepath.Join(filepath.Dir(dst), filepath.FromSlash(linkname))
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") || !within(w.root, resolved) {
		w.logf("skipped symlink %s -> %s (points outside the destination)", dst[len(w.root)+1:], linkname)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.Remove(dst)
	w.n++
	return os.Symlink(linkname, dst)
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *writer) tar(tr *tar.Reader) error {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		dst, err := w.target(h.Name)
		if err == errSkip {
			continue
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = w.mkdir(dst)
		case tar.TypeReg:
			err = w.file(dst, fs.FileMode(h.Mode), tr)
		case tar.TypeSymlink:
			err = w.symlink(dst, h.Linkname)
		case tar.TypeLink:
			// Hard link to an earlier member: same strip/safety rules, then copy.
			src, lerr := w.target(h.Linkname)
			if lerr != nil {
				w.logf("skipped hard link %s -> %s", h.Name, h.Linkname)
				continue
			}
			err = w.copyMember(src, dst)
		default:
			w.logf("skipped %s (unsupported entry type %q)", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

func (w *writer) copyMember(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return w.file(dst, info.Mode(), in)
}

func (w *writer) zip(zr *zip.Reader) error {
	for _, zf := range zr.File {
		dst, err := w.target(zf.Name)
		if err == errSkip {
			continue
		}
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir() || strings.HasSuffix(zf.Name, "/"):
			err = w.mkdir(dst)
		case mode&fs.ModeSymlink != 0:
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				var link []byte
				link, err = io.ReadAll(io.LimitReader(rc, 4096))
				rc.Close()
				if err == nil {
					err = w.symlink(dst, string(link))
				}
			}
		default:
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				err = w.file(dst, mode, rc)
				rc.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", zf.Name, err)
		}
	}
	return nil
}

func (w *writer) sevenzip(zr *sevenzip.Reader) error {
	for _, zf := range zr.File {
		dst, err := w.target(zf.Name)
		if err == errSkip {
			continue
		}
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			err = w.mkdir(dst)
		} else {
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				err = w.file(dst, zf.Mode(), rc)
				rc.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", zf.Name, err)
		}
	}
	return nil
}

// globRegexp compiles a member glob with bsdtar's semantics: * crosses
// directory separators.
func globRegexp(glob string) *regexp.Regexp { return globRegexpCI(glob, false) }

func globRegexpCI(glob string, ci bool) *regexp.Regexp {
	var b strings.Builder
	if ci {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			if j := strings.IndexByte(glob[i:], ']'); j > 0 {
				class := glob[i : i+j+1]
				if strings.HasPrefix(class, "[!") {
					class = "[^" + class[2:]
				}
				b.WriteString(class)
				i += j
				continue
			}
			b.WriteString(`\[`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(regexp.QuoteMeta(glob))
	}
	return re
}
