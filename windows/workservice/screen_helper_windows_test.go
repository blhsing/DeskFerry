package workservice

import (
	"io"
	"net"
	"testing"
	"time"

	"deskferry/internal/tunnel"
)

func TestScreenRelayDrainsHelperResponseBeforeClosing(t *testing.T) {
	relay, home := net.Pipe()
	helper, capture := net.Pipe()
	result := make(chan tunnel.PipeResult, 1)
	go func() {
		result <- tunnel.PipeWithResult(drainScreenResponse(relay), helper)
	}()

	request := []byte("screen request")
	response := []byte("screen response")
	captureDone := make(chan error, 1)
	go func() {
		got := make([]byte, len(request))
		if _, err := io.ReadFull(capture, got); err != nil {
			captureDone <- err
			return
		}
		if string(got) != string(request) {
			captureDone <- io.ErrUnexpectedEOF
			return
		}
		if _, err := capture.Write(response); err != nil {
			captureDone <- err
			return
		}
		captureDone <- capture.Close()
	}()

	if _, err := home.Write(request); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(home, got); err != nil {
		t.Fatalf("read helper response: %v", err)
	}
	if string(got) != string(response) {
		t.Fatalf("response = %q, want %q", got, response)
	}
	if err := <-captureDone; err != nil {
		t.Fatal(err)
	}
	if err := home.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got.BToA.Bytes != int64(len(response)) {
			t.Fatalf("helper response bytes = %d, want %d", got.BToA.Bytes, len(response))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("screen pipe did not finish after the Home connection closed")
	}
}
