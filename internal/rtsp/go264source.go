package rtsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	go264 "github.com/oops1/go.264"

	"github.com/GyeongHoKim/onvif-simulator/internal/obs"
)

// Go264Mode values accepted on Go264SourceConfig.Mode.
const (
	// Go264ModeLive encodes one frame per frame interval for the lifetime of
	// the source. Cost is roughly constant: one core per 640x360@15 instance.
	Go264ModeLive = "live"
	// Go264ModeLoop encodes LoopSeconds of video once at startup, caches the
	// access units, then replays them at the target frame rate. Steady-state
	// CPU is near zero, which is what makes a 10+ instance farm affordable;
	// the trade-off is a one-off warm-up and a repeating clip.
	Go264ModeLoop = "loop"
)

const (
	// defaultGo264LoopSeconds is the warm-up length for Go264ModeLoop when
	// the caller leaves LoopSeconds zero.
	defaultGo264LoopSeconds = 5
	// maxGo264LoopSeconds caps the replay cache (300 s at 30 fps = 9000
	// access units) so a typo cannot turn one instance into an OOM.
	maxGo264LoopSeconds = 300
)

// Buffer geometry and test-pattern constants. Named rather than inline so the
// pixel arithmetic below stays readable, and so a mistyped digit is not
// silently a valid "magic number".
const (
	// rgbaChannels is the byte stride of one RGBA pixel (R, G, B, A).
	rgbaChannels = 4
	// i420Num/i420Den give the I420 plane size: w*h*3/2 bytes.
	i420Num = 3
	i420Den = 2
	// maxSample is the inclusive upper bound of an 8-bit sample.
	maxSample = 255
	// sampleMask keeps an int inside 8 bits before the narrowing
	// conversion, which is what makes byte(x & sampleMask) provably
	// lossless (gosec G115).
	sampleMask = 0xFF
	// stripeScrollPixels is the horizontal scroll speed of the stripe
	// pattern, in pixels per frame.
	stripeScrollPixels = 4
	// stripeDriftR/G/B are the per-frame palette drifts of the R/G/B
	// channels, in samples per frame. Differing rates make the stripe
	// pattern cycle through hues instead of pulsing one channel.
	stripeDriftR = 2
	stripeDriftG = 1
	stripeDriftB = 3
	// ballPhasePrime spreads the ball's start phase across seeds so N
	// instances do not move in lockstep.
	ballPhasePrime = 97
	// ballSineRate/ballCosineRate are the angular rates (rad/s) of the
	// ball's Lissajous path.
	ballSineRate   = 1.3
	ballCosineRate = 1.7
	// annexBStartCodeLen is the length of a 00 00 01 Annex-B start code.
	annexBStartCodeLen = 3
)

// ErrGo264ModeInvalid is returned by NewGo264Source for an unknown Mode.
var ErrGo264ModeInvalid = errors.New("rtsp: go264 mode must be \"live\" or \"loop\"")

// Go264SourceConfig configures the pure-Go synthetic H.264 source. All
// fields are optional; zero values fall back to sensible SD/HD test defaults
// (see defaultGo264Config).
type Go264SourceConfig struct {
	Width   int
	Height  int
	FPS     int
	QP      int
	GOPSize int
	// Mode is Go264ModeLive (default) or Go264ModeLoop.
	Mode string
	// LoopSeconds is the warm-up length for Go264ModeLoop (default 5).
	LoopSeconds int
	// Seed varies the color phase and ball path so several instances look
	// like different cameras.
	Seed int
}

// defaultGo264Config returns the baseline test-encoder settings. 1280x720@30
// matches the bundled embedded sample so behavior between the "file" and
// "go264" source kinds is visually consistent; operators can dial the
// resolution down if the test host is CPU-constrained (the encoder is
// pure-Go and scales with pixels x fps).
func defaultGo264Config() Go264SourceConfig {
	return Go264SourceConfig{
		Width:       1280,
		Height:      720,
		FPS:         30,
		QP:          28,
		GOPSize:     30,
		Mode:        Go264ModeLive,
		LoopSeconds: defaultGo264LoopSeconds,
	}
}

