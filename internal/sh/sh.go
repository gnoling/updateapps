// Package sh runs definitions' shell snippets (post hooks, html.command).
package sh

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// Command wraps script for the platform's shell. When ctx ends the whole
// process tree is killed, not just the shell.
func Command(ctx context.Context, script string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", script)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", script)
	}
	killTree(cmd)
	// Don't wait forever on pipes a stray grandchild still holds open.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
