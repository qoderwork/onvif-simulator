package rtsp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/oops1/go.264"
)

func TestNewGo264SourceProbe(t *testing.T) {
	src, err := NewGo264Source(Go264SourceConfig{
		Width:   320,
		Height:  240,
		FPS:     15,
		QP:      28,
		GOPSize: 15,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewGo264Source: %v", err)
	}
	probe := src.Describe()
	if probe == nil {
		t.Fatal("Describe returned nil probe")
	}
	if probe.Codec != CodecH264 {
		t.Fatalf("probe codec = %q, want H264", probe.Codec)
	}
	if probe.Width != 320 || probe.Height != 240 || probe.FPS != 15 {
		t.Fatalf("probe geometry = %dx%d@%d, want 320x240@15", probe.Width, probe.Height, probe.FPS)
	}
}

// TestGo264EncodePipeline drives the same encode path the producer uses:
// draw a synthetic frame, convert to I420, encode with go.264, split the
// Annex-B output, and assert the first access unit carries SPS, PPS, and an
// IDR slice. This is the round-trip that makes the RTSP stream decodable.
func TestGo264EncodePipeline(t *testing.T) {
	cfg := Go264SourceConfig{Width: 160, Height: 120, FPS: 10, QP: 28, GOPSize: 10}
	g, err := NewGo264Source(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewGo264Source: %v", err)
	}

	enc, err := go264.NewEncoder(go264.EncoderConfig{
		Width:         cfg.Width,
		Height:        cfg.Height,
		FPSNum:        cfg.FPS,
		FPSDen:        1,
		GOPSize:       cfg.GOPSize,
		QP:            cfg.QP,
		ForceSoftware: true,
	})
	if err != nil {
		t.Fatalf("go264.NewEncoder: %v", err)
	}
	defer enc.Close()

	// Encode GOPSize+1 frames: frame 0 must anchor with SPS+PPS+IDR, and
	// the final frame (the next GOP boundary) must also be an IDR.
	frames := int64(cfg.GOPSize + 1)
	for frame := int64(0); frame < frames; frame++ {
		rgba := g.drawFrame(frame)
		i420 := rgbaToI420(rgba, cfg.Width, cfg.Height)
		if len(i420) != cfg.Width*cfg.Height*3/2 {
			t.Fatalf("i420 size = %d, want %d", len(i420), cfg.Width*cfg.Height*3/2)
		}
		annexB, encErr := enc.Encode(i420)
		if encErr != nil {
			t.Fatalf("encode frame %d: %v", frame, encErr)
		}
		nals := splitAnnexB(annexB)
		if len(nals) == 0 {
			t.Fatalf("frame %d: splitAnnexB returned no NALs", frame)
		}
		var hasSPS, hasPPS, hasIDR bool
		for _, nal := range nals {
			if len(nal) == 0 {
				continue
			}
			switch nal[0] & 0x1F {
			case 7:
				hasSPS = true
			case 8:
				hasPPS = true
			case 5:
				hasIDR = true
			}
		}
		switch {
		case frame == 0:
			if !(hasSPS && hasPPS && hasIDR) {
				t.Fatalf("frame 0 missing parameter sets / IDR: sps=%v pps=%v idr=%v", hasSPS, hasPPS, hasIDR)
			}
		case frame%int64(cfg.GOPSize) == 0:
			if !hasIDR {
				t.Fatalf("frame %d (GOP boundary) has no IDR slice", frame)
			}
		default:
			// P-frame within the GOP: must still carry at least one slice.
			if len(nals) == 0 {
				t.Fatalf("frame %d produced no NALs", frame)
			}
		}
	}
}

func TestSplitAnnexB(t *testing.T) {
	// Three-byte and four-byte start codes mixed. The zero_byte of a four-byte
	// start code is part of the delimiter, not of the preceding NAL, so it must
	// not leak into the SPS payload; the trailing zero at EOF is stripped too.
	data := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xac, 0x00, // SPS, followed by a 4-byte start code
		0x00, 0x00, 0x01, 0x68, 0xce, 0x3c, 0x80, // PPS
		0x00, 0x00, 0x01, 0x65, 0x01, 0x02, 0x03, 0x00, // IDR slice, trailing zero
	}
	nals := splitAnnexB(data)
	if len(nals) != 3 {
		t.Fatalf("want 3 NALs, got %d: %q", len(nals), nals)
	}
	if !bytes.Equal(nals[0], []byte{0x67, 0x42, 0xac}) {
		t.Fatalf("SPS = %x, want 6742ac", nals[0])
	}
	if !bytes.Equal(nals[1], []byte{0x68, 0xce, 0x3c, 0x80}) {
		t.Fatalf("PPS = %x, want 68ce3c80", nals[1])
	}
	if !bytes.Equal(nals[2], []byte{0x65, 0x01, 0x02, 0x03}) {
		t.Fatalf("IDR = %x, want 65010203", nals[2])
	}
}

