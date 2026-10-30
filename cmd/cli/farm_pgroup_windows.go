//go:build windows

package main

import "os/exec"

// applyChildProcessGroup is a no-op on Windows, where there is no POSIX
// process-group concept; orphan cleanup is owned by the farm job object
// (see farm_job_windows.go), and Process.Kill reaps direct children directly.
func applyChildProcessGroup(_ *exec.Cmd) {}
