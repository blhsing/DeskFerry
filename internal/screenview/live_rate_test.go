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
