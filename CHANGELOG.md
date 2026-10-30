# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Embedded sample fallback.** A `kind=file` profile with an empty
  `media_file_path` now loops a small H.264 clip compiled into the binary via
  `//go:embed`, so a fresh config streams without shipping a media file.
- **`kind=go264` synthetic source.** New pure-Go source that draws a moving
  test pattern per frame, encodes it with `github.com/oops1/go.264`
  (`ForceSoftware`), and feeds the same `LiveSource` pipeline as the other
  kinds. Configurable via `go264.width/height/fps/qp/gop_size`.
- **go264 `mode` (`live` / `loop`) and `seed`.** `mode=loop` encodes
  `loop_seconds` of video once at startup and replays the cached access units,
  so steady-state CPU is near zero — the difference between one core per
  camera and a 10+ instance farm. `seed` varies the colour phase and ball path
  per instance. `farm` derives a seed per slot when the template leaves it 0.
- **`farm` subcommand.** Launches N `serve` children from one template config,
  deriving per-instance ports, UUID, serial, profile tokens, control-port file,
  and seed, writing `manifest.json`, polling readiness per instance, and
  restarting children that exit unexpectedly until SIGINT/SIGTERM.
- **`farm -clips <dir>`.** Assigns the `.mp4` files in a directory round-robin
  to instances and turns every profile into a file source for the assigned
  clip — 10+ visually different cameras at ~0% CPU, no ffmpeg needed. The
  assignment is recorded as `clip` in `manifest.json`.
- **`farm stop`.** Terminates a farm from its manifest: kills the supervisor
  first (so it cannot respawn), then every instance, and waits until the HTTP
  ports close. Works on a live farm and on one whose parent was force-killed.
