package install

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/sh"
)

// Missing checks install.needs and returns what isn't there, with the
// command that would install it, or "" when the build can go ahead.
func Missing(ctx context.Context, app *def.App) string {
	n := app.Install.Needs
	if n.Empty() {
		return ""
	}
	var parts []string
	var commands, modules, packages []string
	for _, c := range n.Commands {
		if _, err := exec.LookPath(c); err != nil {
			commands = append(commands, c)
		}
	}
	if len(n.PkgConfig) > 0 {
		if _, err := exec.LookPath("pkg-config"); err != nil {
			commands = append(commands, "pkg-config")
		} else {
			for _, m := range n.PkgConfig {
				if sh.Exec(ctx, "pkg-config", "--exists", m).Run() != nil {
					modules = append(modules, m)
				}
			}
		}
	}
	dpkg, err := exec.LookPath("dpkg-query")
	hasDpkg := err == nil
	if hasDpkg {
		for _, p := range n.Apt {
			out, err := sh.Exec(ctx, dpkg, "-W", "-f=${Status}", p).Output()
			if err != nil || !strings.Contains(string(out), "installed") || strings.Contains(string(out), "not-installed") {
				packages = append(packages, p)
			}
		}
	}
	if len(commands) > 0 {
		parts = append(parts, "commands: "+strings.Join(commands, ", "))
	}
	if len(modules) > 0 {
		parts = append(parts, "pkg-config modules: "+strings.Join(modules, ", "))
	}
	if len(packages) > 0 {
		parts = append(parts, "packages: "+strings.Join(packages, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	msg := "missing build dependencies: " + strings.Join(parts, "; ")
	switch {
	case hasDpkg && len(packages) > 0:
		msg += "\ninstall with: sudo apt-get install " + strings.Join(packages, " ")
	case !hasDpkg && len(n.Apt) > 0:
		msg += fmt.Sprintf("\non Debian or Ubuntu these would be: sudo apt-get install %s", strings.Join(n.Apt, " "))
	}
	return msg
}
