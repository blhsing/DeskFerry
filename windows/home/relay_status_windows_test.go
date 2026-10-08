//go:build windows

package homewindows

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"deskferry/internal/tunnel"
	"nhooyr.io/websocket"
)

func TestConnectPreflightChecksRoomAndFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, primary, fallback string
		status                  int
		wantError               string
	}{
		{"offline", `{"rooms":[{"id":"b","control_connections":0},{"id":"h","control_connections":1}]}`, `{"rooms":[]}`, 200, `Work agent in room "b" is not connected`},
		{"data session without control", `{"rooms":[{"id":"b","active_pairs":1}]}`, `{"rooms":[]}`, 200, `Work agent in room "b" is not connected`},
		{"fallback online", `{"rooms":[]}`, `{"rooms":[{"id":"b","control_connections":1}]}`, 200, ""},
		{"relay unavailable", ``, ``, 503, `Could not check the Work agent in room "b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.primary) }))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.fallback) }))
			defer backup.Close()
			cfg := config{RelayAddrs: []string{primary.URL + "/relay/b", backup.URL + "/relay/b"}, Proxy: "direct"}
			err := checkWorkAgentOnline(context.Background(), cfg)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "Remote Desktop was not opened") {
				t.Fatalf("preflight error = %v", err)
			}
		})
	}
}

func TestDashboardStreamsOfflineChangesAndResynchronizes(t *testing.T) {
	var connections atomic.Int32
	disconnect := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DeskFerry-Role") != tunnel.RoleDashboard {
			http.Error(w, "wrong role", 400)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		generation := connections.Add(1)
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"rooms":[{"id":"b","control_connections":1}]}`)); err != nil {
			return
		}
		if generation == 1 {
			<-disconnect
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"rooms":[{"id":"b","control_connections":0}]}`))
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan dashboardUpdate, 8)
	finished := make(chan struct{})
	go func() {
		followRelayDashboard(ctx, config{RelayAddr: server.URL + "/relay/b", Proxy: "direct"}, updates)
		close(finished)
	}()
	receive := func(online bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case update := <-updates:
				if update.err == nil && update.summary.WorkOnline == online {
					return
				}
			case <-timer.C:
				t.Fatalf("no dashboard update online=%t", online)
			}
		}
	}
	receive(true)
	close(disconnect)
	receive(false)
	receive(true)
	if connections.Load() != 2 {
		t.Fatalf("dashboard connections = %d, want 2", connections.Load())
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard did not stop")
	}
}
