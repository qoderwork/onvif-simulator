//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestRunServeSignalShutdown(t *testing.T) {
	cfgPath, cleanup := writeTempConfig(t)
	defer cleanup()

	// Use a temp HOME/USERPROFILE so writeControlPortFile lands somewhere ephemeral.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // os.UserHomeDir uses USERPROFILE on Windows

	// This test signals its own pid. Register a handler BEFORE runServe starts:
	// without one, SIGINT arriving before runServe installs its own handler hits
	// Go's default behaviour and kills the whole test binary (`go test` then
	// reports "signal: interrupt"). Having a handler registered means the signal
	// is trapped for the lifetime of the test; runServe's own Notify is
	// delivered alongside this one.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGINT)
	defer signal.Stop(guard)

	done := make(chan error, 1)
	go func() {
		done <- runServe([]string{"-config", cfgPath})
	}()

	// Give the server a moment to bind.
	time.Sleep(300 * time.Millisecond)

	// runServe registers its handler some way into startup, so one signal can
	// still arrive too early. Re-send until it answers: harmless, because the
	// guard keeps the signal trapped instead of fatal. This removes the
	// dependence on how long startup takes on the current machine.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("runServe returned: %v", err)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("runServe did not exit on SIGINT")
}

func TestRunTUIReturnsTUIError(t *testing.T) {
	cfgPath, cleanup := writeTempConfig(t)
	defer cleanup()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // os.UserHomeDir uses USERPROFILE on Windows

	err := runTUI([]string{"-config", cfgPath})
	if err == nil {
		t.Fatal("expected error from runTUI (no TTY in tests)")
	}
	// The TUI panics or errors when there is no TTY; either way, we just
	// need to know the function path executed. Some TUI failures wrap with
	// errors.New, so we use a simple non-nil check above. Sanity guard:
	if errors.Is(err, errUnknownSubcommand) {
		t.Fatalf("unexpected unknown-subcommand error: %v", err)
	}
}
