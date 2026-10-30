//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// terminateChild kills the child's whole process group so helpers the child
// launched (for example the rpicam-mtxrpicam pair) do not survive us.
func terminateChild(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	// The child was started with Setpgid, so its pid is also its pgid.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// terminatePID force-kills a pid recorded in a farm manifest. It targets the
// process group first (children are started with Setpgid), falling back to the
// single process when the group is already gone.
func terminatePID(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