// TestSplitAnnexBTrimsPadding locks in that a NAL padded with zero bytes
// before the next delimiter loses the padding instead of gaining it.
func TestSplitAnnexBTrimsPadding(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x01, 0x68, 0xce, 0x3c, 0x80, 0x00, 0x00,
		0x00, 0x00, 0x01, 0x65, 0x01,
	}
	nals := splitAnnexB(data)
	if len(nals) != 2 {
		t.Fatalf("want 2 NALs, got %d: %q", len(nals), nals)
	}
	if !bytes.Equal(nals[0], []byte{0x68, 0xce, 0x3c, 0x80}) {
		t.Fatalf("PPS = %x, want 68ce3c80", nals[0])
	}
	if !bytes.Equal(nals[1], []byte{0x65, 0x01}) {
		t.Fatalf("slice = %x, want 6501", nals[1])
	}
}

// TestGo264UnknownModeRejected ensures a typo cannot silently fall back to
// live encoding in a 10-instance farm.
func TestGo264UnknownModeRejected(t *testing.T) {
	_, err := NewGo264Source(Go264SourceConfig{Width: 32, Height: 32, Mode: "stream"}, slog.Default())
	if !errors.Is(err, ErrGo264ModeInvalid) {
		t.Fatalf("expected ErrGo264ModeInvalid, got %v", err)
	}
}

// TestGo264LoopModeReplaysCachedFrames drives the loop-mode producer directly
// and verifies the two properties that make it cheap: PTS keeps increasing
// across warm-up into replay, and every replay frame is byte-identical to the
// cached frame it replaces (period == LoopSeconds*FPS).
func TestGo264LoopModeReplaysCachedFrames(t *testing.T) {
	const (
		fps      = 20
		loopSecs = 1
	)
	src, err := NewGo264Source(Go264SourceConfig{
		Width:       64,
		Height:      64,
		FPS:         fps,
		QP:          30,
		GOPSize:     fps,
		Mode:        Go264ModeLoop,
		LoopSeconds: loopSecs,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewGo264Source: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.produce(ctx)

	period := fps * loopSecs
	want := period + 2*fps
	aus := make([]AccessUnit, 0, want)
	timeout := time.After(20 * time.Second)
	for len(aus) < want {
		select {
		case au := <-src.LiveSource.in:
			aus = append(aus, au)
		case <-timeout:
			t.Fatalf("timed out after %d/%d access units", len(aus), want)
		}
	}

	for i := 1; i < len(aus); i++ {
		if aus[i].PTS <= aus[i-1].PTS {
			t.Fatalf("PTS not strictly increasing at %d: %d <= %d", i, aus[i].PTS, aus[i-1].PTS)
		}
	}
	for i := period; i+period < len(aus); i++ {
		if !sameNALs(aus[i].NALs, aus[i+period].NALs) {
			t.Fatalf("replay frame %d differs from cached frame %d", i+period, i)
		}
	}
}

// TestGo264SeedChangesPattern keeps the farm's per-instance seed honest: two
// seeds must not render the same picture, or a 12-up wall looks like 12
// copies of one camera.
func TestGo264SeedChangesPattern(t *testing.T) {
	draw := func(seed int) []byte {
		s := &go264Source{cfg: Go264SourceConfig{Width: 32, Height: 32, FPS: 10, Seed: seed}}
		buf := make([]byte, 32*32*4)
		s.drawFrameInto(buf, 5)
		return buf
	}
	if bytes.Equal(draw(0), draw(1)) {
		t.Fatal("seed 0 and seed 1 rendered identical frames")
	}
}

func sameNALs(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
