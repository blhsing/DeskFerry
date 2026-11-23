//go:build windows

package screenview

import (
	"bufio"
	"bytes"
	"context"
	"image"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// smoothSource loops a pre-rendered, smooth, video-like animation until
// stopped.
type smoothSource struct {
	width, height int
	frame         atomic.Int64
	stopped       atomic.Bool
	last          *image.RGBA
	frames        []*image.RGBA
}

func (s *smoothSource) render(frame int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, s.width, s.height))
	for y := 0; y < s.height; y++ {
		for x := 0; x < s.width; x++ {
			offset := img.PixOffset(x, y)
			dx, dy := float64(x-frame*6), float64(y-frame*2)
			img.Pix[offset] = uint8(128 + 120*math.Sin(dx/29))
			img.Pix[offset+1] = uint8(128 + 120*math.Sin(dy/19))
			img.Pix[offset+2] = uint8(128 + 120*math.Sin((dx+dy)/37))
			img.Pix[offset+3] = 255
		}
	}
	return img
}

func (s *smoothSource) Next(timeout time.Duration) (*image.RGBA, bool, error) {
	time.Sleep(15 * time.Millisecond)
	if s.stopped.Load() && s.last != nil {
		return s.last, false, nil
	}
	s.last = s.frames[int(s.frame.Add(1))%len(s.frames)]
	return s.last, true, nil
}
func (s *smoothSource) Cursor() CursorState { return CursorState{} }
func (s *smoothSource) Method() string      { return "smooth" }
func (s *smoothSource) Close()              {}

func TestVideoRaisesFrameRateAndRefinesLosslessly(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 25 seconds")
	}
	source := &smoothSource{width: 1280, height: 720}
	for frame := 0; frame < 48; frame++ {
		source.frames = append(source.frames, source.render(frame))
	}
	key := DeriveScreenKey("room", "secret")
	serverSide, clientSide := net.Pipe()
	var logMu sync.Mutex
	var logLines []string
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
		}{reader, &throttledWriter{w: serverSide, rate: 1.5 * (1 << 20)}}, request, LiveServerOptions{
			ScreenKey: key, Source: source, Host: &fakeHost{}, VideoEncoder: mediaFoundationEncoder,
			Logf: func(format string, args ...any) {
				logMu.Lock()
				logLines = append(logLines, sprintf(format, args...))
				logMu.Unlock()
			}})
	}()
	var mu sync.Mutex
	var stamps []time.Time
	videoRects := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: key, VideoDecoder: MediaFoundationDecoder,
		OnEncoded: func(event EncodedEvent) {
			for _, rect := range event.Rects {
				if rect.Enc == EncH264 {
					mu.Lock()
					videoRects++
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
	source.stopped.Store(true)
	settle, err := client.Settle(ctx, 300*time.Millisecond, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	perSecond := make([]int, 20)
	for _, stamp := range stamps {
		if second := int(stamp.Sub(started) / time.Second); second >= 0 && second < len(perSecond) {
			perSecond[second]++
		}
	}
	t.Logf("frames per second: %v (video rects: %d)", perSecond, videoRects)
	sent := videoRects
	mu.Unlock()
	logMu.Lock()
	for _, line := range logLines {
		t.Log(line)
	}
	logMu.Unlock()
	if sent == 0 {
		t.Fatal("no video was sent")
	}
	late := float64(perSecond[16]+perSecond[17]+perSecond[18]+perSecond[19]) / 4
	if late < 25 {
		t.Fatalf("only %.1f fps with video", late)
	}
	snapshot, _, lossless := client.Snapshot()
	if !settle.Settled || !lossless || !bytes.Equal(snapshot.Pix, source.last.Pix) {
		t.Fatalf("after motion stopped the picture was not refined losslessly (settled=%t lossless=%t)", settle.Settled, lossless)
	}
}
