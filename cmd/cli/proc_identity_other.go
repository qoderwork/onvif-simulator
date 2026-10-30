//go:build !windows && !linux

package main

import (
	"errors"
	"syscall"
)

// errProcIdentityUnsupported means this platform has no portable way to read
// another process's executable path and creation time. farm stop degrades to
// pid-only termination there (and says so), instead of refusing to clean up.
var errProcIdentityUnsupported = errors.New("proc: process identity is not supported on this platform")

// processIdentity is unavailable on this platform.
func processIdentity(int) (procIdentity, error) {
	return procIdentity{}, errProcIdentityUnsupported
}

// processAlive reports whether pid refers to a running process (POSIX).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
