package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/flatpak"
)

// System reaches the system's package managers: the only place this project
// runs anything as root.
type System struct {
	// Privilege: auto, sudo, pkexec or none. auto is nothing if root, pkexec
	// under a GUI, sudo at a terminal, else sudo -n (needs a NOPASSWD rule).
	Privilege   string
	Interactive bool   // a person at a terminal can answer a sudo prompt
	GUI         bool   // a polkit agent can show a dialog
	PendingDir  string // where downloads wait when root isn't available
	FlatpakUser bool   // default flatpak scope when a definition doesn't say
}

// ErrNoPrivilege means root is needed and there's no way to get it right now.
var ErrNoPrivilege = errors.New("root is required and isn't available non-interactively")

// asRoot wraps argv so it runs as root.
func (s System) asRoot(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	if os.Geteuid() == 0 {
		return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
	}
	mode := s.Privilege
	if mode == "" || mode == "auto" {
		switch {
		case s.GUI:
			mode = "pkexec"
		default:
			mode = "sudo"
		}
	}
	switch mode {
	case "pkexec":
		return exec.CommandContext(ctx, "pkexec", argv...), nil
	case "sudo":
		if !s.Interactive {
			// -n: fail instead of prompting; nobody's there to answer.
			return exec.CommandContext(ctx, "sudo", append([]string{"-n", "--"}, argv...)...), nil
		}
		cmd := exec.CommandContext(ctx, "sudo", append([]string{"-p", "[updateapps] password for %u to install system packages: ", "--"}, argv...)...)
		cmd.Stdin = os.Stdin
		return cmd, nil
	}
	return nil, ErrNoPrivilege
}

// output runs a read-only query command.
func output(ctx context.Context, name string, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(stderr.String(), 3))
	}
	return strings.TrimSpace(string(out)), nil
}

// runLogged runs cmd, logging its output. A failure carries the output's tail.
func runLogged(cmd *exec.Cmd, logf func(string, ...any)) error {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	logf("run: %s", strings.Join(cmd.Args, " "))
	err := cmd.Run()
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "" {
			logf("  %s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", filepath.Base(cmd.Args[0]), err, lastLines(out.String(), 5))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Item is one app's download waiting for its batch.
type Item struct {
	App  *def.App
	File string // downloaded .deb / .flatpak / .flatpakref; "" when nothing is downloaded
}

// Outcome is what happened to one Item.
type Outcome struct {
	Err error
	// ActionNeeded: not installed, not a failure; the command the user must
	// run. Nothing is recorded.
	ActionNeeded string
	// Version/Display override the resolved version when the package manager
	// knows better (a flatpak's commit).
	Version, Display string
	// Already: the system already had this exact version; nothing was run.
	Already bool
}

// keep moves a download into PendingDir so the user can install it by hand.
func (s System) keep(file string) (string, error) {
	if err := os.MkdirAll(s.PendingDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(s.PendingDir, filepath.Base(file))
	if err := moveFile(file, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// NotAdoptable says why mark-current can't record a system-installed app, or
// "": the package manager must confirm it's installed.
func (s System) NotAdoptable(ctx context.Context, app *def.App, version string) string {
	switch app.Install.Type {
	case def.InstallDeb:
		if app.Install.Package == "" {
			return "install.package isn't set, so dpkg can't be asked"
		}
		if DebInstalled(ctx, app.Install.Package) == "" {
			return "package " + app.Install.Package + " isn't installed"
		}
	case def.InstallFlatpak:
		if app.Source.Type != def.SourceFlatpak {
			// A bundle from a release: only the application id can tell us.
			if app.Install.Package == "" {
				return "install.package (the application id) isn't set, so flatpak can't be asked"
			}
			user := s.FlatpakUser
			if app.Install.Scope != "" {
				user = app.Install.Scope == "user"
			}
			if _, _, ok := flatpak.Installed(ctx, user, flatpak.Ref{ID: app.Install.Package}); !ok {
				return app.Install.Package + " isn't installed"
			}
		}
		if strings.HasPrefix(version, "not-installed:") {
			return "it isn't installed"
		}
	}
	return ""
}
