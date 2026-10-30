package main

import (
	"path/filepath"
	"runtime"
	"strings"
)

// procIdentity is the fingerprint farm records for every process it starts, so
// `farm stop` can tell "our simulator" from "an unrelated process that was
// assigned the same pid after the manifest went stale".
//
// Exe is the resolved executable path and StartNS is the process creation
// time in Unix nanoseconds. Together they survive PID reuse: a new process
// will differ on at least one of them.
type procIdentity struct {
	Exe     string
	StartNS int64
}

// processIdentityFn is a test seam for the platform-specific lookup.
var processIdentityFn = processIdentity

// identityMatches reports whether the live fingerprint matches what the
// manifest recorded. An empty Exe/zero StartNS means "legacy manifest" and
// is handled by the caller, not here.
func identityMatches(got procIdentity, wantExe string, wantStart int64) bool {
	if got.StartNS != wantStart {
		return false
	}
	return sameExe(got.Exe, wantExe)
}

// sameExe compares executable paths. Windows is case-insensitive; elsewhere
// the path is compared byte-for-byte. A " (deleted)" suffix (Linux reports it
// when the binary was replaced while running) is stripped so a rebuilt binary
// still matches its running process.
func sameExe(a, b string) bool {
	a = filepath.Clean(strings.TrimSuffix(a, " (deleted)"))
	b = filepath.Clean(strings.TrimSuffix(b, " (deleted)"))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Reasons returned by stopDecision. reasonAlreadyExited is the only one the
// caller may stay quiet about; the others are worth surfacing.
const (
	reasonAlreadyExited = "already exited"
	reasonPIDReused     = "pid reused by another process"
)

// stopDecision decides whether farm stop may signal pid. It returns false with
// a human-readable reason when the pid no longer belongs to the process the
// manifest recorded, so a stale manifest can never kill an unrelated process.
//
// Legacy manifests (no identity) are still killed: they predate the check and
// the operator asked for a stop.
func stopDecision(pid int, wantExe string, wantStart int64) (bool, string) {
	if pid <= 0 {
		return false, "no pid recorded"
	}
	if strings.TrimSpace(wantExe) == "" || wantStart == 0 {
		return true, ""
	}
	got, err := processIdentityFn(pid)
	if err != nil {
		if !processAlive(pid) {
			return false, reasonAlreadyExited
		}
		// The platform cannot report identity (or access was denied). Killing
		// is still better than leaving a farm running; the port check at the
		// end remains the success criterion.
		return true, "identity unavailable; falling back to pid"
	}
	if !identityMatches(got, wantExe, wantStart) {
		return false, reasonPIDReused
	}
	return true, ""
}
