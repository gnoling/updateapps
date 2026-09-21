package install

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// DebControl is what a .deb says about itself.
type DebControl struct {
	Package, Version, Architecture string
}

// ReadDeb reads a .deb's control fields natively: an ar archive holding
// control.tar.*.
func ReadDeb(file string) (*DebControl, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != "!<arch>\n" {
		return nil, errors.New("not a .deb (no ar archive header)")
	}
	for {
		header := make([]byte, 60)
		if _, err := io.ReadFull(br, header); err != nil {
			return nil, errors.New("not a .deb (no control.tar member)")
		}
		name := strings.TrimRight(strings.TrimSpace(string(header[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(header[48:58])), 10, 64)
		if err != nil || string(header[58:60]) != "`\n" {
			return nil, errors.New("corrupt ar header in .deb")
		}
		member := io.LimitReader(br, size)
		if strings.HasPrefix(name, "control.tar") {
			return readControl(bufio.NewReader(member))
		}
		if _, err := io.Copy(io.Discard, member); err != nil {
			return nil, err
		}
		if size%2 == 1 { // members are padded to even offsets
			br.Discard(1)
		}
	}
}

func readControl(br *bufio.Reader) (*DebControl, error) {
	r, closeR, err := decompress(br)
	if err != nil {
		return nil, err
	}
	defer closeR()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New(".deb has no control file")
		}
		if path.Clean(h.Name) != "control" {
			continue
		}
		c := &DebControl{}
		sc := bufio.NewScanner(tr)
		for sc.Scan() {
			key, value, ok := strings.Cut(sc.Text(), ":")
			if !ok || strings.HasPrefix(key, " ") {
				continue
			}
			switch value = strings.TrimSpace(value); key {
			case "Package":
				c.Package = value
			case "Version":
				c.Version = value
			case "Architecture":
				c.Architecture = value
			}
		}
		if c.Package == "" || c.Version == "" {
			return nil, errors.New(".deb control file lacks Package or Version")
		}
		return c, nil
	}
}

// DebInstalled returns the installed version of pkg, or "" if it isn't installed.
func DebInstalled(ctx context.Context, pkg string) string {
	out, err := output(ctx, "dpkg-query", "-W", "-f=${db:Status-Status} ${Version}", pkg)
	status, version, _ := strings.Cut(out, " ")
	if err != nil || status != "installed" {
		return ""
	}
	return version
}

// debArch maps GOARCH to Debian's names, for when dpkg can't be asked.
var debArch = map[string]string{"amd64": "amd64", "arm64": "arm64", "386": "i386", "arm": "armhf", "riscv64": "riscv64"}

// InstallDebs installs all items in one apt transaction: one privilege prompt,
// one dpkg lock. logf(i) logs for item i.
func (s System) InstallDebs(ctx context.Context, items []Item, logf func(item int) func(string, ...any)) []Outcome {
	out := make([]Outcome, len(items))
	if _, err := exec.LookPath("apt-get"); err != nil {
		for i := range out {
			out[i].Err = errors.New("install type deb needs apt-get (a Debian-family system)")
		}
		return out
	}
	arch, err := output(ctx, "dpkg", "--print-architecture")
	if err != nil {
		arch = debArch[runtime.GOARCH]
	}

	controls := make([]*DebControl, len(items))
	var todo []int
	for i, it := range items {
		c, err := ReadDeb(it.File)
		switch {
		case err != nil:
			out[i].Err = err
		case c.Architecture != "all" && c.Architecture != arch:
			out[i].Err = fmt.Errorf("%s is built for %s but this system is %s (check the asset pattern)", c.Package, c.Architecture, arch)
		case DebInstalled(ctx, c.Package) == c.Version:
			logf(i)("%s %s is already installed", c.Package, c.Version)
			out[i].Already = true
		default:
			logf(i)("package %s %s (%s)", c.Package, c.Version, c.Architecture)
			controls[i] = c
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return out
	}

	install := func(idx []int) error {
		argv := []string{"apt-get", "install", "-y"}
		for _, i := range idx {
			argv = append(argv, items[i].File) // absolute path: apt takes it as a file, not a name
		}
		cmd, err := s.asRoot(ctx, argv...)
		if err != nil {
			return err
		}
		return runLogged(cmd, logf(idx[0]))
	}
	err = install(todo)
	if err != nil && !isNoPrivilege(err) && len(todo) > 1 {
		// One bad package fails the transaction; retry singly to isolate it.
		logf(todo[0])("batch failed; installing packages one at a time")
		for _, i := range todo {
			if ierr := install([]int{i}); ierr != nil && DebInstalled(ctx, controls[i].Package) != controls[i].Version {
				out[i].Err = ierr
			}
		}
		err = nil
	}

	for _, i := range todo {
		c := controls[i]
		switch {
		case out[i].Err != nil:
		case DebInstalled(ctx, c.Package) == c.Version:
		case err != nil && isNoPrivilege(err):
			kept, kerr := s.keep(items[i].File)
			if kerr != nil {
				out[i].Err = kerr
				continue
			}
			out[i].ActionNeeded = fmt.Sprintf("%s %s is downloaded but installing it needs root:\nsudo apt-get install %s", c.Package, c.Version, kept)
		case err != nil:
			out[i].Err = err
		default:
			out[i].Err = fmt.Errorf("apt-get succeeded but %s is at %q, not %s", c.Package, DebInstalled(ctx, c.Package), c.Version)
		}
	}
	return out
}

// isNoPrivilege separates "couldn't become root" (our refusal, sudo -n, a
// dismissed pkexec: exit 126/127) from apt failing.
func isNoPrivilege(err error) bool {
	if errors.Is(err, ErrNoPrivilege) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "a password is required") || strings.Contains(msg, "a terminal is required") ||
		strings.Contains(msg, "no tty present") || strings.Contains(msg, "Not authorized") ||
		strings.Contains(msg, "not allowed for user") {
		return true
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && strings.HasPrefix(msg, "pkexec:") && (ee.ExitCode() == 126 || ee.ExitCode() == 127)
}
