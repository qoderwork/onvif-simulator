//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// applyChildProcessGroup places the child in its own process group (so a group
// kill reaps descendants) AND asks the kernel to SIGKILL it when the farm
// process dies — the Linux counterpart of the Windows kill-on-close job.
//
// Pdeathsig is delivered by the kernel to the child when the parent process
// (more precisely, the thread that forked it) exits, so a farm that is
// kill -9'd, OOM-killed or crashes cannot leave orphaned cameras holding
// ports. The signal is fatal and uncatchable, which is acceptable for a test
// rig: there is no state to flush beyond the log file. The fork→prctl window is
// covered by serve's start-up parent check (see checkFarmParent).
func applyChildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
