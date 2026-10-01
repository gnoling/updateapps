package sh

import (
	"context"
	"os/exec"
	"time"
)

// Exec runs one program directly (no shell) with the same process-tree kill
// as Command.
func Exec(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	killTree(cmd)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
