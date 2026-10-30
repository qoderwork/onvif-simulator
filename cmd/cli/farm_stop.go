package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runFarmStop terminates a farm from its manifest. It kills the supervisor
// first (so it cannot respawn children), then every recorded instance, and
// finally waits until the HTTP and RTSP ports are closed — which is what an
// operator actually cares about when a previous farm was force-killed or
// crashed and its children are still holding ports.
//
// The manifest is written before the farm is declared up and contains both the
// parent pid and every child pid, so this works whether the farm is still
// running or was killed with Task Manager / kill -9.
func runFarmStop(args []string) error {
	fs := flag.NewFlagSet("farm stop", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "path to the farm manifest.json")
	outDir := fs.String("out", "", "farm output directory (manifest.json inside)")
	timeout := fs.Duration("timeout", 10*time.Second, "max time to wait for the endpoints to close")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := strings.TrimSpace(*manifestPath)
	if path == "" {
		if strings.TrimSpace(*outDir) == "" {
			return errFarmManifestRequired
		}
		path = filepath.Join(*outDir, "manifest.json")
	}

	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied manifest path.
	if err != nil {
		return fmt.Errorf("farm stop: read manifest: %w", err)
	}
	// Tolerate a BOM from hand-edited manifests (Notepad / PowerShell 5.1),
	// same as config.Load does.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var manifest farmManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("farm stop: parse manifest: %w", err)
	}

	// Kill the supervisor first: a live farm would otherwise restart each
	// child we terminate (and on Windows its job object kills the rest). The
	// identity check ensures a stale manifest cannot kill a pid the OS has
	// since reused for an unrelated process.
	parentKilled := false
	if manifest.ParentPID > 0 {
		kill, reason := stopDecision(manifest.ParentPID, manifest.ParentExe, manifest.ParentStartedNS)
		switch {
		case !kill:
			if reason != reasonAlreadyExited {
				fmt.Fprintf(os.Stderr, "farm stop: skipping parent %d: %s\n", manifest.ParentPID, reason)
			}
		default:
			if err := terminatePID(manifest.ParentPID); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(os.Stderr, "farm stop: parent %d: %v\n", manifest.ParentPID, err)
			}
			parentKilled = true
		}
	}
	if parentKilled {
		time.Sleep(farmStopGrace)
	}

	// Children may already be gone: on Windows the job object takes them when
	// the parent dies, and a stale manifest may point at exited processes.
	// Errors here are therefore best-effort; the port check below is the
	// authoritative success signal.
	attempted := 0
	for _, inst := range manifest.Instances {
		if inst.PID <= 0 {
			continue
		}
		kill, reason := stopDecision(inst.PID, inst.Exe, inst.StartedNS)
		if !kill {
			if reason != reasonAlreadyExited {
				fmt.Fprintf(os.Stderr, "farm stop: skipping instance %d (pid %d): %s\n", inst.Index, inst.PID, reason)
			}
			continue
		}
		_ = terminatePID(inst.PID) //nolint:errcheck // liveness is verified via ports.
		attempted++
	}

	open := waitPortsClosed(manifest.Instances, *timeout)
	if len(open) > 0 {
		return fmt.Errorf("farm stop: %d endpoint(s) still open after %s: %v", len(open), *timeout, open)
	}
	// The rest exited on their own (typically because the Windows job object
	// took them when the supervisor died), so report both numbers instead of
	// a confusing zero.
	fmt.Printf("farm stop: parent handled, %d/%d instance(s) signalled, the rest already exited; endpoints closed (%s)\n",
		attempted, len(manifest.Instances), path)
	return nil
}

// farmStopGrace gives a killed supervisor a moment to die before its children
// are terminated, so it cannot restart them in between.
const farmStopGrace = 300 * time.Millisecond

// waitPortsClosed polls every instance's HTTP and RTSP port until they stop
// accepting connections or the timeout expires, returning the ports still
// open. Both are checked: a partially killed child can release its HTTP
// listener while still serving RTSP, and a leftover RTSP port would collide
// with the next farm's port block.
func waitPortsClosed(instances []farmInstance, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		open := make([]int, 0, 2*len(instances))
		for _, inst := range instances {
			if inst.HTTPPort > 0 && portOpen(inst.HTTPPort) {
				open = append(open, inst.HTTPPort)
			}
			if inst.RTSPPort > 0 && portOpen(inst.RTSPPort) {
				open = append(open, inst.RTSPPort)
			}
		}
		if len(open) == 0 || time.Now().After(deadline) {
			return open
		}
		time.Sleep(farmPollInterval)
	}
}

// portOpen reports whether something accepts TCP connections on 127.0.0.1:port.
func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), farmDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close() //nolint:errcheck // best-effort probe.
	return true
}
