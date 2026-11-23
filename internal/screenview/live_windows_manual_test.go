//go:build windows

package screenview

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestManualLiveDesktop runs a live session against this PC's real desktop:
// capture, cursor, status and UI Automation. It injects no input. Run it with
// DESKFERRY_MANUAL_SCREEN=1 from an interactive session.
func TestManualLiveDesktop(t *testing.T) {
	if os.Getenv("DESKFERRY_MANUAL_SCREEN") != "1" {
		t.Skip("set DESKFERRY_MANUAL_SCREEN=1 to test against the real desktop")
	}
	enablePhysicalDesktopCoordinates()
	key := DeriveScreenKey("room", "secret")
	source := NewLiveSource()
	defer source.Close()
	host := NewLiveHost()
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
		err = ServeLive(context.Background(), struct {
			io.Reader
			io.Writer
		}{reader, serverSide}, request, LiveServerOptions{ScreenKey: key, Source: source, Host: host, Logf: t.Logf})
		t.Logf("server ended: %v", err)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	frames := 0
	var bytesTotal int
	started := time.Now()
	client, err := StartLive(ctx, clientSide, LiveClientOptions{ScreenKey: key, Client: "agent", Name: "manual test", OnFrame: func(update FrameUpdate) {
		frames++
		bytesTotal += update.Bytes
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	settle, err := client.Settle(ctx, 300*time.Millisecond, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("settled=%t seq=%d after %s, %d frames, %d KiB", settle.Settled, settle.Seq, time.Since(started).Round(time.Millisecond), frames, bytesTotal>>10)
	snapshot, _, lossless := client.Snapshot()
	if snapshot == nil {
		t.Fatal("no framebuffer")
	}
	nonBlack := 0
	for i := 0; i < len(snapshot.Pix); i += 4 * 97 {
		if snapshot.Pix[i] > 16 || snapshot.Pix[i+1] > 16 || snapshot.Pix[i+2] > 16 {
			nonBlack++
		}
	}
	t.Logf("framebuffer %v lossless=%t non-black samples=%d", snapshot.Bounds(), lossless, nonBlack)
	if nonBlack == 0 {
		t.Fatal("the captured desktop is entirely black")
	}
	encodeStart := time.Now()
	rects, err := EncodeKeyframe(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, rect := range rects {
		size += len(rect.Data)
	}
	t.Logf("lossless keyframe: %d rects, %d KiB, encoded in %s", len(rects), size>>10, time.Since(encodeStart).Round(time.Millisecond))
	raw, err := client.Call(ctx, Command{Cmd: "status"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status: %s", raw)
	t.Logf("cursor: %+v", client.Cursor())
	uiStart := time.Now()
	raw, err = client.Call(ctx, Command{Cmd: "ui_tree", Depth: 3, MaxNodes: 20})
	if err != nil {
		t.Fatalf("ui_tree: %v", err)
	}
	var tree struct {
		Nodes []UINode `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &tree)
	t.Logf("ui_tree: %d nodes in %s", len(tree.Nodes), time.Since(uiStart).Round(time.Millisecond))
	for index, node := range tree.Nodes {
		if index == 5 {
			break
		}
		t.Logf("  %s %q rect=%v", node.Role, node.Name, node.Rect)
	}
	if len(tree.Nodes) == 0 {
		t.Fatal("UI Automation returned no nodes")
	}
}

// TestManualInjectHarmlessInput moves the pointer to where it already is and
// taps Shift, which checks the SendInput structure layout on this PC.
func TestManualInjectHarmlessInput(t *testing.T) {
	if os.Getenv("DESKFERRY_MANUAL_SCREEN") != "1" {
		t.Skip("set DESKFERRY_MANUAL_SCREEN=1 to test against the real desktop")
	}
	enablePhysicalDesktopCoordinates()
	source := NewLiveSource()
	defer source.Close()
	if _, _, err := source.Next(0); err != nil {
		t.Fatal(err)
	}
	cursor := source.Cursor()
	host := NewLiveHost()
	if err := host.Inject([]InputEvent{{T: "move", X: At(cursor.X), Y: At(cursor.Y)}}); err != nil {
		t.Fatalf("mouse move: %v", err)
	}
	if err := host.Inject([]InputEvent{{T: "key", VK: 0x10, Down: true}, {T: "key", VK: 0x10, Down: false}}); err != nil {
		t.Fatalf("shift tap: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, err := source.Next(0); err != nil {
		t.Fatal(err)
	}
	after := source.Cursor()
	if abs(after.X-cursor.X) > 1 || abs(after.Y-cursor.Y) > 1 {
		t.Fatalf("pointer moved from %d,%d to %d,%d", cursor.X, cursor.Y, after.X, after.Y)
	}
	if !host.LastHumanInput().IsZero() && time.Since(host.LastHumanInput()) < 50*time.Millisecond {
		t.Log("note: recent human input seen during the test")
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