// go264Source is a Source that synthesizes a moving H.264 stream entirely in
// Go via github.com/oops1/go.264. It wraps a LiveSource: the wrapper owns the
// encoder and a frame-generation goroutine that draws a dynamic pattern,
// encodes each frame, and pushes the resulting access units to the embedded
// LiveSource which handles RTP packetization, the GOP replay cache, and
// SDP parameter-set mirroring.
//
// The encoder runs with ForceSoftware=true so no hardware (NVENC/VA-API)
// backend is ever opened — important for headless test hosts and for builds
// produced with -tags go264_nohwaccel.
type go264Source struct {
	*LiveSource
	enc    *go264.Encoder
	cfg    Go264SourceConfig
	logger *slog.Logger

	// startOnce guards the producer goroutine so Run can be defensive even
	// if a server were to call it twice (it never should).
	startOnce chan struct{}

	// closeOnce makes the encoder release idempotent: Go264ModeLoop closes
	// it right after warm-up (freeing its buffers), while the deferred
	// cleanup in produce still runs on every exit path.
	closeOnce sync.Once
}

// closeEncoder releases the encoder at most once.
func (g *go264Source) closeEncoder() {
	g.closeOnce.Do(func() {
		if g.enc == nil {
			return
		}
		if err := g.enc.Close(); err != nil {
			// Nothing actionable at teardown, but the encoder can report a
			// flush failure and that is worth a line in the log.
			g.logger.Warn("rtsp go264: encoder close", "error", err)
		}
	})
}

// NewGo264Source builds a synthetic H.264 source. The probe carries codec
// (H264) and geometry from cfg; SPS/PPS are intentionally left empty because
// LiveSource extracts them from the first encoded IDR and mirrors them into
// the gortsplib H264 format before Ready() releases — the same path the
// rpicam live source uses.
func NewGo264Source(cfg Go264SourceConfig, logger *slog.Logger) (*go264Source, error) {
	if logger == nil {
		logger = obs.Discard()
	}
	if cfg.Width <= 0 {
		cfg.Width = defaultGo264Config().Width
	}
	if cfg.Height <= 0 {
		cfg.Height = defaultGo264Config().Height
	}
	if cfg.FPS <= 0 {
		cfg.FPS = defaultGo264Config().FPS
	}
	if cfg.QP <= 0 {
		cfg.QP = defaultGo264Config().QP
	}
	if cfg.GOPSize <= 0 {
		cfg.GOPSize = defaultGo264Config().GOPSize
	}
	switch cfg.Mode {
	case "":
		cfg.Mode = Go264ModeLive
	case Go264ModeLive, Go264ModeLoop:
	default:
		return nil, fmt.Errorf("%w: %q", ErrGo264ModeInvalid, cfg.Mode)
	}
	if cfg.LoopSeconds <= 0 {
		cfg.LoopSeconds = defaultGo264LoopSeconds
	}
	if cfg.LoopSeconds > maxGo264LoopSeconds {
		cfg.LoopSeconds = maxGo264LoopSeconds
	}
	// Normalise the seed into 0..255 so callers can use any int (the farm
	// derives it from the instance index) without touching the pattern
	// arithmetic below.
	cfg.Seed %= 256
	if cfg.Seed < 0 {
		cfg.Seed += 256
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
		return nil, fmt.Errorf("rtsp: create go264 encoder: %w", err)
	}

	probe := &ProbeResult{
		Codec:  CodecH264,
		Width:  cfg.Width,
		Height: cfg.Height,
		FPS:    cfg.FPS,
	}

	g := &go264Source{
		LiveSource: NewLiveSource(probe, logger),
		enc:        enc,
		cfg:        cfg,
		logger:     logger,
		startOnce:  make(chan struct{}, 1),
	}
	return g, nil
}

