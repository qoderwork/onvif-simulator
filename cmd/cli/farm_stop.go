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

// defaultFarmStopTimeout bounds how long `farm stop` waits for the recorded
// endpoints to stop accepting connections.
const defaultFarmStopTimeout = 10 * time.Second

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
	timeout := fs.Duration("timeout", defaultFarmStopTimeout, "max time to wait for the endpoints to close")
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

	manifest, err := loadFarmManifest(path)
	if err != nil {
		return err
	}

	// Kill the supervisor first: a live farm would otherwise restart each
	// child we terminate (and on Windows its job object kills the rest). The
	// identity check ensures a stale manifest cannot kill a pid the OS has
	// since reused for an unrelated process.
	if stopFarmParent(&manifest) {
		time.Sleep(farmStopGrace)
	}

	// Children may already be gone: on Windows the job object takes them when
	// the parent dies, and a stale manifest may point at exited processes.
	// Errors here are therefore best-effort; the port check below is the
	// authoritative success signal.
	attempted := 0
	for i := range manifest.Instances {
		if manifest.Instances[i].PID <= 0 {
			continue
		}
		kill, reason := stopInstance(i, &manifest)
		if !kill {
			if reason != reasonAlreadyExited {
				fmt.Fprintf(os.Stderr, "farm stop: skipping instance %d (pid %d): %s\n",
					manifest.Instances[i].Index, manifest.Instances[i].PID, reason)
			}
			continue
		}
		attempted++
	}

	open := waitPortsClosed(manifest.Instances, *timeout)
	if len(open) > 0 {
		return fmt.Errorf("%w after %s: %v", errFarmStopEndpointsOpen, *timeout, open)
	}
	// The rest exited on their own (typically because the Windows job object
	// took them when the supervisor died), so report both numbers instead of
	// a confusing zero.
	fmt.Printf("farm stop: parent handled, %d/%d instance(s) signaled, the rest already exited; endpoints closed (%s)\n",
		attempted, len(manifest.Instances), path)
	return nil
}

// loadFarmManifest reads and parses a farm manifest, tolerating the BOM a
// hand-edited manifest picks up from Notepad / PowerShell 5.1 (same as
// config.Load does).
func loadFarmManifest(path string) (farmManifest, error) {
	// The path is operator-supplied by design; `farm stop` is a local
	// maintenance command, not a network surface.
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied manifest path.
	if err != nil {
		return farmManifest{}, fmt.Errorf("farm stop: read manifest: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var manifest farmManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return farmManifest{}, fmt.Errorf("farm stop: parse manifest: %w", err)
	}
	return manifest, nil
}

// stopFarmParent terminates the recorded supervisor and reports whether it was
// signaled. A stale manifest is refused rather than killing whatever process
// now owns that pid.
func stopFarmParent(manifest *farmManifest) bool {
	if manifest.ParentPID <= 0 {
		return false
	}
	kill, reason := stopDecision(manifest.ParentPID, manifest.ParentExe, manifest.ParentStartedNS)
	if !kill {
		if reason != reasonAlreadyExited {
			fmt.Fprintf(os.Stderr, "farm stop: skipping parent %d: %s\n", manifest.ParentPID, reason)
		}
		return false
	}
	if err := terminatePID(manifest.ParentPID); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Fprintf(os.Stderr, "farm stop: parent %d: %v\n", manifest.ParentPID, err)
	}
	return true
}

// errFarmStopEndpointsOpen means `farm stop` signaled everything it could but
// some ports still accept connections, which is the only real failure signal
// (a process can be gone while a stale listener survives on Windows).
var errFarmStopEndpointsOpen = errors.New("farm stop: endpoint(s) still open")

// stopInstance applies stopDecision to one recorded instance and terminates it
// when allowed. It returns whether the kill was attempted plus the reason when
// it was not. Indexing the slice instead of ranging over it avoids copying a
// 200-byte farmInstance per iteration, and keeps the decision next to the
// signal so the two cannot drift apart.
func stopInstance(i int, manifest *farmManifest) (attempted bool, reason string) {
	inst := &manifest.Instances[i]
	kill, why := stopDecision(inst.PID, inst.Exe, inst.StartedNS)
	if !kill {
		return false, why
	}
	_ = terminatePID(inst.PID) //nolint:errcheck // liveness is verified via ports.
	return true, ""
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
		for i := range instances {
			if instances[i].HTTPPort > 0 && portOpen(instances[i].HTTPPort) {
				open = append(open, instances[i].HTTPPort)
			}
			if instances[i].RTSPPort > 0 && portOpen(instances[i].RTSPPort) {
				open = append(open, instances[i].RTSPPort)
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
	conn, err := dialProbe(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close() //nolint:errcheck // best-effort probe.
	return true
}
