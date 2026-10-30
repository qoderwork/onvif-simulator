//go:build !windows && !linux

package main

import (
	"os/exec"
	"syscall"
)

// applyChildProcessGroup places the child in its own process group so a group
// kill reaps descendants. macOS/BSD have no Pdeathsig equivalent; there the
// farm stop command is the cleanup path for a hard-killed parent.
func applyChildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