// Run starts the frame-production goroutine and then drives the embedded
// LiveSource. It returns when ctx is canceled (context.Canceled, treated as
// graceful by the server) or the AU channel is closed.
func (g *go264Source) Run(ctx context.Context) error {
	produceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	select {
	case g.startOnce <- struct{}{}:
		go g.produce(produceCtx)
	default:
		// Defensive: a second Run call would spawn a second producer and
		// double the frame rate. Servers never do this, but refuse rather
		// than leak a goroutine.
		g.logger.Warn("rtsp go264: Run called more than once; ignoring extra producer")
	}

	err := g.LiveSource.Run(ctx)
	// Ensure the AU channel is closed so a lingering Push (racing the
	// cancel) is dropped rather than blocking, and so LiveSource.Run's
	// channel-close path is not double-triggered.
	g.Close()
	return err
}

// produce is the encoder goroutine. Go264ModeLive encodes one frame per
// frame interval forever; Go264ModeLoop warms up a fixed-length clip and then
// replays it without touching the encoder. It exits when produceCtx is
// canceled.
func (g *go264Source) produce(produceCtx context.Context) {
	defer g.closeEncoder()

	baseTS, err := randomUint32()
	if err != nil {
		g.logger.Error("rtsp go264: random timestamp seed", "err", err)
		baseTS = 0
	}

	switch g.cfg.Mode {
	case Go264ModeLoop:
		g.produceLoop(produceCtx, baseTS)
	default:
		g.produceLive(produceCtx, baseTS)
	}
}

// produceLive encodes a fresh frame every tick, indefinitely.
func (g *go264Source) produceLive(produceCtx context.Context, baseTS uint32) {
	frameDur := time.Duration(float64(time.Second) / float64(g.cfg.FPS))
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	// Reuse both frame buffers across ticks: at 720p30 a fresh RGBA+I420
	// pair per frame would allocate ~5 MB x 30/s.
	w, h := g.cfg.Width, g.cfg.Height
	rgba := make([]byte, w*h*rgbaChannels)
	i420 := make([]byte, w*h*i420Num/i420Den)

	var frameIdx int64
	for {
		select {
		case <-produceCtx.Done():
			return
		case <-ticker.C:
		}

		nals, ok := g.encodeFrame(rgba, i420, frameIdx)
		if !ok {
			continue
		}
		g.pushAccessUnit(baseTS, frameIdx, nals)
		frameIdx++
	}
}

// produceLoop warms up cfg.LoopSeconds of video, caches the access units, and
// then replays them at the target frame rate. The encoder is closed once the
// cache is built, so the steady-state cost of a loop-mode instance is one
// timer tick per frame instead of a full encode.
//
// Warm-up is paced against wall-clock time so the first frames reach clients
// immediately (DESCRIBE only needs the first IDR) and no burst of LoopSeconds
// worth of video is flushed at once. When the encoder is slower than the
// target frame rate the requested interval simply elapses between frames.
func (g *go264Source) produceLoop(produceCtx context.Context, baseTS uint32) {
	frameDur := time.Duration(float64(time.Second) / float64(g.cfg.FPS))
	w, h := g.cfg.Width, g.cfg.Height
	rgba := make([]byte, w*h*rgbaChannels)
	i420 := make([]byte, w*h*i420Num/i420Den)

	total := g.cfg.LoopSeconds * g.cfg.FPS
	if total < 1 {
		total = 1
	}
	cache := make([][][]byte, 0, total)

	next := time.Now()
	for i := range total {
		if produceCtx.Err() != nil {
			return
		}
		nals, ok := g.encodeFrame(rgba, i420, int64(i))
		if ok {
			nals = cloneNALs(nals)
			cache = append(cache, nals)
			g.pushAccessUnit(baseTS, int64(i), nals)
		}

		next = next.Add(frameDur)
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-produceCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}

	if len(cache) == 0 {
		g.logger.Warn("rtsp go264: loop warm-up produced no frames", "seconds", g.cfg.LoopSeconds)
		return
	}

	// Free the encoder and its buffers before entering the replay loop: this
	// is what makes steady-state CPU near zero and keeps the farm affordable.
	g.closeEncoder()
	g.logger.Info("rtsp go264: loop warm-up complete",
		"frames", len(cache), "seconds", g.cfg.LoopSeconds, "mode", Go264ModeLoop)

	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	frameIdx := int64(len(cache))
	idx := 0
	for {
		select {
		case <-produceCtx.Done():
			return
		case <-ticker.C:
		}
		g.pushAccessUnit(baseTS, frameIdx, cache[idx])
		idx = (idx + 1) % len(cache)
		frameIdx++
	}
}

