package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/GyeongHoKim/onvif-simulator/internal/config"
)

// farmRestartBackoff is the delay between crash-restart attempts for a child
// that exited unexpectedly, to avoid a tight respawn loop when a child is
// fundamentally broken. farmDialTimeout and farmPollInterval tune the
// readiness probe that runs once per instance before the farm is declared up.
const (
	farmRestartBackoff = 500 * time.Millisecond
	farmDialTimeout    = 200 * time.Millisecond
	farmPollInterval   = 100 * time.Millisecond

	// farmStableUptime is how long a child must run before an exit stops
	// counting toward farmMaxRestarts. Without it a healthy child that
	// exits once after hours would still accumulate failure counts.
	farmStableUptime = 30 * time.Second
	// farmMaxRestarts caps consecutive rapid restarts per slot so a
	// fundamentally broken instance config does not respawn forever.
	farmMaxRestarts = 5
	// farmMaxPort is the highest valid TCP port.
	farmMaxPort = 65535
	// farmShutdownTimeout bounds how long shutdown waits for children to be
	// reaped after SIGKILL, so a wedged child cannot hang `farm` forever.
	farmShutdownTimeout = 5 * time.Second
	// farmDiscoveryMode forces manual-add mode. aiovms reaches each
	// instance by IP:port, and N processes sharing UDP 3702 on one host
	// is unreliable (Windows in particular), so multicast discovery is
	// disabled for every derived instance.
	farmDiscoveryMode = "NonDiscoverable"
	// defaultFarmReadyTimeout is the -ready-timeout default: long enough
	// for a cold Go start plus config load, short enough that a broken
	// template fails the farm in seconds rather than minutes.
	defaultFarmReadyTimeout = 10 * time.Second
)

var (
	// errFarmCountInvalid means -count was omitted or non-positive.
	errFarmCountInvalid = errors.New("farm: -count must be > 0")
	// errFarmNotReady means an instance's HTTP port did not accept a
	// connection within the readiness timeout.
	errFarmNotReady = errors.New("farm: instance not ready")
	// errFarmPortRange means the derived port block falls outside 1..65535.
	errFarmPortRange = errors.New("farm: derived port range is invalid")
	// errFarmPortOverlap means the HTTP and RTSP blocks collide; config
	// validation rejects any instance whose http_port equals rtsp_port.
	errFarmPortOverlap = errors.New("farm: http and rtsp port ranges overlap")
	// errFarmRestartLimit means a slot exceeded farmMaxRestarts and was
	// given up on.
	errFarmRestartLimit = errors.New("farm: restart limit exceeded")
	// errFarmNoReady means not a single instance accepted a connection
	// within -ready-timeout, which is always a template/port problem rather
	// than a slow start.
	errFarmNoReady = errors.New("farm: no instance became ready")
	// errFarmNoClips means -clips pointed at a directory without any .mp4.
	errFarmNoClips = errors.New("farm: -clips directory contains no .mp4 files")
	// errFarmManifestRequired means `farm stop` was not told where the
	// manifest is.
	errFarmManifestRequired = errors.New("farm stop: -manifest or -out is required")
	// errFarmParentGone means a child checked its recorded farm supervisor
	// pid and found a different parent, i.e. the farm was already killed.
	errFarmParentGone = errors.New("serve: farm parent pid is gone")
)

// farmParentPIDEnv carries the supervisor pid into every child. serve checks
// it once at start-up: if the farm already died, the child exits instead of
// running unsupervised. That closes the tiny fork→prctl window in which Linux
// Pdeathsig could be missed.
const farmParentPIDEnv = "ONVIF_SIMULATOR_PARENT_PID"

// checkFarmParent makes a child exit when the farm supervisor is already gone.
// It is a no-op outside a farm (env unset). On Unix it closes the tiny
// fork→prctl window in which Pdeathsig could be missed. On Windows the parent
// pid recorded at process creation never changes, so a dead parent cannot be
// detected this way — the job object owns the orphan guarantee there — but
// the check still rejects a stale or forged env value.
func checkFarmParent() error {
	raw := strings.TrimSpace(os.Getenv(farmParentPIDEnv))
	if raw == "" {
		return nil
	}
	// Written as "only act when the value parses and is positive" rather than
	// "bail out when it does not": an unparsable or non-positive value means
	// this process was not launched by a farm, which is not an error.
	if want, err := strconv.Atoi(raw); err == nil && want > 0 {
		if os.Getppid() != want {
			return fmt.Errorf("%w: expected %d, got %d", errFarmParentGone, want, os.Getppid())
		}
	}
	return nil
}

