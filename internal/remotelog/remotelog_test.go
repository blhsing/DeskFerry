package remotelog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestHubUploadsBacklogAndNewLinesAfterAcknowledgement(t *testing.T) {
	var mu sync.Mutex
	var received []string
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-DeskFerry-Role"); got != "diagnostic-log" {
			t.Errorf("role = %q", got)
		}
		if got := r.Header.Get("X-DeskFerry-Log-Component"); got != "home-agent-test" {
			t.Errorf("component = %q", got)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var value batch
			if json.Unmarshal(payload, &value) != nil {
				return
			}
			mu.Lock()
			received = append(received, value.Entries...)
			count := len(received)
			mu.Unlock()
			ack, _ := json.Marshal(acknowledgement{Accepted: len(value.Entries)})
			if conn.Write(r.Context(), websocket.MessageText, ack) != nil {
				return
			}
			if count >= 2 {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		}
	}))
	defer server.Close()

	hub := New("home-agent-test")
	hub.SetInstance("unit")
	_, _ = hub.Write([]byte("before connect\n"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.AddTarget(ctx, Target{RelayAddr: server.URL + "/relay/unit"})
	_, _ = hub.Write([]byte("after connect\n"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for uploaded logs")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || received[0] != "before connect" || received[1] != "after connect" {
		t.Fatalf("received = %#v", received)
	}
}

func TestHubBoundsOfflineBacklog(t *testing.T) {
	hub := New("test")
	for i := 0; i < maxQueuedLines+100; i++ {
		_, _ = hub.Write([]byte("line\n"))
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.lines) != maxQueuedLines {
		t.Fatalf("queued lines = %d", len(hub.lines))
	}
}
