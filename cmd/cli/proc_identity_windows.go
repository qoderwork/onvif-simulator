//go:build windows

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE from the Windows process API: GetExitCodeProcess
// reports it while the process is running.
const stillActive = 259

// processIdentity resolves a pid's executable path and creation time through
// the Win32 process API.
func processIdentity(pid int) (procIdentity, error) {
	if pid <= 0 {
		return procIdentity{}, errors.New("proc: invalid pid")
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid), //nolint:gosec // pid comes from a manifest written on this host.
	)
	if err != nil {
		return procIdentity{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }() //nolint:errcheck // handle close is not actionable.

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return procIdentity{}, fmt.Errorf("query image name %d: %w", pid, err)
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return procIdentity{}, fmt.Errorf("query process times %d: %w", pid, err)
	}
	return procIdentity{
		Exe:     windows.UTF16ToString(buf[:size]),
		StartNS: creation.Nanoseconds(),
	}, nil
}

// processAlive reports whether pid refers to a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid), //nolint:gosec // manifest pid.
	)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }() //nolint:errcheck // handle close is not actionable.

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