- **Windows job object.** Every farm child is assigned to a
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` job, so a farm crash or force-kill takes
  the children with it instead of leaving orphaned cameras holding ports.
  `farm` warns and continues if job objects are unavailable (nested jobs in CI).
- **Linux parent-death signal.** Every farm child is started with
  `Pdeathsig=SIGKILL`, giving Linux the same crash safety as the Windows job
  object. `serve` also verifies the supervisor pid it was launched with at
  start-up, which covers the fork→prctl window where the signal can be missed.
- **farm stop process fingerprint.** The manifest now records each process's
  executable path and creation time (`parent_exe`/`parent_started_ns`,
  `exe`/`started_ns`). Before signalling, `farm stop` verifies the pid still
  belongs to that process and skips it (`pid reused by another process`) when
  the OS reused the pid, so a stale manifest can no longer kill an unrelated
  program. Manifests without a fingerprint keep the old pid-only behaviour, and
  a UTF-8 BOM in a hand-edited manifest is tolerated.

### Changed

- `splitAnnexB` now trims trailing zero bytes from every NAL, matching Annex B
  start-code semantics (the zero_byte of a `00 00 00 01` prefix no longer leaks
  into the preceding NAL's payload).
- `profiles[].go264` width and height are validated as even at config load time
  instead of failing later in the encoder.
- Profiles that use the embedded sample or `kind=go264` no longer log a
  MJPEG-sibling warning at startup; they are skipped silently because there is
  no on-disk file to transcode.
- `serve` and `event` accept `-control-port-file`, so a farm can give every
  child (and every control call) its own loopback control port instead of all
  of them racing on `~/.onvif-simulator/control.port`.
- `farm-template.json` now uses `mode=loop` for go264 (640x360@15, 3 s
  warm-up), the shape that keeps a 12-instance wall affordable.

### Fixed

- `farm` killed only the direct child on Unix, leaving grandchildren (for
  example the rpicam helper pair) alive; it now kills the child's process
  group. It also no longer respawns a child after shutdown has begun, and the
  restarted child keeps the requested `-log-level`.
- `farm`-derived instances now rewrite `network.xaddrs` and
  `runtime.network_protocols` to their own ports, so several devices on one
  host no longer advertise the template's endpoints; discovery is forced to
  `NonDiscoverable` (devices are added by `IP:port`), HTTP/RTSP port blocks are
  validated before spawning, and `manifest.json` URLs use a real host (`-host`,
  defaulting to the first non-loopback IPv4). A child is given up after five
  consecutive rapid restarts. The implicit temp directory is removed on exit,
  and a child's stdout/stderr goes to `*.out.log` so two descriptors never
  append to the same file.
- `farm` now validates the template once before spawning (an invalid template
  used to start N children that all exited), aborts when zero instances become
  ready, prints the `manifest.json` path even for the implicit temp directory,
  and reaps every child before removing that directory or exiting.
- `config.Load` strips a leading UTF-8 BOM. Windows editors (Notepad,
  PowerShell 5.1 `Set-Content -Encoding UTF8`) produced configs that failed
  with `invalid character '\ufeff'`.

### Performance

- The go264 producer reuses its RGBA and I420 frame buffers instead of
  allocating roughly 5 MB per frame, and computes PTS on an exact 90 kHz grid
  for fps values that do not divide 90000.

## [0.4.0] — Profile S compliance

### Highlights

- **ONVIF Profile S v1.3 conformance.** Every device-side mandatory feature
  in [Profile S Specification v1.3](https://www.onvif.org/profiles/profile-s/)
  §7.1–§7.13 is implemented. The README headline and CLI/TUI/GUI surfaces
  now claim Profile S compliance.

### Added

- **§7.7 Event Handling — WS-BaseNotification push subscriptions** (`GYE-77`).
  The event service now accepts `wsnt:Subscribe` requests, validates
  `ConsumerReference`, fans `Notify` POSTs out to the registered consumer
  asynchronously, and force-expires consumers that exceed the configured
  consecutive-failure threshold. Pull-point and push subscriptions share
  one keyed registry but count against separate capacity caps.
- **§7.9 MJPEG over RTSP** (`GYE-76`). The embedded RTSP server gains an
  `MJPEGSource`; `kind=file` profiles transparently transcode through a
  bundled ffmpeg helper, and the Raspberry Pi build channel exposes the Pi
  ISP secondary stream as MJPEG. `GetVideoEncoderConfigurationOptions`
  advertises the `JPEG` option block and `GetGuaranteedNumberOfVideoEncoder
  Instances` reports a non-zero `JPEG` instance count.
- **Profile S conformance validation harness.** `just e2e` exercises every
  Profile S mandatory operation, including the new event push and metadata
  configuration paths, against a running simulator. The DTT (ONVIF Device
  Test Tool) workflow is documented under "Profile S conformance
  validation" in the README.
- **CHANGELOG.md.** First release of this file; v0.4.0 is the baseline
  entry.

### Changed

- Task runner migrated from `make` to [`just`](https://github.com/casey/just)
  (`GYE-79`). The legacy `Makefile` remains as a deprecation wrapper that
  forwards every target to the matching `just` recipe.
- CLI `--help` and `version` now show the Profile S compliance claim. The
  TUI status bar and GUI footer both surface "ONVIF Profile S v1.3
  compliant".

### Fixed

- RTSP live source no longer flips `ready` before the first IDR, so DESCRIBE
  responses never expose a pre-keyframe SDP.
- SPS/PPS are repeated in-band on every outgoing IDR for clients that miss
  the SDP-borne copy.
- `rpicamera` Contrast/Saturation/Sharpness defaults are aligned with the
  upstream `mtxrpicam` semantics (1.0 = neutral).

## [0.3.1]

See git history for pre-v0.4.0 changes — entries before this changelog was
introduced are tracked in commit messages and PR descriptions.

[Unreleased]: https://github.com/GyeongHoKim/onvif-simulator/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/GyeongHoKim/onvif-simulator/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/GyeongHoKim/onvif-simulator/releases/tag/v0.3.1
