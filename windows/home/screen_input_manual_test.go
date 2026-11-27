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

// TestManualScreenInputChangesThePicture takes control of a saved
// destination, opens and closes the Start menu with the Windows key, and
// reports whether frames arrive around it. Set
// DESKFERRY_MANUAL_INPUT_DESTINATION to a destination name.
func TestManualScreenInputChangesThePicture(t *testing.T) {
	name := os.Getenv("DESKFERRY_MANUAL_INPUT_DESTINATION")
	if name == "" {
		t.Skip("set DESKFERRY_MANUAL_INPUT_DESTINATION to control a destination")
	}
	cfg, err := loadConfig("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := selectDestinationConfig(&cfg, name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		t.Fatal(err)
	}
	var frames atomic.Int64
	client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{
		ScreenKey: cfg.screenKey(), Client: "agent", Name: "DeskFerry input check",
		OnFrame: func(screenview.FrameUpdate) { frames.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	time.Sleep(4 * time.Second)
	t.Logf("before input: %d frames", frames.Load())
	if err := client.SetControl(ctx, true, false); err != nil {
		t.Fatalf("take control: %v", err)
	}
	press := func(vk int) {
		if err := client.Input(ctx, screenview.InputEvent{T: "key", VK: vk, Down: true}, screenview.InputEvent{T: "key", VK: vk, Down: false}); err != nil {
			t.Fatalf("key %#x: %v", vk, err)
		}
	}
	before := frames.Load()
	press(0x5B) // Windows key: opens Start
	time.Sleep(4 * time.Second)
	t.Logf("after the Windows key: %d new frames", frames.Load()-before)
	before = frames.Load()
	press(0x1B) // Escape: closes Start
	time.Sleep(4 * time.Second)
	t.Logf("after Escape: %d new frames", frames.Load()-before)
	_ = client.SetControl(ctx, false, false)
	raw, _ := client.Call(ctx, screenview.Command{Cmd: "status"})
	t.Logf("work status: %s", raw)
}
