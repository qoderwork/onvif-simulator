//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// userHZ is the Linux USER_HZ exposed by /proc: /proc/<pid>/stat reports
// starttime in these clock ticks, and the kernel fixes USER_HZ at 100 on every
// architecture we build for.
const userHZ = 100

// procStatStartField is the 1-based field number of starttime in
// /proc/<pid>/stat (after comm), i.e. field 22 of the man-page layout.
const procStatStartField = 22

// processIdentity resolves a pid's executable path and creation time from
// /proc. starttime is ticks since boot, so it is converted with the btime
// value from /proc/stat to Unix nanoseconds that survive reboots. The tick
// resolution (10 ms at USER_HZ=100) is far finer than the seconds a pid-reuse
// check needs.
func processIdentity(pid int) (procIdentity, error) {
	if pid <= 0 {
		return procIdentity{}, errors.New("proc: invalid pid")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return procIdentity{}, fmt.Errorf("readlink /proc/%d/exe: %w", pid, err)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // procfs path from a manifest pid.
	if err != nil {
		return procIdentity{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}

	// comm (field 2) is wrapped in parentheses and may itself contain spaces
	// or parentheses, so split after the LAST ')' rather than tokenizing the
	// whole line. The remainder starts at field 3 (state), which makes
	// starttime index 22-3 = 19.
	close := bytes.LastIndexByte(stat, ')')
	if close < 0 || close+1 >= len(stat) {
		return procIdentity{}, fmt.Errorf("parse /proc/%d/stat: missing comm", pid)
	}
	fields := strings.Fields(string(stat[close+1:]))
	idx := procStatStartField - 3
	if len(fields) <= idx {
		return procIdentity{}, fmt.Errorf("parse /proc/%d/stat: too few fields", pid)
	}
	ticks, err := strconv.ParseUint(fields[idx], 10, 64)
	if err != nil {
		return procIdentity{}, fmt.Errorf("parse /proc/%d/stat starttime: %w", pid, err)
	}
	boot, err := linuxBootTime()
	if err != nil {
		return procIdentity{}, err
	}
	return procIdentity{
		Exe:     exe,
		StartNS: boot*int64(time.Second) + int64(ticks)*(int64(time.Second)/userHZ),
	}, nil
}

// linuxBootTime returns the system boot time (Unix seconds) from /proc/stat.
func linuxBootTime() (int64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		const prefix = "btime "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		btime, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse /proc/stat btime: %w", err)
		}
		return btime, nil
	}
	return 0, errors.New("parse /proc/stat: no btime line")
}

// processAlive reports whether pid refers to a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
