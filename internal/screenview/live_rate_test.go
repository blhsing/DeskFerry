package screenview

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// throttledWriter limits a writer to rate bytes per second.
type throttledWriter struct {
	w     io.Writer
	rate  float64
	start time.Time
	sent  float64
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	t.sent += float64(len(p))
	if due := t.start.Add(time.Duration(t.sent / t.rate * float64(time.Second))); time.Until(due) > 0 {
		time.Sleep(time.Until(due))
	}
	return t.w.Write(p)
}

// movingSource changes every tile on every capture, like full-screen video.
type movingSource struct {
	frames []*image.RGBA
	index  int
}

func (s *movingSource) Next(timeout time.Duration) (*image.RGBA, bool, error) {
	time.Sleep(10 * time.Millisecond)
	s.index++
	return s.frames[s.index%len(s.frames)], true, nil
}
func (s *movingSource) Cursor() CursorState { return CursorState{} }
func (s *movingSource) Method() string      { return "moving" }
func (s *movingSource) Close()              {}

func TestCompressionRaisesFrameRateOnASlowLink(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 20 seconds")
	}
	var frames []*image.RGBA
	for seed := int64(1); seed <= 6; seed++ {
		frames = append(frames, photoFrame(1280, 720, seed))
	}
	key := DeriveScreenKey("room", "secret")
	serverSide, clientSide := net.Pipe()
	var logMu sync.Mutex
	var levelLog []string
	go func() {
		defer serverSide.Close()
		reader := bufio.NewReader(serverSide)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		request, err := ReadRequest(bytes.NewReader(line))
		if err != nil {
			return
		}
		conn := struct {
			io.Reader
			io.Writer
		}{reader, &throttledWriter{w: serverSide, rate: 1.5 * (1 << 20)}}
		_ = ServeLive(context.Background(), conn, request, LiveServerOptions{ScreenKey: key, Source: &movingSource{frames: frames}, Host: &fakeHost{},
			Logf: func(format string, args ...any) {
				logMu.Lock()
				levelLog = append(levelLog, time.Now().Format("15:04:05.000")+" "+sprintf(format, args...))
				logMu.Unlock()
			}})
	}()
	var mu sync.Mutex
	var stamps []time.Time
	scaled := 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: key, Client: "viewer", Name: "rate test",
		OnEncoded: func(event EncodedEvent) {
			for _, rect := range event.Rects {
				if rect.Enc == EncJPEGScaled {
					mu.Lock()
					scaled++
					mu.Unlock()
				}
			}
		},
		OnFrame: func(FrameUpdate) {
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	started := time.Now()
	time.Sleep(20 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	perSecond := make([]int, 20)
	for _, stamp := range stamps {
		if second := int(stamp.Sub(started) / time.Second); second >= 0 && second < len(perSecond) {
			perSecond[second]++
		}
	}
	t.Logf("frames per second: %v (scaled rects: %d)", perSecond, scaled)
	logMu.Lock()
	for _, line := range levelLog {
		t.Log(line)
	}
	logMu.Unlock()
	early := float64(perSecond[0]+perSecond[1]+perSecond[2]) / 3
	late := float64(perSecond[16]+perSecond[17]+perSecond[18]+perSecond[19]) / 4
	if late < early*2 {
		t.Fatalf("frame rate did not improve enough: early %.1f fps, late %.1f fps", early, late)
	}
	if scaled == 0 {
		t.Fatal("no reduced-resolution rects were sent")
	}
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func TestMinFPSZeroSendsOnlyLossless(t *testing.T) {
	var frames []*image.RGBA
	for seed := int64(1); seed <= 3; seed++ {
		frames = append(frames, photoFrame(320, 192, seed))
	}
	key := DeriveScreenKey("room", "secret")
	serverSide, clientSide := net.Pipe()
	go func() {
		defer serverSide.Close()
		reader := bufio.NewReader(serverSide)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		request, err := ReadRequest(bytes.NewReader(line))
		if err != nil {
			return
		}
		_ = ServeLive(context.Background(), struct {
			io.Reader
			io.Writer
		}{reader, serverSide}, request, LiveServerOptions{ScreenKey: key, Source: &movingSource{frames: frames}, Host: &fakeHost{}, Logf: t.Logf})
	}()
	var mu sync.Mutex
	lossy, total := 0, 0
	zero := 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: key, MinFPS: &zero, OnEncoded: func(event EncodedEvent) {
		mu.Lock()
		defer mu.Unlock()
		for _, rect := range event.Rects {
			total++
			if rect.Enc == EncJPEG || rect.Enc == EncJPEGScaled {
				lossy++
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	time.Sleep(2 * time.Second)
	if err := client.SetMinFPS(ctx, 30); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if total == 0 || lossy != 0 {
		mu.Unlock()
		t.Fatalf("with min fps 0: %d rects, %d lossy", total, lossy)
	}
	mu.Unlock()
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if lossy == 0 {
		t.Fatal("photo tiles stayed lossless after raising min fps to 30")
	}
}

// delayedReader delivers what src produces delay later, like a link with
// latency in one direction.
type delayedReader struct {
	chunks  chan delayedChunk
	pending []byte
}

type delayedChunk struct {
	data []byte
	due  time.Time
}

func newDelayedReader(src io.Reader, delay time.Duration) *delayedReader {
	reader := &delayedReader{chunks: make(chan delayedChunk, 4096)}
	go func() {
		defer close(reader.chunks)
		buffer := make([]byte, 64<<10)
		for {
			n, err := src.Read(buffer)
			if n > 0 {
				reader.chunks <- delayedChunk{data: append([]byte(nil), buffer[:n]...), due: time.Now().Add(delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		chunk, ok := <-r.chunks
		if !ok {
			return 0, io.EOF
		}
		time.Sleep(time.Until(chunk.due))
		r.pending = chunk.data
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// With 200 ms of latency on the acknowledgements, a sender that keeps only
// one frame in flight is limited to a few frames a second however fast the
// link is.
func TestFramesArePipelinedOverLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 12 seconds")
	}
	var frames []*image.RGBA
	for seed := int64(1); seed <= 6; seed++ {
		frames = append(frames, photoFrame(1280, 720, seed))
	}
	key := DeriveScreenKey("room", "secret")
	serverSide, clientSide := net.Pipe()
	go func() {
		defer serverSide.Close()
		reader := bufio.NewReader(newDelayedReader(serverSide, 200*time.Millisecond))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		request, err := ReadRequest(bytes.NewReader(line))
		if err != nil {
			return
		}
		_ = ServeLive(context.Background(), struct {
			io.Reader
			io.Writer
		}{reader, &throttledWriter{w: serverSide, rate: 4 << 20}}, request, LiveServerOptions{ScreenKey: key, Source: &movingSource{frames: frames}, Host: &fakeHost{}, Logf: t.Logf})
	}()
	var mu sync.Mutex
	var stamps []time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: key, OnFrame: func(FrameUpdate) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	started := time.Now()
	time.Sleep(12 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	perSecond := make([]int, 12)
	for _, stamp := range stamps {
		if second := int(stamp.Sub(started) / time.Second); second >= 0 && second < len(perSecond) {
			perSecond[second]++
		}
	}
	t.Logf("frames per second with 200 ms ack latency: %v", perSecond)
	late := float64(perSecond[8]+perSecond[9]+perSecond[10]+perSecond[11]) / 4
	if late < 25 {
		t.Fatalf("only %.1f fps over a 4 MB/s link with 200 ms latency", late)
	}
}

func TestControllerSkipsAStepThatDidNotHelp(t *testing.T) {
	c := newFPSController([]string{FeatureScaledJPEG}, false, nil)
	now := time.Now()
	c.decide(now, 3, true, true, 3, 0)
	if c.level != 1 {
		t.Fatalf("level = %d, want a trial of 1", c.level)
	}
	c.decide(now.Add(time.Second), 6, true, true, 6, 0) // level 1 helped
	c.decide(now.Add(2*time.Second), 6, true, true, 6, 0)
	if c.level != 2 {
		t.Fatalf("level = %d, want a trial of 2", c.level)
	}
	c.decide(now.Add(3*time.Second), 6.2, true, true, 6, 0) // level 2 did not help
	if c.level != 1 {
		t.Fatalf("level = %d, want a revert to 1", c.level)
	}
	c.decide(now.Add(4*time.Second), 6, true, true, 6, 0)
	if c.level != 3 {
		t.Fatalf("level = %d, want level 2 skipped for a trial of 3", c.level)
	}
}

// TestControllerKeepsVideoWhileMotionLasts covers a screen saver: once video
// keeps up, changes no longer wait for the link, but the screen is still
// moving, so the controller must neither call it calm nor try lighter levels.
func TestControllerKeepsVideoWhileMotionLasts(t *testing.T) {
	c := newFPSController([]string{FeatureScaledJPEG, FeatureH264}, true, nil)
	video := c.maxLevel
	if !c.levels[video].Video {
		t.Fatalf("strongest level %+v is not video", c.levels[video])
	}
	now := time.Now()
	second := func() time.Time { now = now.Add(time.Second); return now }
	c.setLevel(video, "test")
	c.keptTiles[video] = 1200
	for range 60 {
		c.decide(second(), 25, false, true, 25, 1150)
		if c.level != video {
			t.Fatalf("left video for level %d during sustained motion", c.level)
		}
	}
	// The screen calms down, then the motion returns: straight back to video.
	for range 6 {
		c.decide(second(), 0, false, false, 0, 0)
	}
	if c.level != 0 {
		t.Fatalf("level = %d after calm, want 0", c.level)
	}
	c.decide(second(), 3, true, true, 3, 1200)
	if c.level != video {
		t.Fatalf("level = %d when motion returned, want a trial of video (%d)", c.level, video)
	}
	// Motion shrinks to a small area (typing): lighter levels are tried.
	c.trial = nil
	for range headroomWindows {
		c.decide(second(), 20, false, true, 20, 40)
	}
	if c.level != video-1 {
		t.Fatalf("level = %d with little motion, want a trial of %d", c.level, video-1)
	}
	c.decide(second(), 20, false, true, 20, 40)
	if c.level != video-1 || c.trial != nil {
		t.Fatalf("lighter level was not kept (level %d)", c.level)
	}
}

func TestControllerGoesStraightToVideoForFullScreenMotion(t *testing.T) {
	c := newFPSController([]string{FeatureH264}, true, nil)
	c.totalTiles = 1000
	c.decide(time.Now(), 2, true, true, 2, 900)
	if c.level != c.maxLevel || !c.levels[c.level].Video {
		t.Fatalf("level = %d, want a trial of video (%d)", c.level, c.maxLevel)
	}
	c = newFPSController([]string{FeatureH264}, true, nil)
	c.totalTiles = 1000
	c.decide(time.Now(), 2, true, true, 2, 100)
	if c.level != 1 {
		t.Fatalf("level = %d for a small moving area, want the next step (1)", c.level)
	}
}