// encodeFrame draws, converts, and encodes one frame. It returns ok=false when
// the encoder produced no usable NAL units, so callers can skip the frame
// without advancing their PTS bookkeeping inconsistently.
func (g *go264Source) encodeFrame(rgba, i420 []byte, idx int64) ([][]byte, bool) {
	g.drawFrameInto(rgba, idx)
	rgbaToI420Into(i420, rgba, g.cfg.Width, g.cfg.Height)

	annexB, err := g.enc.Encode(i420)
	if err != nil {
		g.logger.Warn("rtsp go264: encode frame", "idx", idx, "err", err)
		return nil, false
	}
	nals := splitAnnexB(annexB)
	if len(nals) == 0 {
		return nil, false
	}
	return nals, true
}

// pushAccessUnit stamps a 90 kHz PTS from the frame index rather than
// accumulating a per-frame delta, so fps values that do not divide 90000
// (7, 11, ...) stay on the exact grid instead of drifting.
func (g *go264Source) pushAccessUnit(baseTS uint32, frameIdx int64, nals [][]byte) {
	g.Push(AccessUnit{
		PTS:  int64(baseTS) + frameIdx*int64(videoClockRate)/int64(g.cfg.FPS),
		NTP:  time.Now(),
		NALs: nals,
	})
}

// cloneNALs deep-copies an access unit so a cached frame cannot alias the
// encoder's output buffer after it is reused.
func cloneNALs(nals [][]byte) [][]byte {
	out := make([][]byte, len(nals))
	for i, nal := range nals {
		out[i] = append([]byte(nil), nal...)
	}
	return out
}

// drawFrame renders one frame into a freshly allocated RGBA buffer. Callers
// on the hot path should prefer drawFrameInto with a reused buffer.
func (g *go264Source) drawFrame(idx int64) []byte {
	rgba := make([]byte, g.cfg.Width*g.cfg.Height*rgbaChannels)
	g.drawFrameInto(rgba, idx)
	return rgba
}

// drawFrameInto renders a deterministic, visibly-moving test pattern into
// rgba: diagonal color stripes that scroll with the frame index plus a white
// ball that bounces around the frame. The motion is what makes the "go264"
// source a useful liveness check distinct from the static embedded sample
// clip. Seed shifts the palette and the ball path, so N instances on one host
// are visually distinguishable (the farm derives it from the instance index).
func (g *go264Source) drawFrameInto(rgba []byte, idx int64) {
	w, h := g.cfg.Width, g.cfg.Height
	seed := g.cfg.Seed

	// Scroll speed in pixels per frame; keeps motion obvious at any fps.
	scroll := int(idx)*stripeScrollPixels + seed*13
	for y := range h {
		for x := range w {
			v := (x + y + scroll) % (maxSample + 1)
			i := (y*w + x) * rgbaChannels
			// The mask is what makes the narrowing conversion lossless; every
			// term is non-negative, so "and" behaves like "mod 256" here.
			rgba[i] = byte((v + int(idx)*stripeDriftR + seed*5) & sampleMask)
			rgba[i+1] = byte((v*2 + int(idx)*stripeDriftG + seed*3) & sampleMask)
			rgba[i+2] = byte((maxSample - v + int(idx)*stripeDriftB + seed*7) & sampleMask)
			rgba[i+3] = maxSample
		}
	}

	// Bouncing ball — a clear focal motion independent of the stripes. The
	// seed offsets both phases so every camera gets a different trajectory.
	t := float64(idx)/float64(g.cfg.FPS) + float64(seed%ballPhasePrime)*0.11 // seconds
	cx := int((math.Sin(t*ballSineRate)*0.5 + 0.5) * float64(w-1))
	cy := int((math.Cos(t*ballCosineRate)*0.5 + 0.5) * float64(h-1))
	radius := w / 12
	r2 := radius * radius
	for y := range h {
		dy := y - cy
		if dy < -radius || dy > radius {
			continue
		}
		for x := range w {
			dx := x - cx
			if dx < -radius || dx > radius {
				continue
			}
			if dx*dx+dy*dy <= r2 {
				i := (y*w + x) * rgbaChannels
				rgba[i] = maxSample
				rgba[i+1] = maxSample
				rgba[i+2] = maxSample
			}
		}
	}
}

