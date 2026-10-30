package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GyeongHoKim/onvif-simulator/internal/config"
)

// farmTestTemplate returns a minimal valid template that exercises the
// derivation paths (go264 profile, seed left at zero, no XAddrs).
func farmTestTemplate() config.Config {
	return config.Config{
		Version: config.CurrentVersion,
		Device: config.DeviceConfig{
			UUID:         "urn:uuid:00000000-0000-4000-8000-0000000000aa",
			Manufacturer: "Acme",
			Model:        "SimCam-100",
			Serial:       "SN-TMPL",
		},
		Network: config.NetworkConfig{HTTPPort: 18080, RTSPPort: 18554},
		Media: config.MediaConfig{
			Profiles: []config.ProfileConfig{{
				Name:  "main",
				Token: "profile_main",
				Kind:  config.ProfileKindGo264,
				Go264: &config.Go264Config{
					Width: 320, Height: 180, FPS: 10, QP: 28, GOPSize: 10,
					Mode: config.Go264ModeLoop, LoopSeconds: 2,
				},
			}},
		},
		Auth: config.AuthConfig{
			Enabled: true,
			Users:   []config.UserConfig{{Username: "admin", Password: "admin", Role: config.RoleAdministrator}},
		},
	}
}

func TestValidatePortRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		start   int
		count   int
		wantEnd int
		wantErr bool
	}{
		{name: "single", start: 18080, count: 1, wantEnd: 18080},
		{name: "block", start: 18080, count: 12, wantEnd: 18091},
		{name: "zero start", start: 0, count: 1, wantErr: true},
		{name: "negative start", start: -1, count: 2, wantErr: true},
		{name: "overflows max port", start: 65530, count: 10, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validatePortRange("http", tc.start, tc.count)
			if tc.wantErr {
				if !errors.Is(err, errFarmPortRange) {
					t.Fatalf("expected errFarmPortRange, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantEnd {
				t.Fatalf("last port = %d, want %d", got, tc.wantEnd)
			}
		})
	}
}

// TestWriteInstanceConfigsDerivesPerInstanceIdentity covers the invariants a
// 10+ camera farm relies on: unique UUIDs, suffixed serials/tokens, distinct
// ports, no multicast discovery, a per-instance control-port file, and a
// distinct go264 seed so the wall does not show 12 identical pictures.
func TestWriteInstanceConfigsDerivesPerInstanceIdentity(t *testing.T) {
	t.Parallel()

	tmpl := farmTestTemplate()
	f := &farm{
		template: tmpl,
		outDir:   t.TempDir(),
		host:     "127.0.0.1",
	}
	f.manifest.Count = 2

	httpPort := func(i int) int { return 18080 + i }
	rtspPort := func(i int) int { return 18554 + i }
	if err := f.writeInstanceConfigs(2, httpPort, rtspPort, t.TempDir()); err != nil {
		t.Fatalf("writeInstanceConfigs: %v", err)
	}
	if len(f.manifest.Instances) != 2 {
		t.Fatalf("manifest instances = %d, want 2", len(f.manifest.Instances))
	}

	seenUUID := map[string]bool{}
	seenSeed := map[int]bool{}
	for i := 0; i < 2; i++ {
		inst := f.manifest.Instances[i]
		if inst.UUID == "" || seenUUID[inst.UUID] {
			t.Fatalf("instance %d has empty or duplicate UUID %q", i, inst.UUID)
		}
		seenUUID[inst.UUID] = true
		if inst.HTTPPort != 18080+i || inst.RTSPPort != 18554+i {
			t.Fatalf("instance %d ports = %d/%d, want %d/%d",
				i, inst.HTTPPort, inst.RTSPPort, 18080+i, 18554+i)
		}
		if inst.ControlPortFile == "" {
			t.Fatalf("instance %d has no control-port file", i)
		}

		data, err := os.ReadFile(inst.ConfigPath)
		if err != nil {
			t.Fatalf("read instance %d config: %v", i, err)
		}
		var got config.Config
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("parse instance %d config: %v", i, err)
		}
		if got.Device.Serial != "SN-TMPL-"+strconv.Itoa(i) {
			t.Fatalf("instance %d serial = %q", i, got.Device.Serial)
		}
		if got.Runtime.DiscoveryMode != farmDiscoveryMode {
			t.Fatalf("instance %d discovery = %q, want %q", i, got.Runtime.DiscoveryMode, farmDiscoveryMode)
		}
		p := got.Media.Profiles[0]
		if p.Token != "profile_main-"+strconv.Itoa(i) {
			t.Fatalf("instance %d token = %q", i, p.Token)
		}
		if p.Go264 == nil || p.Go264.Seed != i+1 {
			t.Fatalf("instance %d go264 seed = %+v, want %d", i, p.Go264, i+1)
		}
		seenSeed[p.Go264.Seed] = true
	}
	if len(seenSeed) != 2 {
		t.Fatalf("seeds = %v, want two distinct values", seenSeed)
	}
}

