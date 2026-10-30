package rtsp

import "testing"

// TestEmbeddedSampleSourceSmoke verifies that the embedded sample clip
// (bundled via //go:embed) parses into a valid, describable RTSP source.
// This is the fallback path used when a "file" profile has an empty
// media_file_path.
func TestEmbeddedSampleSourceSmoke(t *testing.T) {
	src, err := EmbeddedSampleSource()
	if err != nil {
		t.Fatalf("EmbeddedSampleSource returned error: %v", err)
	}

	probe := src.Describe()
	if probe == nil {
		t.Fatal("Describe() returned nil probe")
	}
	if probe.Codec != "H264" && probe.Codec != "H265" {
		t.Fatalf("unexpected embedded probe codec: %q", probe.Codec)
	}
	if probe.Width == 0 || probe.Height == 0 {
		t.Fatalf("embedded probe has zero dimensions: %dx%d", probe.Width, probe.Height)
	}

	t.Logf("embedded sample: codec=%s size=%dx%d fps=%d",
		probe.Codec, probe.Width, probe.Height, probe.FPS)
}
