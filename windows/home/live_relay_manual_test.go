//go:build windows

package homewindows

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"deskferry/internal/screenagent"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
	"deskferry/internal/winsecret"
)

// TestManualLiveScreenThroughRelay opens a read-only live screen session
// through the real relays to the Work agent of a room, as an AI agent would.
// It needs DESKFERRY_MANUAL_RELAY_ROOM, DESKFERRY_MANUAL_RELAY_PROXY and
// DESKFERRY_MANUAL_PASSWORD_FILE (a DPAPI room password file).
func TestManualLiveScreenThroughRelay(t *testing.T) {
	room := os.Getenv("DESKFERRY_MANUAL_RELAY_ROOM")
	if room == "" {
		t.Skip("set DESKFERRY_MANUAL_RELAY_ROOM to test through the relays")
	}
	protected, err := os.ReadFile(os.Getenv("DESKFERRY_MANUAL_PASSWORD_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	password, err := winsecret.Unprotect(protected)
	if err != nil {
		t.Fatal(err)
	}
	relays := []string{"https://test-officialwebsite.azurewebsites.net/relay/" + room, "http://217.142.228.117/relay/" + room}
	cfg := config{Proxy: os.Getenv("DESKFERRY_MANUAL_RELAY_PROXY"), SelectedDestination: "manual"}
	cfg.setRelayAddresses(relays)
	cfg.Destinations = []destinationProfile{{
		Name:       "manual",
		RelayAddrs: relays,
		Room:       room,
		RoomProof:  tunnel.RoomPasswordProof(relays[0], "", password),
	}}
	connect := func(ctx context.Context) (*screenview.LiveClient, error) {
		conn, route, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
		if err != nil {
			return nil, err
		}
		t.Logf("relay=%s protocol=%s", route.RelayAddr, route.Protocol)
		return screenview.StartLive(ctx, conn, screenview.LiveClientOptions{ScreenKey: cfg.screenKey(), Client: "agent", Name: "DeskFerry manual test"})
	}
	agent := screenagent.New(connect, screenagent.Options{})
	defer agent.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	shot, err := agent.Do(ctx, "screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first screenshot after %s: %d KiB PNG; %s", time.Since(started).Round(time.Millisecond), len(shot.PNG)>>10, shot.Text)
	started = time.Now()
	if _, err := agent.Do(ctx, "screenshot", nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("second screenshot after %s", time.Since(started).Round(time.Millisecond))
	status, err := agent.Do(ctx, "screen_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status: %s", status.Text)
	if _, err := agent.Do(ctx, "left_click", screenagent.Args{}); err == nil || !strings.Contains(err.Error(), "control_not_allowed") {
		t.Fatalf("a click without -screen-control = %v; want control_not_allowed", err)
	}
	if path := os.Getenv("DESKFERRY_MANUAL_SCREENSHOT"); path != "" {
		_ = os.WriteFile(path, shot.PNG, 0600)
	}
}
