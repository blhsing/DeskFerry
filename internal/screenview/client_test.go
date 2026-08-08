package screenview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIOptionsValidation(t *testing.T) {
	if _, err := (CLIOptions{}).request(); err == nil {
		t.Fatal("expected missing output to fail")
	}
	if _, err := (CLIOptions{Screenshot: "one.png", StreamDirectory: "frames"}).request(); err == nil {
		t.Fatal("expected competing outputs to fail")
	}
	if _, err := (CLIOptions{Screenshot: "one.png", Count: 1}).request(); err == nil {
		t.Fatal("expected a one-shot count to fail")
	}
	request, err := (CLIOptions{StreamDirectory: "frames", Interval: 500 * time.Millisecond, Count: 2}).request()
	if err != nil {
		t.Fatal(err)
	}
	if request.Mode != ModeStream || request.IntervalMS != 500 {
		t.Fatalf("request = %#v", request)
	}
}

func TestRunCLIWritesOneShotPNGToStdout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go serveTestScreen(t, server, 1)
	var output bytes.Buffer
	if err := RunCLI(context.Background(), client, CLIOptions{Screenshot: "-", Stdout: &output, Status: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(decoded.At(0, 0)).(color.RGBA); got.R != 10 {
		t.Fatalf("pixel = %#v", got)
	}
}

func TestRunCLIStopsAfterRequestedStreamCount(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go serveTestScreen(t, server, 3)
	directory := t.TempDir()
	var status bytes.Buffer
	options := CLIOptions{StreamDirectory: directory, Count: 2, Status: &status, now: func() time.Time {
		return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	}}
	if err := RunCLI(context.Background(), client, options); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("wrote %d screenshots, want 2", len(entries))
	}
	if !strings.Contains(status.String(), filepath.Join(directory, "DeskFerry-20260808-120000.000-000001.png")) {
		t.Fatalf("status output = %q", status.String())
	}
}

func serveTestScreen(t *testing.T, conn net.Conn, count int) {
	t.Helper()
	defer conn.Close()
	request, err := ReadRequest(conn)
	if err != nil {
		t.Error(err)
		return
	}
	current := image.NewRGBA(image.Rect(0, 0, 2, 1))
	current.SetRGBA(0, 0, color.RGBA{R: 10, A: 255})
	frame, payloads, err := EncodeFull(1, current)
	if err != nil {
		t.Error(err)
		return
	}
	if err := WriteFrame(conn, frame, payloads); err != nil {
		t.Error(err)
		return
	}
	if request.Mode == ModeSingle {
		return
	}
	for sequence := 2; sequence <= count; sequence++ {
		previous := cloneRGBA(current)
		current.SetRGBA(0, 0, color.RGBA{R: uint8(sequence * 10), A: 255})
		frame, payloads, err = EncodeDelta(uint64(sequence), previous, current, DefaultTileSize)
		if err != nil {
			t.Error(err)
			return
		}
		if err := WriteFrame(conn, frame, payloads); err != nil {
			return
		}
	}
}
