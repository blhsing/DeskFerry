package screenview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu      sync.Mutex
	frame   *image.RGBA
	changed bool
}

func newFakeSource(width, height int) *fakeSource {
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
		}
	}
	// A photographic block so JPEG and lossless refinement are exercised.
	random := rand.New(rand.NewSource(1))
	for y := 0; y < 64 && y < height; y++ {
		for x := 0; x < 64 && x < width; x++ {
			frame.Set(x, y, color.RGBA{R: uint8(random.Intn(256)), G: uint8(random.Intn(256)), B: uint8(random.Intn(256)), A: 255})
		}
	}
	return &fakeSource{frame: frame, changed: true}
}

func (s *fakeSource) paint(rect image.Rectangle, c color.RGBA) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			s.frame.SetRGBA(x, y, c)
		}
	}
	s.changed = true
}

func (s *fakeSource) Next(timeout time.Duration) (*image.RGBA, bool, error) {
	s.mu.Lock()
	changed := s.changed
	s.changed = false
	out := image.NewRGBA(s.frame.Bounds())
	copy(out.Pix, s.frame.Pix)
	s.mu.Unlock()
	if !changed && timeout > 0 {
		time.Sleep(min(timeout, 5*time.Millisecond))
	}
	return out, changed, nil
}

func (s *fakeSource) Cursor() CursorState { return CursorState{X: 5, Y: 6, Visible: true} }
func (s *fakeSource) Method() string      { return "fake" }
func (s *fakeSource) Close()              {}

type fakeHost struct {
	mu     sync.Mutex
	events []InputEvent
	clip   string
}

func (h *fakeHost) Inject(events []InputEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, events...)
	return nil
}
func (h *fakeHost) ReleaseAll()                     {}
func (h *fakeHost) LastHumanInput() time.Time       { return time.Time{} }
func (h *fakeHost) ClipboardGet() (string, error)   { return h.clip, nil }
func (h *fakeHost) ClipboardSet(text string) error  { h.clip = text; return nil }
func (h *fakeHost) Desktop() (string, bool, string) { return "Default", false, "Notepad" }
func (h *fakeHost) UITree(Command) ([]UINode, error) {
	return []UINode{{Name: "OK", Role: "button"}}, nil
}
func (h *fakeHost) UIFind(Command) ([]UINode, error) { return nil, nil }
func (h *fakeHost) SAS() error                       { return errors.New("not here") }

func startLivePair(t *testing.T, serverKey, clientKey string, controlAllowed bool) (*LiveClient, *fakeSource, *fakeHost, chan error) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	source := newFakeSource(200, 130)
	host := &fakeHost{}
	served := make(chan error, 1)
	go func() {
		line, err := readLine(serverSide, 1<<20)
		if err != nil {
			served <- err
			return
		}
		request, err := ReadRequest(bytes.NewReader(line))
		if err != nil {
			served <- err
			return
		}
		served <- ServeLive(context.Background(), serverSide, request, LiveServerOptions{ScreenKey: serverKey, ControlAllowed: controlAllowed, Source: source, Host: host, Version: "test", Logf: t.Logf})
		_ = serverSide.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: clientKey, Client: "agent", Name: "test"})
	if err != nil {
		_ = clientSide.Close()
		t.Logf("start live: %v", err)
		return nil, source, host, served
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, source, host, served
}

