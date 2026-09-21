//go:build !unix

package sh

import "os/exec"

// killTree: exec's default (kill the shell) is the best available here.
func killTree(cmd *exec.Cmd) {}
