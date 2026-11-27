//go:build windows

package homewindows

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

// TestManualScreenFramesKeepArriving watches a saved destination's screen,
// read-only, for DESKFERRY_MANUAL_FRAMES_SECONDS (default 70) and reports how
// many frames arrived and when, to tell a still screen from a capture that
// stopped (for example with a laptop's lid closed). Set
// DESKFERRY_MANUAL_FRAMES_DESTINATION to a destination name.
func TestManualScreenFramesKeepArriving(t *testing.T) {
	name := os.Getenv("DESKFERRY_MANUAL_FRAMES_DESTINATION")
	if name == "" {
		t.Skip("set DESKFERRY_MANUAL_FRAMES_DESTINATION to watch a destination")
	}
	seconds := 70
	if value := os.Getenv("DESKFERRY_MANUAL_FRAMES_SECONDS"); value != "" {
		if parsed, err := time.ParseDuration(value + "s"); err == nil {
			seconds = int(parsed.Seconds())
		}
	}
	cfg, err := loadConfig("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := selectDestinationConfig(&cfg, name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+60)*time.Second)
	defer cancel()
	conn, route, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		t.Fatal(err)
	}
	var frames atomic.Int64
	var last atomic.Int64
	started := time.Now()
	client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{
		ScreenKey: cfg.screenKey(), Client: "agent", Name: "DeskFerry frame check",
		OnFrame: func(screenview.FrameUpdate) {
			frames.Add(1)
			last.Store(int64(time.Since(started)))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	t.Logf("relay=%s", route.RelayAddr)
	for elapsed := 0; elapsed < seconds; elapsed += 10 {
		time.Sleep(10 * time.Second)
		status := client.Status()
		t.Logf("after %ds: frames=%d last_frame_at=%s compression=%q", elapsed+10, frames.Load(),
			time.Duration(last.Load()).Round(time.Millisecond), status.Compression)
	}
	raw, err := client.Call(ctx, screenview.Command{Cmd: "status"})
	t.Logf("work status: %s (err=%v)", raw, err)
}