// farmManifest is the JSON document farm writes once every instance is up (and
// keeps updated on restart). It is the single source of truth a test harness
// can read to learn each simulated device's endpoints.
type farmManifest struct {
	Count  int    `json:"count"`
	OutDir string `json:"out_dir"`
	// ParentPID is the farm supervisor process. `farm stop` terminates it
	// first, so a live farm cannot respawn the children it is about to kill
	// (and on Windows the job object takes the children with it).
	ParentPID int `json:"parent_pid"`
	// ParentExe/ParentStartedNS fingerprint the supervisor so a stale
	// manifest cannot kill an unrelated process that inherited its pid.
	ParentExe       string         `json:"parent_exe,omitempty"`
	ParentStartedNS int64          `json:"parent_started_ns,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	Instances       []farmInstance `json:"instances"`
}

// farmInstance records one simulated device's derived identity and endpoints.
type farmInstance struct {
	Index      int    `json:"index"`
	UUID       string `json:"uuid"`
	Serial     string `json:"serial"`
	HTTPPort   int    `json:"http_port"`
	RTSPPort   int    `json:"rtsp_port"`
	HTTPURL    string `json:"http_url"`
	RTSPURL    string `json:"rtsp_url"`
	ConfigPath string `json:"config_path"`
	LogPath    string `json:"log_path"`
	// OutPath holds the child's stdout/stderr (startup banner, event and
	// mutation lines). It is a separate file from LogPath because the
	// child opens both, and two descriptors appending to one file interleave
	// on Windows.
	OutPath string `json:"out_log_path"`
	// ControlPortFile is the per-instance control-port file passed to the
	// child via -control-port-file. It is per-instance on purpose: the
	// default ~/.onvif-simulator/control.port would make N children
	// overwrite each other, so `event` could never target one camera.
	ControlPortFile string `json:"control_port_file"`
	// ControlPort is read back from ControlPortFile after readiness. It is
	// 0 when the file did not appear; the file path stays authoritative
	// because a restarted child binds a fresh random port.
	ControlPort int `json:"control_port"`
	// Clip is the media file assigned to this instance by -clips, empty when
	// the template's own source configuration was kept.
	Clip string `json:"clip,omitempty"`
	// Exe/StartedNS fingerprint the child process (executable path and
	// creation time) so `farm stop` can refuse to signal a reused pid.
	Exe       string `json:"exe,omitempty"`
	StartedNS int64  `json:"started_ns,omitempty"`
	PID       int    `json:"pid"`
}

// farm wires up the test-farm supervisor: it derives N distinct simulator
// configs from a template, spawns one `serve` process per instance, polls for
// TCP readiness, and keeps each child alive (restarting on unexpected exit)
// until the process is asked to stop (SIGINT/SIGTERM, or Ctrl+C on Windows —
// an external SIGTERM cannot reach a background Go process there).
type farm struct {
	template     config.Config
	host         string
	outDir       string
	restart      bool
	readyTimeout time.Duration
	logLevel     string

	mu       sync.Mutex
	cmds     map[int]*exec.Cmd
	files    map[int]*farmLogFiles
	started  map[int]time.Time
	manifest farmManifest
	shutdown bool

	// job is the Windows kill-on-close job object. Nil on other platforms.
	job *farmJob
	// clips is the sorted list of -clips media files assigned round-robin.
	clips []string

	// supervisors tracks the per-slot restart goroutines so shutdown can
	// wait for every child to be reaped before the process exits (and
	// before a temp -out directory is removed).
	supervisors sync.WaitGroup
}

// farmLogFiles owns the descriptor the parent needs: out receives the
// child's stdout/stderr. The structured slog file is opened by the child
// itself via -log-file, so the parent must not also hold a handle.
type farmLogFiles struct {
	out *os.File
}

// runFarm dispatches the `farm` subcommands: `farm stop ...` terminates a
// running (or crashed) farm from its manifest, everything else starts one.
func runFarm(args []string) error {
	if len(args) > 0 && args[0] == "stop" {
		return runFarmStop(args[1:])
	}
	return runFarmStart(args)
}

// farmOptions is the parsed `farm start` command line.
type farmOptions struct {
	cfgPath      string
	count        int
	httpBase     int
	rtspBase     int
	outDir       string
	readyTimeout time.Duration
	logLevel     string
	restart      bool
	host         string
	clipsDir     string
}

// farmPlan is everything farm derives from the template and flags before any
// child process exists. Keeping it separate from the flags makes the
// validation that must happen before a single port is bound explicit.
type farmPlan struct {
	tmpl       config.Config
	resolved   string
	dir        string
	cleanupDir string
	hostName   string
	httpStart  int
	httpEnd    int
	rtspStart  int
	rtspEnd    int
	clips      []string
}

// resolveClips turns -clips into the sorted absolute paths assigned
// round-robin to instances, overriding each template's media source (a go264
// template becomes a file profile backed by the assigned clip).
func resolveClips(dir string) ([]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	clips, err := listClips(dir)
	if err != nil {
		return nil, err
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("%w: %s", errFarmNoClips, dir)
	}
	return clips, nil
}

// makeOutDir creates the output directory, returning it plus the directory to
// remove on exit (empty when -out named one, since the operator owns it then).
func makeOutDir(dir string) (outDir, cleanupDir string, err error) {
	if dir != "" {
		if mkErr := os.MkdirAll(dir, dirMode); mkErr != nil {
			return "", "", fmt.Errorf("farm: create out dir: %w", mkErr)
		}
		return dir, "", nil
	}
	tmp, mkErr := os.MkdirTemp("", "onvif-farm-")
	if mkErr != nil {
		return "", "", fmt.Errorf("farm: create temp out dir: %w", mkErr)
	}
	return tmp, tmp, nil
}

// planFarm loads and validates the template, resolves clips, validates the two
// port blocks, and creates the output directory. Everything that can fail
// happens here, so a failure never leaves half a farm behind.
func planFarm(opts *farmOptions) (*farmPlan, error) {
	resolved, err := config.Resolve(opts.cfgPath)
	if err != nil {
		return nil, err
	}
	if _, ensureErr := config.EnsureExists(resolved); ensureErr != nil {
		return nil, ensureErr
	}
	config.SetPath(resolved)

	tmpl, err := config.Load()
	if err != nil {
		return nil, err
	}
	// Validate once up front: deriving N configs from an invalid template
	// would start N children that all exit immediately, leaving the operator
	// with "N launched, none ready" and a restart loop instead of one clear
	// error.
	if valErr := config.Validate(&tmpl); valErr != nil {
		return nil, fmt.Errorf("farm: template config is invalid: %w", valErr)
	}

	clips, err := resolveClips(opts.clipsDir)
	if err != nil {
		return nil, err
	}

	// Resolve and validate the port blocks before creating anything on disk:
	// an overlapping or out-of-range block makes some instances fail config
	// validation on startup, which would look like "farm launched N but only
	// M came up".
	httpStart := tmpl.Network.HTTPPort
	if opts.httpBase > 0 {
		httpStart = opts.httpBase
	}
	rtspStart := tmpl.Network.RTSPPortOrDefault()
	if opts.rtspBase > 0 {
		rtspStart = opts.rtspBase
	}
	httpEnd, err := validatePortRange("http", httpStart, opts.count)
	if err != nil {
		return nil, err
	}
	rtspEnd, err := validatePortRange("rtsp", rtspStart, opts.count)
	if err != nil {
		return nil, err
	}
	if httpStart <= rtspEnd && rtspStart <= httpEnd {
		return nil, fmt.Errorf("%w: http %d..%d, rtsp %d..%d",
			errFarmPortOverlap, httpStart, httpEnd, rtspStart, rtspEnd)
	}

	dir, cleanupDir, err := makeOutDir(opts.outDir)
	if err != nil {
		return nil, err
	}

	hostName := strings.TrimSpace(opts.host)
	if hostName == "" {
		hostName = detectFarmHost()
	}

	return &farmPlan{
		tmpl:       tmpl,
		resolved:   resolved,
		dir:        dir,
		cleanupDir: cleanupDir,
		hostName:   hostName,
		httpStart:  httpStart,
		httpEnd:    httpEnd,
		rtspStart:  rtspStart,
		rtspEnd:    rtspEnd,
		clips:      clips,
	}, nil
}

// newFarm builds the supervisor state from an already-validated plan.
func newFarm(opts *farmOptions, plan *farmPlan) *farm {
	f := &farm{
		template:     plan.tmpl,
		host:         plan.hostName,
		outDir:       plan.dir,
		restart:      opts.restart,
		readyTimeout: opts.readyTimeout,
		logLevel:     opts.logLevel,
		cmds:         make(map[int]*exec.Cmd),
		files:        make(map[int]*farmLogFiles),
		started:      make(map[int]time.Time),
		clips:        plan.clips,
		manifest: farmManifest{
			Count:     opts.count,
			OutDir:    plan.dir,
			ParentPID: os.Getpid(),
			StartedAt: time.Now(),
		},
	}
	// Fingerprint ourselves so `farm stop` can refuse to signal a pid that a
	// later process reused. Unsupported platforms leave the fields empty and
	// stop degrades to pid-only termination.
	if id, idErr := processIdentity(os.Getpid()); idErr == nil {
		f.manifest.ParentExe = id.Exe
		f.manifest.ParentStartedNS = id.StartNS
	}
	return f
}

// attachJob puts every child in a Windows kill-on-close job object so a
// crashed or force-killed farm cannot leave orphaned cameras holding ports.
// Failure to create the job is not fatal (sandboxed/CI environments may
// forbid nested jobs); `farm stop` still covers cleanup.
func (f *farm) attachJob() func() {
	job, jobErr := newFarmJob()
	if jobErr != nil {
		fmt.Fprintf(os.Stderr, "warning: job object unavailable, children may outlive a hard kill: %v\n", jobErr)
		return func() {}
	}
	f.job = job
	return job.Close
}

// launchAll writes one config per instance and starts every child. Any spawn
// failure tears the whole farm down: a partial farm is more confusing than a
// clear error.
func (f *farm) launchAll(count int, httpPortFor, rtspPortFor func(int) int, tmplDir string) error {
	// Derive and write the configs up front so a crash during startup still
	// leaves a complete, inspectable artifact set.
	if err := f.writeInstanceConfigs(count, httpPortFor, rtspPortFor, tmplDir); err != nil {
		return err
	}
	for i := range count {
		if err := f.spawn(i); err != nil {
			// Best-effort cleanup of already-spawned children; no supervisor
			// goroutines exist yet, so reap them here or they become zombies.
			f.beginShutdown()
			f.killAllAndReap(farmShutdownTimeout)
			return fmt.Errorf("farm: spawn instance %d: %w", i, err)
		}
	}
	return nil
}

// awaitReady polls readiness for every instance before declaring the farm up.
// Zero ready instances is fatal; a straggler is only a warning.
func (f *farm) awaitReady(count int) error {
	ready, readyErr := f.waitReady(count)
	if ready == 0 {
		f.beginShutdown()
		f.killAllAndReap(farmShutdownTimeout)
		return fmt.Errorf("%w: %w", errFarmNoReady, readyErr)
	}
	if readyErr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", readyErr)
	}
	return nil
}

// announce prints where the artifacts are. Always printed, including for the
// implicit temp dir: a test harness needs the manifest path while the farm is
// running, and the directory is only deleted on exit.
func (f *farm) announce(plan *farmPlan) {
	manifestPath := filepath.Join(plan.dir, "manifest.json")
	if plan.cleanupDir != "" {
		fmt.Printf("farm: launched %d instances in temp dir %s (removed on exit)\n", f.manifest.Count, plan.dir)
	} else {
		fmt.Printf("farm: launched %d instances in %s\n", f.manifest.Count, plan.dir)
	}
	fmt.Printf("farm: manifest at %s\n", manifestPath)
	fmt.Printf("farm: devices at %s, http ports %d..%d, rtsp ports %d..%d\n",
		plan.hostName, plan.httpStart, plan.httpEnd, plan.rtspStart, plan.rtspEnd)
}

func runFarmStart(args []string) error {
	fs := flag.NewFlagSet("farm", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "template config (defaults to the user config dir)")
	count := fs.Int("count", 0, "number of simulated devices to launch (required, >0)")
	httpBase := fs.Int("http-base", 0,
		"starting HTTP port; instance i uses http-base+i (default: template http_port)")
	rtspBase := fs.Int("rtsp-base", 0,
		"starting RTSP port; instance i uses rtsp-base+i (default: template rtsp_port)")
	outDir := fs.String("out", "",
		"directory for per-instance configs, logs, and manifest.json (default: a temp dir)")
	readyTimeout := fs.Duration("ready-timeout", defaultFarmReadyTimeout,
		"max time to wait per instance for its HTTP port to accept connections")
	logLevel := fs.String("log-level", "info", "log level passed to each child serve process")
	noRestart := fs.Bool("no-restart", false, "do not restart children that exit unexpectedly")
	host := fs.String("host", "",
		"host advertised in manifest URLs (default: first non-loopback IPv4, else 127.0.0.1)")
	clipsDir := fs.String("clips", "",
		"directory of .mp4 clips; instance i uses the i-th file (sorted) as every profile's media_file_path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// -count is the one flag with no usable default.
	if *count <= 0 {
		return errFarmCountInvalid
	}
	opts := &farmOptions{
		cfgPath:      *cfgPath,
		count:        *count,
		httpBase:     *httpBase,
		rtspBase:     *rtspBase,
		outDir:       *outDir,
		readyTimeout: *readyTimeout,
		logLevel:     *logLevel,
		restart:      !*noRestart,
		host:         *host,
		clipsDir:     *clipsDir,
	}
	plan, err := planFarm(opts)
	if err != nil {
		return err
	}
	if plan.cleanupDir != "" {
		// A temp dir is scratch space; it is kept only when -out names one.
		defer func() {
			_ = os.RemoveAll(plan.cleanupDir) //nolint:errcheck // best-effort cleanup.
		}()
	}

	f := newFarm(opts, plan)
	defer f.attachJob()()

	httpPortFor := func(i int) int { return plan.httpStart + i }
	rtspPortFor := func(i int) int { return plan.rtspStart + i }
	if err := f.launchAll(opts.count, httpPortFor, rtspPortFor, filepath.Dir(plan.resolved)); err != nil {
		return err
	}
	if err := f.awaitReady(opts.count); err != nil {
		return err
	}

	f.refreshControlPorts(opts.count)
	if err := f.writeManifest(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write manifest: %v\n", err)
	}
	f.announce(plan)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// Supervisor: one goroutine per slot restarts its child on unexpected
	// exit. The parent loop blocks on the signal channel and tears down on
	// SIGINT/SIGTERM.
	for i := range opts.count {
		f.supervisors.Add(1)
		go func(slot int) {
			defer f.supervisors.Done()
			f.supervise(slot)
		}(i)
	}

	<-sigCh
	fmt.Println("\nfarm: shutting down")
	f.beginShutdown()
	f.killAll()
	f.waitChildren(farmShutdownTimeout)
	return nil
}

// waitChildren blocks until every supervise goroutine has observed its child
// exit and released its log descriptors, bounded by timeout. Callers must
// have called killAll first. Waiting matters on Windows: removing a temp -out
// directory while a child still holds instance-<i>.out.log fails silently, and
// leaving the process before the children are reaped can orphan them.
func (f *farm) waitChildren(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		f.supervisors.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Fprintln(os.Stderr, "warning: timed out waiting for child processes to exit")
	}
}

// writeInstanceConfigs deep-copies the template (JSON round-trip) for each
// instance, derives a distinct device identity, network ports, and profile
// tokens, and writes the result to <out>/instance-<i>.json.
func (f *farm) writeInstanceConfigs(count int, httpPortFor, rtspPortFor func(int) int, tmplDir string) error {
	for i := range count {
		inst, assignedClip, err := f.deriveInstance(i, httpPortFor, rtspPortFor, tmplDir)
		if err != nil {
			return err
		}
		// Validate the derived instance, not just the template: the port
		// rewrite, relative-path resolution, and seed assignment are all
		// farm-owned mutations, and failing here names the offending slot
		// instead of leaving N children to die at startup.
		if valErr := config.Validate(&inst); valErr != nil {
			return fmt.Errorf("farm: instance %d config is invalid: %w", i, valErr)
		}

		path := filepath.Join(f.outDir, fmt.Sprintf("instance-%d.json", i))
		data, err := json.MarshalIndent(inst, "", "  ")
		if err != nil {
			return fmt.Errorf("farm: marshal instance %d: %w", i, err)
		}
		if err := os.WriteFile(path, data, fileMode); err != nil {
			return fmt.Errorf("farm: write instance %d config: %w", i, err)
		}

		f.manifest.Instances = append(f.manifest.Instances, farmInstance{
			Index:           i,
			UUID:            inst.Device.UUID,
			Serial:          inst.Device.Serial,
			HTTPPort:        inst.Network.HTTPPort,
			RTSPPort:        inst.Network.RTSPPortOrDefault(),
			HTTPURL:         "http://" + net.JoinHostPort(f.host, strconv.Itoa(inst.Network.HTTPPort)),
			RTSPURL:         "rtsp://" + net.JoinHostPort(f.host, strconv.Itoa(inst.Network.RTSPPortOrDefault())),
			ConfigPath:      path,
			LogPath:         filepath.Join(f.outDir, fmt.Sprintf("instance-%d.log", i)),
			OutPath:         filepath.Join(f.outDir, fmt.Sprintf("instance-%d.out.log", i)),
			ControlPortFile: filepath.Join(f.outDir, fmt.Sprintf("instance-%d.ctl", i)),
			Clip:            assignedClip,
		})
	}
	return nil
}

// deriveInstance deep-copies the template and applies every per-instance
// mutation: identity, ports, advertised endpoints, protocol table, discovery
// mode, clip assignment, and profile tokens/seeds. It returns the config plus
// the clip it assigned (empty when the template's own source was kept).
func (f *farm) deriveInstance(
	i int, httpPortFor, rtspPortFor func(int) int, tmplDir string,
) (config.Config, string, error) {
	inst, err := cloneConfig(&f.template)
	if err != nil {
		return config.Config{}, "", fmt.Errorf("farm: clone template for instance %d: %w", i, err)
	}
	inst.Device.UUID = "urn:uuid:" + uuid.NewString()
	if inst.Device.Serial != "" {
		inst.Device.Serial = inst.Device.Serial + "-" + strconv.Itoa(i)
	} else {
		inst.Device.Serial = "SN-" + strconv.Itoa(i)
	}
	inst.Network.HTTPPort = httpPortFor(i)
	inst.Network.RTSPPort = rtspPortFor(i)

	rewriteXAddrs(&inst)
	syncNetworkProtocols(&inst)

	// aiovms adds devices by IP:port, so multicast discovery is not needed;
	// several processes sharing UDP 3702 on one host would make startup
	// non-deterministic.
	inst.Runtime.DiscoveryMode = farmDiscoveryMode

	// -clips overrides the template's source for every profile of this
	// instance: each camera becomes a file-backed stream of its assigned
	// clip, so a 12-up wall shows 12 different pictures at ~0 CPU (the
	// looper only packetizes, it never encodes).
	assignedClip := ""
	if len(f.clips) > 0 {
		assignedClip = f.clips[i%len(f.clips)]
	}
	configureProfiles(&inst, i, assignedClip, tmplDir)
	return inst, assignedClip, nil
}

// rewriteXAddrs points every advertised endpoint at this instance's HTTP port.
// A template that pins network.xaddrs would otherwise make all instances claim
// the same endpoint, so aiovms would discover only one of them. The host the
// operator chose is kept; only the port is rewritten.
func rewriteXAddrs(inst *config.Config) {
	for k, raw := range inst.Network.XAddrs {
		u, perr := url.Parse(strings.TrimSpace(raw))
		if perr != nil || u.Host == "" {
			continue
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(inst.Network.HTTPPort))
		inst.Network.XAddrs[k] = u.String()
	}
}

// syncNetworkProtocols keeps the ONVIF protocol table consistent with the
// ports this instance actually listens on; otherwise GetNetworkProtocols
// reports the template's ports for every device.
func syncNetworkProtocols(inst *config.Config) {
	for k := range inst.Runtime.NetworkProtocols {
		switch strings.ToUpper(strings.TrimSpace(inst.Runtime.NetworkProtocols[k].Name)) {
		case "HTTP":
			inst.Runtime.NetworkProtocols[k].Port = []int{inst.Network.HTTPPort}
		case "RTSP":
			inst.Runtime.NetworkProtocols[k].Port = []int{inst.Network.RTSPPortOrDefault()}
		}
	}
}

// configureProfiles applies the per-profile mutations for one instance: the
// -clips override or relative-path resolution, unique token/name suffixes, and
// a distinct go264 seed so the wall does not show N copies of one camera.
func configureProfiles(inst *config.Config, i int, assignedClip, tmplDir string) {
	for p := range inst.Media.Profiles {
		switch {
		case assignedClip != "":
			inst.Media.Profiles[p].Kind = config.ProfileKindFile
			inst.Media.Profiles[p].MediaFilePath = assignedClip
			inst.Media.Profiles[p].RPICam = nil
			inst.Media.Profiles[p].Go264 = nil
		default:
			// Make any relative media file path absolute against the
			// template's directory so children (which may run from a
			// different working directory) resolve it correctly regardless of
			// the instance config location. Empty paths (embedded/go264
			// sources) are left alone.
			path := inst.Media.Profiles[p].MediaFilePath
			if path != "" && !filepath.IsAbs(path) {
				inst.Media.Profiles[p].MediaFilePath = filepath.Join(tmplDir, path)
			}
		}
		inst.Media.Profiles[p].Token = inst.Media.Profiles[p].Token + "-" + strconv.Itoa(i)
		inst.Media.Profiles[p].Name = inst.Media.Profiles[p].Name + " " + strconv.Itoa(i)
		// A template that sets seed explicitly wins.
		if g := inst.Media.Profiles[p].Go264; g != nil && g.Seed == 0 {
			g.Seed = i + 1
		}
	}
}

// listClips returns the absolute paths of every .mp4 in dir, sorted by name
// (case-insensitive) so instance assignment is stable across runs.
func listClips(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("farm: resolve -clips: %w", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("farm: read -clips %s: %w", abs, err)
	}
	clips := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
			continue
		}
		clips = append(clips, filepath.Join(abs, entry.Name()))
	}
	sort.Slice(clips, func(i, j int) bool {
		return strings.ToLower(clips[i]) < strings.ToLower(clips[j])
	})
	return clips, nil
}

// cloneConfig deep-copies a Config via a JSON round-trip. The Config type is
// fully serializable, so this is the simplest correct copy.
func cloneConfig(c *config.Config) (config.Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return config.Config{}, err
	}
	var out config.Config
	if err := json.Unmarshal(data, &out); err != nil {
		return config.Config{}, err
	}
	return out, nil
}

// spawn launches one `serve` child for instance i and records its process.
// It (re)uses the instance config written by writeInstanceConfigs and points
// the child's structured log at the instance log file.
func (f *farm) spawn(i int) error {
	inst := f.manifest.Instances[i]
	outFile, err := os.OpenFile(inst.OutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open output log %s: %w", inst.OutPath, err)
	}

	// Close the stdout/stderr file of a previous incarnation of this slot.
	f.mu.Lock()
	if prev := f.files[i]; prev != nil {
		_ = prev.out.Close() //nolint:errcheck // best-effort teardown.
	}
	f.files[i] = &farmLogFiles{out: outFile}
	f.started[i] = time.Now()
	f.mu.Unlock()

	self, err := os.Executable()
	if err != nil {
		_ = outFile.Close() //nolint:errcheck // teardown.
		return fmt.Errorf("resolve self executable: %w", err)
	}

	// self is the herd's own executable and every argument is derived from
	// farm-controlled state, so re-executing it is intentional and safe. The
	// structured log (-log-file) is opened by the child itself; the parent
	// only owns the stdout/stderr redirect. -control-port-file keeps each
	// instance's loopback control port in its own file instead of the
	// per-user default that all children would otherwise overwrite.
	// The context is only a handle for the linter's benefit: child lifetime is
	// owned by the supervisor, which terminates the process explicitly.
	cmd := exec.CommandContext(context.Background(), self, "serve", //nolint:gosec // self path comes from os.Executable.
		"-config", inst.ConfigPath,
		"-log-level", f.logLevel,
		"-log-file", inst.LogPath,
		"-control-port-file", inst.ControlPortFile,
	)
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	// The child checks this pid at start-up and exits if the farm is already
	// gone, covering a Pdeathsig signal missed in the fork window on Linux.
	cmd.Env = append(os.Environ(), farmParentPIDEnv+"="+strconv.Itoa(os.Getpid()))
	// On Unix, place the child in its own process group so a group kill
	// reclaims any grandchildren it might spawn; on Linux it additionally gets
	// Pdeathsig. On Windows this is a no-op; the job object owns that.
	applyChildProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = outFile.Close() //nolint:errcheck // teardown.
		return fmt.Errorf("start child: %w", err)
	}

	// As soon as the pid exists, bind it to the kill-on-close job so a farm
	// crash cannot orphan it. Assignment failure is not fatal: the child runs,
	// it just loses the crash-safety net (warned once per slot).
	if assignErr := f.job.assign(cmd.Process.Pid); assignErr != nil {
		fmt.Fprintf(os.Stderr, "farm: warning: instance %d not in job object: %v\n", i, assignErr)
	}

	f.mu.Lock()
	f.cmds[i] = cmd
	f.manifest.Instances[i].PID = cmd.Process.Pid
	f.mu.Unlock()

	// Fingerprint the child for `farm stop`. A failure here only means stop
	// later falls back to pid-only for this slot, so it is not fatal.
	if id, idErr := processIdentity(cmd.Process.Pid); idErr == nil {
		f.mu.Lock()
		f.manifest.Instances[i].Exe = id.Exe
		f.manifest.Instances[i].StartedNS = id.StartNS
		f.mu.Unlock()
	}
	return nil
}

// supervise is the per-slot restart loop. It waits for the current child to
// exit; if the exit was unexpected and restarting is enabled, it respawns
// after a short backoff. The loop ends when the farm begins shutting down.
func (f *farm) supervise(i int) {
	restarts := 0
	for {
		f.mu.Lock()
		cmd := f.cmds[i]
		f.mu.Unlock()
		if cmd == nil {
			return
		}
		_ = cmd.Wait() //nolint:errcheck // child exit status is not actionable here.

		// The child is done; release its log file handles.
		f.mu.Lock()
		files := f.files[i]
		f.files[i] = nil
		startedAt := f.started[i]
		shuttingDown := f.shutdown
		f.mu.Unlock()
		if files != nil {
			_ = files.out.Close() //nolint:errcheck // best-effort teardown.
		}

		if shuttingDown {
			return
		}
		if !f.restart {
			fmt.Fprintf(os.Stderr, "farm: instance %d exited (no-restart)\n", i)
			return
		}

		// Only rapid exit loops count toward the limit: a child that ran for
		// farmStableUptime was healthy, so reset the counter first.
		if time.Since(startedAt) >= farmStableUptime {
			restarts = 0
		}
		restarts++
		if restarts > farmMaxRestarts {
			f.mu.Lock()
			f.cmds[i] = nil
			f.manifest.Instances[i].PID = 0
			f.mu.Unlock()
			fmt.Fprintf(os.Stderr, "farm: instance %d exceeded %d restarts, giving up: %v\n",
				i, farmMaxRestarts, errFarmRestartLimit)
			_ = f.writeManifest() //nolint:errcheck // manifest refresh is best-effort.
			return
		}

		fmt.Fprintf(os.Stderr, "farm: instance %d exited, restarting (%d/%d)\n", i, restarts, farmMaxRestarts)
		time.Sleep(farmRestartBackoff)
		// Shutdown may have started during the backoff; respawning now would
		// orphan the new child because killAll already ran.
		f.mu.Lock()
		shuttingDown = f.shutdown
		f.mu.Unlock()
		if shuttingDown {
			return
		}
		if err := f.spawn(i); err != nil {
			fmt.Fprintf(os.Stderr, "farm: restart instance %d failed: %v\n", i, err)
			return
		}
		// The restarted child binds a fresh random control port, so refresh
		// the manifest in the background; the .ctl file stays authoritative
		// either way.
		go f.refreshControlPort(i)
		_ = f.writeManifest() //nolint:errcheck // manifest refresh is best-effort.
	}
}

// refreshControlPorts reads every instance's control-port file concurrently
// once the farm is up and records the ports in the manifest, so a harness can
// trigger events without guessing the file layout. It blocks until every
// instance has been polled (or timed out) so the manifest written right after
// reflects the final state; the total cost is one readyTimeout, not
// count×timeout.
func (f *farm) refreshControlPorts(count int) {
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			f.refreshControlPort(slot)
		}(i)
	}
	wg.Wait()
}

// refreshControlPort polls one instance's control-port file until it appears
// (bounded by readyTimeout) and stores the value in the manifest.
func (f *farm) refreshControlPort(i int) {
	f.mu.Lock()
	path := f.manifest.Instances[i].ControlPortFile
	f.mu.Unlock()

	deadline := time.Now().Add(f.readyTimeout)
	for time.Now().Before(deadline) {
		port, err := readControlPortFile(path)
		if err == nil {
			f.mu.Lock()
			f.manifest.Instances[i].ControlPort = port
			f.mu.Unlock()
			return
		}
		time.Sleep(farmPollInterval)
	}
}

// waitReady dials every instance's HTTP port concurrently until it accepts
// connections or its own ready-timeout elapses, so the farm's startup cost is
// the slowest instance rather than the sum of all instances. It returns how
// many instances became ready plus the first per-instance error. A single
// slow instance is not fatal, but zero ready means the template or the port
// block is broken and the caller aborts the farm.
//
// There is deliberately no early-out for a child that has already exited:
// exit state only becomes observable through exec.Cmd.Wait, and the
// supervisor goroutines that own Wait are started after waitReady returns.
// A dead child therefore just burns its timeout here, in parallel with the
// healthy instances, and the supervisor restarts it afterwards.
func (f *farm) waitReady(count int) (int, error) {
	var wg sync.WaitGroup
	readyCh := make(chan int, count)
	var mu sync.Mutex
	var firstErr error

	waitOne := func(i int) {
		defer wg.Done()
		port := f.manifest.Instances[i].HTTPPort
		addr := "127.0.0.1:" + strconv.Itoa(port)
		deadline := time.Now().Add(f.readyTimeout)
		for time.Now().Before(deadline) {
			conn, err := dialProbe(addr)
			if err == nil {
				_ = conn.Close() //nolint:errcheck // best-effort.
				readyCh <- i
				return
			}
			time.Sleep(farmPollInterval)
		}
		msg := fmt.Sprintf("instance %d (http :%d) not ready within %s", i, port, f.readyTimeout)
		fmt.Fprintln(os.Stderr, "warning: "+msg)
		mu.Lock()
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: %s", errFarmNotReady, msg)
		}
		mu.Unlock()
	}

	for i := range count {
		wg.Add(1)
		go waitOne(i)
	}
	wg.Wait()
	close(readyCh)
	return len(readyCh), firstErr
}

// dialProbe opens a short-lived TCP connection to addr for a liveness probe.
// It routes through a context-carrying net.Dialer rather than
// net.DialTimeout, and it is deliberately given a background context: the
// probe must respect its own dial timeout, not the caller's lifetime.
func dialProbe(addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: farmDialTimeout}
	return dialer.DialContext(context.Background(), "tcp", addr)
}

// beginShutdown flips the shutdown flag so supervisor goroutines stop
// respawning children.
func (f *farm) beginShutdown() {
	f.mu.Lock()
	f.shutdown = true
	f.mu.Unlock()
}

// killAll terminates every running child, including any grandchildren the
// child put in its own Unix process group.
func (f *farm) killAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, cmd := range f.cmds {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		if err := terminateChild(cmd); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "farm: kill instance %d (pid %d): %v\n", i, cmd.Process.Pid, err)
		}
	}
}

// killAllAndReap kills every child and waits for the processes to be reaped,
// bounded by timeout. It is used on failure paths where the per-slot
// supervisor goroutines were never started (spawn failure, zero ready), so the
// caller owns Wait.
func (f *farm) killAllAndReap(timeout time.Duration) {
	f.mu.Lock()
	cmds := make([]*exec.Cmd, 0, len(f.cmds))
	for _, cmd := range f.cmds {
		if cmd != nil && cmd.Process != nil {
			cmds = append(cmds, cmd)
		}
	}
	f.mu.Unlock()

	f.killAll()

	done := make(chan struct{})
	go func() {
		for _, cmd := range cmds {
			_ = cmd.Wait() //nolint:errcheck // exit status is not actionable.
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Fprintln(os.Stderr, "warning: timed out reaping child processes")
	}
}

// writeManifest serializes the current farm state to manifest.json, best-effort.
func (f *farm) writeManifest() error {
	f.mu.Lock()
	m := f.manifest
	f.mu.Unlock()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.outDir, "manifest.json"), data, fileMode)
}

// validatePortRange checks that a block of count ports starting at start fits
// inside 1..farmMaxPort and returns its last port.
func validatePortRange(label string, start, count int) (int, error) {
	last := start + count - 1
	if start < 1 || last > farmMaxPort {
		return 0, fmt.Errorf("%w: %s %d..%d", errFarmPortRange, label, start, last)
	}
	return last, nil
}

// detectFarmHost returns the first non-loopback IPv4 address of the host so
// manifest URLs are reachable from another machine or a Dockerised VMS. Falls
// back to 127.0.0.1 when no such address exists.
func detectFarmHost() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String()
		}
	}
	return "127.0.0.1"
}
