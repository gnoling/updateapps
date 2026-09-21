// Package systest fakes the system package managers for tests: real .deb
// files, plus stand-in sudo/apt-get/dpkg/flatpak on PATH.
package systest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Deb builds a minimal but genuine .deb (ar + control.tar.gz + data.tar.gz).
func Deb(pkg, version, arch string) []byte {
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: nobody\nDescription: test package\n line two of the description\n", pkg, version, arch)
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(name string, data []byte) {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
		out.Write(data)
		if len(data)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tarGz(map[string]string{"./control": control, "./md5sums": ""}))
	member("data.tar.gz", tarGz(map[string]string{"./usr/bin/" + pkg: "#!/bin/sh\n"}))
	return out.Bytes()
}

func tarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names) // map order would make the archive's size vary between calls
	for _, name := range names {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[name]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Fake is a stand-in system: a directory of fake commands ahead of the real
// ones on PATH, plus the state they keep.
type Fake struct {
	t   *testing.T
	Dir string // state: dpkg/<pkg> holds a version; flatpak/<id> holds a commit; *.log record calls
}

// Install puts the fake commands on PATH for the duration of the test.
func Install(t *testing.T) *Fake {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake package managers are shell scripts")
	}
	f := &Fake{t: t, Dir: t.TempDir()}
	bin := filepath.Join(f.Dir, "bin")
	for _, d := range []string{bin, filepath.Join(f.Dir, "dpkg"), filepath.Join(f.Dir, "flatpak"), filepath.Join(f.Dir, "remote")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range scripts {
		script := "#!/bin/sh\nS=" + f.Dir + "\n" + body
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// Deny makes becoming root impossible (sudo -n wants a password, flatpak
// --system isn't authorized); Allow undoes it.
func (f *Fake) Deny()  { os.WriteFile(filepath.Join(f.Dir, "deny"), nil, 0o644) }
func (f *Fake) Allow() { os.Remove(filepath.Join(f.Dir, "deny")) }

// DpkgVersion is the installed version of pkg ("" if not installed).
func (f *Fake) DpkgVersion(pkg string) string { return f.read("dpkg", pkg) }

// SetDpkg marks pkg as installed at version.
func (f *Fake) SetDpkg(pkg, version string) { f.write("dpkg", pkg, version) }

// FlatpakCommit is the installed commit of ref, e.g. "org.x.App//stable".
func (f *Fake) FlatpakCommit(ref string) string {
	return f.read("flatpak", strings.ReplaceAll(ref, "/", "_"))
}

// SetFlatpak marks ref installed at commit, from origin.
func (f *Fake) SetFlatpak(ref, commit, origin string) {
	f.write("flatpak", strings.ReplaceAll(ref, "/", "_"), commit)
	f.write("flatpak", strings.ReplaceAll(ref, "/", "_")+".origin", origin)
}

// SetRemote sets what any remote offers for ref.
func (f *Fake) SetRemote(ref, commit, version string) {
	f.write("remote", strings.ReplaceAll(ref, "/", "_"), commit+" "+version)
}

// Calls returns the recorded invocations of cmd (apt-get, sudo, flatpak), one per line.
func (f *Fake) Calls(cmd string) []string {
	data, _ := os.ReadFile(filepath.Join(f.Dir, cmd+".log"))
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (f *Fake) read(dir, name string) string {
	data, _ := os.ReadFile(filepath.Join(f.Dir, dir, name))
	return strings.TrimSpace(string(data))
}

func (f *Fake) write(dir, name, value string) {
	if err := os.WriteFile(filepath.Join(f.Dir, dir, name), []byte(value+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// The fakes understand just enough. apt-get reads package and version out of
// the file name (pkg_version_arch.deb), and fails on any file with "broken"
// in its name, the way one bad package fails a real transaction.
var scripts = map[string]string{
	"sudo": `echo "$*" >> $S/sudo.log
if [ -e $S/deny ]; then echo "sudo: a password is required" >&2; exit 1; fi
while [ $# -gt 0 ]; do case "$1" in -n) shift;; -p) shift 2;; --) shift; break;; *) break;; esac; done
exec "$@"
`,
	"pkexec": `echo "$*" >> $S/pkexec.log
if [ -e $S/deny ]; then echo "Error executing command as another user: Not authorized" >&2; exit 126; fi
exec "$@"
`,
	"dpkg": `[ "$1" = --print-architecture ] && echo amd64
`,
	"dpkg-query": `for a; do pkg=$a; done
[ -s $S/dpkg/$pkg ] || { echo "dpkg-query: no packages found matching $pkg" >&2; exit 1; }
printf 'installed %s' "$(cat $S/dpkg/$pkg)"
`,
	"apt-get": `echo "$*" >> $S/apt-get.log
for a; do case "$a" in *broken*) echo "E: Sub-process /usr/bin/dpkg returned an error code (1)" >&2; exit 100;; esac; done
for a; do case "$a" in *.deb) b=$(basename "$a" .deb); pkg=${b%%_*}; rest=${b#*_}; echo "${rest%%_*}" > $S/dpkg/$pkg; echo "Setting up $pkg (${rest%%_*}) ...";; esac; done
`,
	"flatpak": `echo "$*" >> $S/flatpak.log
cmd=$1; shift
scope=--user; from=; bundle=; show=; pos=
while [ $# -gt 0 ]; do case "$1" in
  --user|--system) scope=$1;; --show-commit|--show-origin) show=$1;; --from) from=$2; shift;; --bundle) bundle=$2; shift;;
  -*) ;; *) pos="$pos $1";; esac; shift; done
set -- $pos
key() { echo "$1" | tr / _; }
denied() { [ "$scope" = --system ] && [ -e $S/deny ]; }
case $cmd in
info)
  f=$S/flatpak/$(key "$1"); [ -s "$f" ] || { echo "error: $1 not installed" >&2; exit 1; }
  if [ "$show" = --show-origin ]; then cat "$f.origin"; else cat "$f"; fi;;
remote-info)
  f=$S/remote/$(key "$2"); [ -s "$f" ] || { echo "error: Can't find ref $2 in remote $1" >&2; exit 1; }
  read commit version < "$f"; printf '\n        ID: x\n    Commit: %s\n   Version: %s\n' "$commit" "$version";;
install|update)
  if denied; then echo "error: Flatpak system operation Deploy not allowed for user" >&2; exit 1; fi
  if [ -n "$bundle" ]; then echo bundle > $S/flatpak/bundle-installed; exit 0; fi
  if [ -n "$from" ]; then ref="$(sed -n 's/^Name=//p' "$from")//$(sed -n 's/^Branch=//p' "$from")"; origin=$(sed -n 's/^SuggestRemoteName=//p' "$from")
  elif [ $cmd = install ]; then origin=$1; ref=$2
  else ref=$1; origin=$(cat $S/flatpak/$(key "$ref").origin); fi
  read commit version < $S/remote/$(key "$ref") || { echo "error: no such ref" >&2; exit 1; }
  echo "$commit" > $S/flatpak/$(key "$ref"); echo "$origin" > $S/flatpak/$(key "$ref").origin;;
esac
`,
}
