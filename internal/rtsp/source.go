package rtsp

import (
	"context"
	"fmt"
	"io"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
)

// Source is an RTSP track provider that Server registers under a URL path.
// Server calls Describe before building the gortsplib stream, AttachStream
// to bind the source to that stream, then Run in a dedicated goroutine. Run
// must observe ctx cancellation and return when it is done. A non-nil return
// is logged but does not affect other sources.
//
// Concrete implementations live alongside this file: file-backed sources
// loop a local mp4 (NewFileSource); live sources push access units from an
// external producer (livesource.go).
type Source interface {
	// Describe returns the codec and parameter-sets metadata used to build
	// the SDP and the RTP encoder. The returned pointer must remain valid
	// for the lifetime of the source — Server reads it without copying.
	Describe() *ProbeResult

	// AttachStream binds the source to the gortsplib ServerStream and the
	// description.Media that Server constructed from Describe(). Called
	// once, before Run.
	AttachStream(stream *gortsplib.ServerStream, media *description.Media)

	// Ready blocks until the source has produced enough data to honor an
	// RTSP DESCRIBE. File-backed sources return immediately because the
	// SDP is fully synthesizable from the probe. Live sources block until
	// the first IDR access unit is observed so clients never see a
	// pre-keyframe SDP. ctx may carry a timeout; Ready returns ctx.Err on
	// cancel/timeout.
	Ready(ctx context.Context) error

	// Run drives the source. It returns nil on graceful ctx-driven shutdown
	// and a non-nil error on any other terminal condition.
	Run(ctx context.Context) error
}

// NewFileSource opens the mp4 at path, probes it, and returns a Source that
// loops it through the embedded RTSP server. The probe runs synchronously so
// the caller learns about invalid files immediately rather than after the
// goroutine spawns. The file handle stays open for the rest of the process
// (Source has no Close hook because profile sources are only added once at
// startup) and is rewound on each loop iteration.
func NewFileSource(path string) (Source, error) {
	f, err := openMP4(path)
	if err != nil {
		return nil, fmt.Errorf("rtsp: open %s: %w", path, err)
	}
	probe, err := probeReader(f)
	if err != nil {
		_ = f.Close() //nolint:errcheck // best-effort cleanup on probe failure
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close() //nolint:errcheck // best-effort cleanup on seek failure
		return nil, fmt.Errorf("rtsp: seek %s: %w", path, err)
	}
	return &looper{reader: f, probe: probe}, nil
}

// NewFileSourceFromReader builds a Source that loops an mp4 supplied as an
// in-memory reader (for example an //go:embed asset), so callers need no
// on-disk file and no ffmpeg pre-generation step. The reader is rewound on
// each loop iteration, so it must be an io.ReadSeeker.
func NewFileSourceFromReader(r io.ReadSeeker) (Source, error) {
	probe, err := probeReader(r)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return &looper{reader: r, probe: probe}, nil
}
