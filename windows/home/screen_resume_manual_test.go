//go:build windows

package homewindows

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"

	"nhooyr.io/websocket"
)

// droppableConn is a relay transport the test can cut, as a proxy would.
type droppableConn struct {
	tunnel.MessageConn
	dropped atomic.Bool
}

var errTransportCut = errors.New("transport cut by the test")

func (c *droppableConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	kind, data, err := c.MessageConn.Read(ctx)
	if c.dropped.Load() {
		return 0, nil, errTransportCut
	}
	return kind, data, err
}

func (c *droppableConn) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	if c.dropped.Load() {
		return errTransportCut
	}
	return c.MessageConn.Write(ctx, kind, data)
}

func (c *droppableConn) cut() {
	c.dropped.Store(true)
	_ = c.MessageConn.CloseNow()
}

// TestManualScreenSessionSurvivesATransportDrop opens a live screen session
// to a saved destination through the real relays, cuts its WebSocket the way
// a proxy would, and checks that the same session continues over a resumed
// transport. Set DESKFERRY_MANUAL_RESUME_DESTINATION to a destination name.
func TestManualScreenSessionSurvivesATransportDrop(t *testing.T) {
	name := os.Getenv("DESKFERRY_MANUAL_RESUME_DESTINATION")
	if name == "" {
		t.Skip("set DESKFERRY_MANUAL_RESUME_DESTINATION to test through the relays")
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
	relayAddr := cfg.relayAddresses()[0]
	headers := http.Header{}
	tunnel.AddProtocolV2Header(headers)
	headers.Set(tunnel.HeaderResumable, "1")
	tunnel.AddHeartbeatHeader(headers)
	headers.Set(tunnel.HeaderRoomProof, cfg.roomProof())
	tunnel.AddServiceHeader(headers, tunnel.ServiceScreen)
	ws, err := tunnel.DialMessageConnWithHeaders(ctx, relayAddr, cfg.Proxy, tunnel.RoleClient, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := tunnel.AwaitSessionReadyCompatibleServiceInfo(ctx, ws, tunnel.ServiceScreen)
	if err != nil {
		t.Fatal(err)
	}
	if ready.SessionID == "" {
		t.Fatal("the relay did not offer a resumable session")
	}
	transport := &droppableConn{MessageConn: ws}
	var logMu sync.Mutex
	var resumeLog []string
	conn := tunnel.NewResumableWebSocketConn(ctx, transport, tunnel.ResumableWebSocketOptions{
		RelayAddr: relayAddr, AlternateRelayAddrs: cfg.relayAddresses(), Proxy: cfg.Proxy, SessionID: ready.SessionID,
		Side: "client", RoomProof: cfg.roomProof(), Service: tunnel.ServiceScreen, Heartbeat: ready.Heartbeat,
		Logf: func(format string, args ...any) {
			logMu.Lock()
			resumeLog = append(resumeLog, time.Now().Format("15:04:05.000 ")+fmt.Sprintf(format, args...))
			logMu.Unlock()
		},
	})
	var frames atomic.Int64
	client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{
		ScreenKey: cfg.screenKey(), Client: "agent", Name: "DeskFerry resume test",
		OnFrame: func(screenview.FrameUpdate) { frames.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	t.Logf("session %s relay=%s heartbeat=%t", ready.SessionID, relayAddr, ready.Heartbeat)
	time.Sleep(5 * time.Second)
	before := frames.Load()
	if before == 0 {
		t.Fatal("no frames before the drop")
	}
	transport.cut()
	cutAt := time.Now()
	t.Logf("cut the transport after %d frames", before)
	// Ask for a full redraw after the drop; the reply proves the session
	// carries requests and frames both ways again.
	refreshCtx, refreshCancel := context.WithTimeout(ctx, 45*time.Second)
	defer refreshCancel()
	if _, err := client.Call(refreshCtx, screenview.Command{Cmd: "refresh", Keyframe: true}); err != nil {
		t.Fatalf("the session did not survive the drop: %v", err)
	}
	t.Logf("session answered %s after the drop", time.Since(cutAt).Round(time.Millisecond))
	time.Sleep(3 * time.Second)
	select {
	case <-client.Done():
		t.Fatalf("the session ended: %v", client.Err())
	default:
	}
	if frames.Load() <= before {
		t.Fatal("no frames arrived after the drop")
	}
	logMu.Lock()
	for _, line := range resumeLog {
		t.Log(line)
	}
	logMu.Unlock()
	t.Logf("frames: %d before the drop, %d in total", before, frames.Load())
}