func TestLiveSessionStreamsAndSettlesLosslessly(t *testing.T) {
	key := DeriveScreenKey("room", "secret")
	client, source, host, _ := startLivePair(t, key, key, true)
	if client == nil {
		t.Fatal("live session did not start")
	}
	if client.Legacy() {
		t.Fatal("expected a live session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.Settle(ctx, 100*time.Millisecond, 5*time.Second)
	if err != nil || !result.Settled {
		t.Fatalf("settle = %+v, %v", result, err)
	}
	assertFramebufferMatches(t, client, source)

	source.paint(image.Rect(70, 70, 150, 100), color.RGBA{R: 200, G: 10, B: 10, A: 255})
	result, err = client.Settle(ctx, 100*time.Millisecond, 5*time.Second)
	if err != nil || !result.Settled || len(result.Changed) == 0 {
		t.Fatalf("settle after change = %+v, %v", result, err)
	}
	assertFramebufferMatches(t, client, source)

	if err := client.Input(ctx, InputEvent{T: "move", X: At(1), Y: At(2)}); ErrorCode(err) != "control_not_enabled" {
		t.Fatalf("input before control = %v", err)
	}
	if err := client.SetControl(ctx, true, false); err != nil {
		t.Fatal(err)
	}
	if err := client.Input(ctx, InputEvent{T: "text", S: "hi"}); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	if len(host.events) != 1 || host.events[0].S != "hi" {
		t.Fatalf("injected events = %+v", host.events)
	}
	host.mu.Unlock()
	if _, err := client.Call(ctx, Command{Cmd: "ui_tree"}); err != nil {
		t.Fatal(err)
	}
}

func assertFramebufferMatches(t *testing.T, client *LiveClient, source *fakeSource) {
	t.Helper()
	snapshot, _, lossless := client.Snapshot()
	if !lossless {
		t.Fatal("settled framebuffer is not lossless")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !bytes.Equal(snapshot.Pix, source.frame.Pix) {
		t.Fatal("framebuffer differs from the source after settling")
	}
}

func TestLiveSessionRejectsWrongPassword(t *testing.T) {
	client, _, _, _ := startLivePair(t, DeriveScreenKey("room", "secret"), DeriveScreenKey("room", "other"), true)
	if client != nil {
		t.Fatal("a session with the wrong screen key started")
	}
}

func TestLiveControlRefusedWhenNotAllowed(t *testing.T) {
	key := DeriveScreenKey("room", "secret")
	client, _, _, _ := startLivePair(t, key, key, false)
	if client == nil {
		t.Fatal("live session did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.SetControl(ctx, true, false); ErrorCode(err) != "control_not_allowed" {
		t.Fatalf("control = %v", err)
	}
}

func TestLiveClientFallsBackToLegacyStream(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	go func() {
		defer serverSide.Close()
		if _, err := readLine(serverSide, 1<<20); err != nil {
			return
		}
		frame := image.NewRGBA(image.Rect(0, 0, 32, 32))
		full, payloads, _ := EncodeFull(1, frame)
		_ = WriteFrame(serverSide, full, payloads)
		_, _ = io.Copy(io.Discard, serverSide)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: DeriveScreenKey("room", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !client.Legacy() {
		t.Fatal("expected the legacy stream")
	}
	if err := client.Input(ctx, InputEvent{T: "text", S: "x"}); ErrorCode(err) != "unsupported" {
		t.Fatalf("legacy input = %v", err)
	}
}

func TestEncodeTilesIsLosslessWhenRequested(t *testing.T) {
	source := newFakeSource(130, 70)
	grid := newTileGrid(130, 70)
	grid.markAll()
	rects, lossy, err := grid.encodeTiles(source.frame, func(index int) bool { return grid.dirty[index] }, encodeMode{Lossless: true})
	if err != nil || lossy != 0 {
		t.Fatalf("encode = %d lossy, %v", lossy, err)
	}
	client := &LiveClient{fb: image.NewRGBA(source.frame.Bounds())}
	if _, err := client.drawRects(rects); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client.fb.Pix, source.frame.Pix) {
		t.Fatal("lossless tiles did not round-trip")
	}
}

// The Android Home app derives the same key independently, from the room proof.
func TestDeriveScreenKeyVector(t *testing.T) {
	if got := DeriveScreenKey(" Room ", "secret"); got != "Z2eEzs6T6rqmtt2ZMsMIPsDWsknsfEUvRLfwZxkTmBU" {
		t.Fatalf("DeriveScreenKey = %s", got)
	}
}
