//go:build windows

package main

import (
	"os"
	"os/exec"
)

// terminateChild kills the child process. Windows has no process-group kill
// here, so descendants of the child must exit on their own once it is gone.
func terminateChild(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

// terminatePID force-kills a pid recorded in a farm manifest. `farm stop`
// typically runs while the farm parent still owns a job object, which already
// kills the children when it dies; this covers the Unix-style case and stale
// manifests.
func terminatePID(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
