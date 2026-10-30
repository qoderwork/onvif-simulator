package rtsp

import (
	"testing"
	"time"

	"log/slog"
)

// TestGo264SourceServerStream registers a pure-Go synthetic H.264 source and
// verifies a real RTSP client can DESCRIBE it and receive RTP packets.
//
// DESCRIBE blocks until Ready() releases, which for a live source requires
// the first IDR (and its SPS/PPS) to be observed and mirrored into the SDP —
// so a successful Describe already proves the encoder produced valid parameter
// sets. Receiving packets then proves the producer goroutine is actively
// encoding and the LiveSource is packetizing the stream.
func TestGo264SourceServerStream(t *testing.T) {
	s := New(freePort(t), WithLogger(slog.New(slog.DiscardHandler)))
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

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
	if _, err := s.AddSource("go264", src); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	const deadline = 10 * time.Second
	got := readRTP(t, s.Port(), "go264", deadline)
	if got == 0 {
		t.Fatalf("expected at least one RTP packet from go264 source within %s, got none", deadline)
	}
}
