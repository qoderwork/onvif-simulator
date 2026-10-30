package rtsp

import (
	"bytes"
	_ "embed"
)

//go:embed assets/main.mp4
var embeddedSampleMP4 []byte

// EmbeddedSampleSource returns an RTSP Source that loops the built-in sample
// clip. It lets `serve` stream video with no media_file_path set and no ffmpeg
// pre-generation step — the clip is baked into the binary as an //go:embed
// asset.
//
// The reader is a bytes.Reader over the embedded bytes, wrapped as an
// io.ReadSeeker so the looper can rewind and loop indefinitely without ever
// touching disk.
func EmbeddedSampleSource() (Source, error) {
	return NewFileSourceFromReader(bytes.NewReader(embeddedSampleMP4))
}
