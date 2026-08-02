package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExternalRelayResumption is opt-in so the same protocol probe can run
// against the Azure, Go, or Python relay implementation during release checks.
func TestExternalRelayResumption(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	if baseURL == "" {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL is not set")
	}
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if proxySpec == "" {
		proxySpec = "direct"
	}
	room := "compat-" + time.Now().Format("150405.000000")
	relayAddr := baseURL + "/relay/" + room
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set(HeaderResumable, "1")

	agentWS, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleAgent, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	waitExternalAgent(t, ctx, baseURL, room)
	clientWS, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleClient, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := AwaitWebSocketStartSession(ctx, agentWS)
	if err != nil {
		t.Fatal(err)
	}
	clientSession, err := AwaitWebSocketStartSession(ctx, clientWS)
	if err != nil {
		t.Fatal(err)
	}
	if agentSession == "" || agentSession != clientSession {
		t.Fatalf("negotiated sessions agent=%q client=%q", agentSession, clientSession)
	}

	agentConn := NewResumableWebSocketConn(ctx, agentWS, ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: proxySpec, SessionID: agentSession, Side: "agent"})
	clientConn := NewResumableWebSocketConn(ctx, clientWS, ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: proxySpec, SessionID: clientSession, Side: "client"})
	defer agentConn.Close()
	defer clientConn.Close()
	compatTransfer(t, ctx, clientConn, agentConn, []byte("before-drop"))
	_ = clientWS.CloseNow()
	compatTransfer(t, ctx, agentConn, clientConn, []byte("after-resume"))
}

func waitExternalAgent(t *testing.T, ctx context.Context, baseURL, room string) {
	t.Helper()
	type roomStatus struct {
		Waiting int `json:"waiting_agents"`
	}
	type snapshot struct {
		Rooms []roomStatus `json:"rooms"`
	}
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/relay/status?room="+room, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var status snapshot
			err = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if err == nil && len(status.Rooms) == 1 && status.Rooms[0].Waiting == 1 {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func compatTransfer(t *testing.T, ctx context.Context, source, destination io.ReadWriter, payload []byte) {
	t.Helper()
	errs := make(chan error, 2)
	go func() {
		_, err := source.Write(payload)
		errs <- err
	}()
	got := make([]byte, len(payload))
	go func() {
		_, err := io.ReadFull(destination, got)
		errs <- err
	}()
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if string(got) != string(payload) {
		t.Fatalf("received %q, want %q", got, payload)
	}
}