// TestCloneConfigIsDeepCopy guards the derivation against aliasing the
// template's nested slices/pointers, which would make instance 0's edits leak
// into every other instance.
func TestCloneConfigIsDeepCopy(t *testing.T) {
	t.Parallel()

	tmpl := farmTestTemplate()
	clone, err := cloneConfig(&tmpl)
	if err != nil {
		t.Fatalf("cloneConfig: %v", err)
	}
	clone.Media.Profiles[0].Token = "changed"
	if tmpl.Media.Profiles[0].Token == "changed" {
		t.Fatal("clone shares the template's profile slice")
	}
	if clone.Media.Profiles[0].Go264 == tmpl.Media.Profiles[0].Go264 {
		t.Fatal("clone shares the template's Go264 pointer")
	}
}

// TestFarmManifestControlPortFile verifies the manifest carries the written
// path (the control port itself is filled in after the child starts).
func TestFarmManifestControlPortFile(t *testing.T) {
	t.Parallel()

	tmpl := farmTestTemplate()
	dir := t.TempDir()
	f := &farm{template: tmpl, outDir: dir, host: "127.0.0.1"}
	f.manifest.Count = 1
	if err := f.writeInstanceConfigs(1, func(int) int { return 18080 }, func(int) int { return 18554 }, dir); err != nil {
		t.Fatalf("writeInstanceConfigs: %v", err)
	}
	want := filepath.Join(dir, "instance-0.ctl")
	if got := f.manifest.Instances[0].ControlPortFile; got != want {
		t.Fatalf("control port file = %q, want %q", got, want)
	}
}

// TestListClips checks the stable ordering and the .mp4 filter (case
// insensitive, directories ignored).
func TestListClips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"b.mp4", "A.MP4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.mp4"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	clips, err := listClips(dir)
	if err != nil {
		t.Fatalf("listClips: %v", err)
	}
	if len(clips) != 2 {
		t.Fatalf("clips = %v, want 2 entries", clips)
	}
	if filepath.Base(clips[0]) != "A.MP4" || filepath.Base(clips[1]) != "b.mp4" {
		t.Fatalf("clip order = %v, want [A.MP4 b.mp4]", clips)
	}
	if !filepath.IsAbs(clips[0]) {
		t.Fatalf("clip %q is not absolute", clips[0])
	}

	empty := t.TempDir()
	if got, err := listClips(empty); err != nil || len(got) != 0 {
		t.Fatalf("empty dir: clips=%v err=%v", got, err)
	}
}

// TestWriteInstanceConfigsAssignsClips verifies -clips round-robins over the
// sorted clip list, switches every profile to a file source (clearing any
// go264/rpicam block), and records the assignment in the manifest.
func TestWriteInstanceConfigsAssignsClips(t *testing.T) {
	t.Parallel()

	tmpl := farmTestTemplate()
	dir := t.TempDir()
	clipA := filepath.Join(dir, "a.mp4")
	clipB := filepath.Join(dir, "b.mp4")
	clips := []string{clipA, clipB}

	f := &farm{template: tmpl, outDir: dir, host: "127.0.0.1", clips: clips}
	f.manifest.Count = 3
	if err := f.writeInstanceConfigs(3,
		func(i int) int { return 18080 + i },
		func(i int) int { return 18554 + i },
		dir,
	); err != nil {
		t.Fatalf("writeInstanceConfigs: %v", err)
	}

	want := []string{clipA, clipB, clipA}
	for i := 0; i < 3; i++ {
		inst := f.manifest.Instances[i]
		if inst.Clip != want[i] {
			t.Fatalf("instance %d clip = %q, want %q", i, inst.Clip, want[i])
		}
		data, err := os.ReadFile(inst.ConfigPath)
		if err != nil {
			t.Fatalf("read instance %d: %v", i, err)
		}
		var got config.Config
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("parse instance %d: %v", i, err)
		}
		prof := got.Media.Profiles[0]
		if prof.Kind != config.ProfileKindFile {
			t.Fatalf("instance %d kind = %q, want file", i, prof.Kind)
		}
		if prof.MediaFilePath != want[i] {
			t.Fatalf("instance %d media_file_path = %q, want %q", i, prof.MediaFilePath, want[i])
		}
		if prof.Go264 != nil || prof.RPICam != nil {
			t.Fatalf("instance %d kept a synthetic/camera source block", i)
		}
	}
}

// TestFarmStopRequiresManifest locks in the usage error instead of a confusing
// "open : no such file" when the operator forgets -manifest/-out.
func TestFarmStopRequiresManifest(t *testing.T) {
	t.Parallel()

	if err := runFarmStop(nil); !errors.Is(err, errFarmManifestRequired) {
		t.Fatalf("expected errFarmManifestRequired, got %v", err)
	}
	if err := runFarmStop([]string{"-out", ""}); !errors.Is(err, errFarmManifestRequired) {
		t.Fatalf("expected errFarmManifestRequired for empty -out, got %v", err)
	}
}
