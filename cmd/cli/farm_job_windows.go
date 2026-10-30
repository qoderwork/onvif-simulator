//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// farmJob wraps a Windows job object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Every child is assigned to it right
// after start, so the kernel kills the whole set when the last handle closes
// — i.e. when farm exits normally, crashes, or is force-killed. That closes
// the orphan hole: a hard kill of the parent (Task Manager, taskkill /F) can
// no longer leave cameras bound to their ports with no supervisor.
type farmJob struct {
	handle windows.Handle
}

// newFarmJob creates the kill-on-close job object. A nil *farmJob is valid and
// every method tolerates it, so callers can keep going when job objects are
// unavailable (for example when a sandbox forbids nested jobs).
func newFarmJob() (*farmJob, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h) //nolint:errcheck // teardown after failure.
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	return &farmJob{handle: h}, nil
}

// assign puts one child process into the job. Descendants the child spawns
// (ffmpeg, mtxrpicam) inherit the job unless they explicitly break away.
func (j *farmJob) assign(pid int) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid), //nolint:gosec // pid comes from exec.Cmd on this host.
	)
	if err != nil {
		return fmt.Errorf("open child %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }() //nolint:errcheck // handle close is not actionable.
	if err := windows.AssignProcessToJobObject(j.handle, h); err != nil {
		return fmt.Errorf("assign child %d to job: %w", pid, err)
	}
	return nil
}

// Close releases the job handle. Closing it is what triggers the kernel kill
// of any child still running, so it must happen only after the farm has
// decided to stop (or during process teardown).
func (j *farmJob) Close() {
	if j == nil || j.handle == 0 {
		return
	}
	_ = windows.CloseHandle(j.handle) //nolint:errcheck // best-effort teardown.
	j.handle = 0
}
