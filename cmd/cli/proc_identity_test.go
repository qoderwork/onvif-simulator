package main

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

func TestIdentityMatches(t *testing.T) {
	t.Parallel()

	got := procIdentity{Exe: "/usr/local/bin/onvif-simulator", StartNS: 1_700_000_000}
	if !identityMatches(got, "/usr/local/bin/onvif-simulator", 1_700_000_000) {
		t.Fatal("identical identity must match")
	}
	if identityMatches(got, "/usr/local/bin/onvif-simulator", 1_700_000_001) {
		t.Fatal("different start time must not match")
	}
	if identityMatches(got, "/opt/other-binary", 1_700_000_000) {
		t.Fatal("different executable must not match")
	}
}

// TestSameExeDeletedSuffix covers Linux reporting " (deleted)" after the
// running binary was replaced: `farm stop` must still recognise its child.
func TestSameExeDeletedSuffix(t *testing.T) {
	t.Parallel()

	if !sameExe("/opt/sim/onvif-simulator (deleted)", "/opt/sim/onvif-simulator") {
		t.Fatal("deleted suffix should be ignored")
	}
}

func TestStopDecisionLegacyManifest(t *testing.T) {
	t.Parallel()

	kill, reason := stopDecision(1234, "", 0)
	if !kill || reason != "" {
		t.Fatalf("legacy manifest = (%v, %q), want (true, \"\")", kill, reason)
	}
}

func TestStopDecisionRejectsReusedPID(t *testing.T) {
	old := processIdentityFn
	t.Cleanup(func() { processIdentityFn = old })
	processIdentityFn = func(int) (procIdentity, error) {
		return procIdentity{Exe: "/opt/sim/onvif-simulator", StartNS: 100}, nil
	}

	kill, reason := stopDecision(1234, "/opt/sim/onvif-simulator", 200)
	if kill {
		t.Fatal("pid with a different start time must not be killed")
	}
	if reason != reasonPIDReused {
		t.Fatalf("reason = %q, want %q", reason, reasonPIDReused)
	}
}

func TestStopDecisionAllowsMatchingPID(t *testing.T) {
	old := processIdentityFn
	t.Cleanup(func() { processIdentityFn = old })
	processIdentityFn = func(int) (procIdentity, error) {
		return procIdentity{Exe: "/opt/sim/onvif-simulator", StartNS: 100}, nil
	}

	kill, reason := stopDecision(1234, "/opt/sim/onvif-simulator", 100)
	if !kill || reason != "" {
		t.Fatalf("matching identity = (%v, %q), want (true, \"\")", kill, reason)
	}
}

func TestStopDecisionSkipsExitedProcess(t *testing.T) {
	old := processIdentityFn
	t.Cleanup(func() { processIdentityFn = old })
	processIdentityFn = func(int) (procIdentity, error) {
		return procIdentity{}, errors.New("no such process")
	}

	// 2^31-1 is above every real pid_max and does not exist.
	kill, reason := stopDecision(1<<31-1, "/opt/sim/onvif-simulator", 100)
	if kill {
		t.Fatal("exited process must not be killed")
	}
	if reason != reasonAlreadyExited {
		t.Fatalf("reason = %q, want %q", reason, reasonAlreadyExited)
	}
}

// TestCheckFarmParent covers the child-side guard that closes the fork→prctl
// window for Linux Pdeathsig.
func TestCheckFarmParent(t *testing.T) {
	t.Setenv(farmParentPIDEnv, "")
	if err := checkFarmParent(); err != nil {
		t.Fatalf("unset env: %v", err)
	}

	t.Setenv(farmParentPIDEnv, strconv.Itoa(os.Getppid()))
	if err := checkFarmParent(); err != nil {
		t.Fatalf("live parent: %v", err)
	}

	t.Setenv(farmParentPIDEnv, strconv.Itoa(os.Getppid()+1))
	if err := checkFarmParent(); err == nil {
		t.Fatal("dead parent must be rejected")
	}
}
