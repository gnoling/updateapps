//go:build unix

package sh

import (
	"os/exec"
	"syscall"
)

// killTree puts the command in its own process group and makes cancellation
// signal the group.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