// rgbaToI420 converts an RGBA buffer to a freshly allocated planar I420 buffer
// (Y, then half-resolution U and V). Callers on the hot path should prefer
// rgbaToI420Into with a reused buffer.
func rgbaToI420(rgba []byte, w, h int) []byte {
	out := make([]byte, w*h*i420Num/i420Den)
	rgbaToI420Into(out, rgba, w, h)
	return out
}

// rgbaToI420Into converts an RGBA buffer to planar I420 (Y, then
// half-resolution U and V) using integer BT.601 full-range coefficients,
// writing into out, which must hold exactly w*h*3/2 bytes. go.264's Encode
// consumes exactly Width*Height*3/2 bytes of planar I420.
func rgbaToI420Into(out, rgba []byte, w, h int) {
	yPlane := out[:w*h]
	cw, ch := w/2, h/2
	uPlane := out[w*h : w*h+cw*ch]
	vPlane := out[w*h+cw*ch:]

	for y := range h {
		for x := range w {
			i := (y*w + x) * rgbaChannels
			r := int(rgba[i])
			g := int(rgba[i+1])
			b := int(rgba[i+2])
			yy := (77*r + 150*g + 29*b + 128) >> 8
			if yy > maxSample {
				yy = maxSample
			} else if yy < 0 {
				yy = 0
			}
			// The mask is redundant after the clamp but is what makes the
			// narrowing conversion provably lossless (gosec G115).
			yPlane[y*w+x] = byte(yy & sampleMask)
		}
	}

	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			var rSum, gSum, bSum int
			for dy := range 2 {
				for dx := range 2 {
					xi := x + dx
					yi := y + dy
					if xi >= w {
						xi = w - 1
					}
					if yi >= h {
						yi = h - 1
					}
					i := (yi*w + xi) * rgbaChannels
					rSum += int(rgba[i])
					gSum += int(rgba[i+1])
					bSum += int(rgba[i+2])
				}
			}
			r := rSum >> 2
			g := gSum >> 2
			b := bSum >> 2
			u := (-43*r - 84*g + 127*b + 128) >> 8
			v := (127*r - 106*g - 21*b + 128) >> 8
			if u < 0 {
				u = 0
			} else if u > maxSample {
				u = maxSample
			}
			if v < 0 {
				v = 0
			} else if v > maxSample {
				v = maxSample
			}
			ci := (y>>1)*cw + (x >> 1)
			uPlane[ci] = byte(u)
			vPlane[ci] = byte(v)
		}
	}
}

// splitAnnexB splits an Annex-B byte stream into individual raw NAL units with
// the start codes removed. Delimiters are the 3-byte 00 00 01; a 4-byte
// 00 00 00 01 is treated as that delimiter preceded by a zero_byte, which per
// H.264 Annex B is part of the start-code prefix and therefore trimmed from
// the preceding NAL instead of left in its payload. Trailing zero bytes are
// stripped from every NAL for the same reason: emulation prevention guarantees
// a NAL payload never legitimately ends with a 00 byte.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	n := len(data)

	// scEnd[j] is the byte index immediately after the j-th 00 00 01.
	var scEnd []int
	i := 0
	for i < n-2 {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			scEnd = append(scEnd, i+annexBStartCodeLen)
			i += annexBStartCodeLen
		} else {
			i++
		}
	}

	for j := range scEnd {
		start := scEnd[j]
		var end int
		if j+1 < len(scEnd) {
			// Stop before the next delimiter; its leading zero bytes are not
			// part of this NAL.
			end = scEnd[j+1] - annexBStartCodeLen
		} else {
			end = n
		}
		for end > start && data[end-1] == 0 {
			end--
		}
		if end > start {
			nals = append(nals, data[start:end])
		}
	}
	return nals
}
