package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deskferry/internal/tunnel"
	"nhooyr.io/websocket"
)

func TestRoomIDMatchesOtherRelays(t *testing.T) {
	tests := map[string]string{
		"":                      "default",
		" WorkDesk ":            "workdesk",
		"/Team Room!!/":         "team-room",
		"...":                   "default",
		strings.Repeat("A", 80): strings.Repeat("a", 64),
	}
	for input, want := range tests {
		if got := roomID(input); got != want {
			t.Fatalf("roomID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHealthAndEmptyStatus(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	resp, err := http.Get(server.URL + "/relay/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health["service"] != serviceName {
		t.Fatalf("service = %v", health["service"])
	}

	resp, err = http.Get(server.URL + "/relay/status?room=unit-empty")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status StatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Service != serviceName {
		t.Fatalf("service = %q", status.Service)
	}
	if len(status.Rooms) != 0 {
		t.Fatalf("rooms = %d, want 0", len(status.Rooms))
	}
}

func TestHomeAgentStatusPresence(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	home := dialRole(t, ctx, server.URL, "/relay/unit-home/ws", "home-agent")
	deadline := time.Now().Add(3 * time.Second)
	var status StatusSnapshot
	for time.Now().Before(deadline) {
		status = getStatus(t, server.URL, "unit-home")
		if len(status.Rooms) == 1 && status.Rooms[0].HomeAgentConnected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(status.Rooms) != 1 || !status.Rooms[0].HomeAgentConnected {
		t.Fatalf("home presence was not reflected before timeout: %+v", status.Rooms)
	}
	_ = home.Close(websocket.StatusNormalClosure, "")

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status = getStatus(t, server.URL, "unit-home")
		if len(status.Rooms) == 1 && !status.Rooms[0].HomeAgentConnected {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("home presence did not disconnect: %+v", status.Rooms)
}

func TestAgentClientPairAndBridgeBytes(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := dialRole(t, ctx, server.URL, "/relay/unit-bridge/ws", "agent")
	home := dialRole(t, ctx, server.URL, "/relay/unit-bridge/ws", "client")
	defer agent.Close(websocket.StatusNormalClosure, "")
	defer home.Close(websocket.StatusNormalClosure, "")

	expectText(t, ctx, agent, startMessage)
	expectText(t, ctx, home, startMessage)
	agent.SetReadLimit(relayWebSocketReadLimit)

	if err := home.Write(ctx, websocket.MessageBinary, []byte("from-home")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, agent, "from-home")

	if err := agent.Write(ctx, websocket.MessageBinary, []byte("from-agent")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, home, "from-agent")

	// A full resumable frame is 64 KiB of data plus a 9-byte header. This
	// exceeds nhooyr's 32 KiB default and must pass through the relay intact.
	largeFrame := bytes.Repeat([]byte{0xa5}, 64*1024+9)
	if err := home.Write(ctx, websocket.MessageBinary, largeFrame); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := agent.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || !bytes.Equal(payload, largeFrame) {
		t.Fatalf("large frame = (%v, %d bytes), want binary %d bytes", typ, len(payload), len(largeFrame))
	}

	status := getStatus(t, server.URL, "unit-bridge")
	if len(status.Rooms) != 1 || status.Rooms[0].ActivePairs != 1 || status.Rooms[0].TotalPairs != 1 {
		t.Fatalf("unexpected bridge status: %+v", status.Rooms)
	}
}

func TestResumablePairReattachesAfterWebSocketDrop(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	capability := http.Header{"X-DeskFerry-Resumable": []string{"1"}}
	agent := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-resume/ws", "agent", capability)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := getStatus(t, server.URL, "unit-resume")
		if len(status.Rooms) == 1 && status.Rooms[0].WaitingAgents == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	home := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-resume/ws", "client", capability)

	agentStart := expectTextPrefix(t, ctx, agent, startMessage+" ")
	homeStart := expectTextPrefix(t, ctx, home, startMessage+" ")
	if agentStart != homeStart {
		t.Fatalf("session starts differ: agent=%q home=%q", agentStart, homeStart)
	}
	sessionID := strings.TrimPrefix(agentStart, startMessage+" ")

	if err := home.Write(ctx, websocket.MessageBinary, []byte("before-drop")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, agent, "before-drop")
	_ = home.CloseNow()
	if _, _, err := agent.Read(ctx); err == nil {
		t.Fatal("agent socket remained open after paired client drop")
	}

	agentHeaders := http.Header{
		"X-DeskFerry-Session":      []string{sessionID},
		"X-DeskFerry-Session-Side": []string{"agent"},
	}
	clientHeaders := http.Header{
		"X-DeskFerry-Session":      []string{sessionID},
		"X-DeskFerry-Session-Side": []string{"client"},
	}
	agent = dialRoleHeaders(t, ctx, server.URL, "/relay/unit-resume/ws", resumeRole, agentHeaders)
	home = dialRoleHeaders(t, ctx, server.URL, "/relay/unit-resume/ws", resumeRole, clientHeaders)
	defer agent.Close(websocket.StatusNormalClosure, "session closed")
	defer home.Close(websocket.StatusNormalClosure, "session closed")
	expectText(t, ctx, agent, resumeMessage+" "+sessionID)
	expectText(t, ctx, home, resumeMessage+" "+sessionID)

	if err := agent.Write(ctx, websocket.MessageBinary, []byte("after-resume")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, home, "after-resume")
	status := getStatus(t, server.URL, "unit-resume")
	if len(status.Rooms) != 1 || status.Rooms[0].ActivePairs != 1 || status.Rooms[0].TotalPairs != 1 {
		t.Fatalf("resumed bridge changed pair counts: %+v", status.Rooms)
	}
}

func TestResumableStreamsSurviveForcedRelayTransportLoss(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	relayAddr := server.URL + "/relay/unit-resume-stream"
	headers := http.Header{}
	headers.Set(tunnel.HeaderResumable, "1")
	agentWS, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleAgent, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := getStatus(t, server.URL, "unit-resume-stream")
		if len(status.Rooms) == 1 && status.Rooms[0].WaitingAgents == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	clientWS, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleClient, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := tunnel.AwaitWebSocketStartSession(ctx, agentWS)
	if err != nil {
		t.Fatal(err)
	}
	clientSession, err := tunnel.AwaitWebSocketStartSession(ctx, clientWS)
	if err != nil {
		t.Fatal(err)
	}
	if agentSession == "" || agentSession != clientSession {
		t.Fatalf("negotiated sessions agent=%q client=%q", agentSession, clientSession)
	}
	agentConn := tunnel.NewResumableWebSocketConn(ctx, agentWS, tunnel.ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: "direct", SessionID: agentSession, Side: "agent"})
	clientConn := tunnel.NewResumableWebSocketConn(ctx, clientWS, tunnel.ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: "direct", SessionID: clientSession, Side: "client"})
	defer agentConn.Close()
	defer clientConn.Close()

	assertStreamTransfer(t, ctx, clientConn, agentConn, bytes.Repeat([]byte{0xa5}, 64*1024+9))
	_ = clientWS.CloseNow()
	assertStreamTransfer(t, ctx, agentConn, clientConn, []byte("after-resume"))
}

func TestAgentIdentityReplacesExistingWaitingSocket(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	headers := http.Header{
		"X-DeskFerry-Agent-Instance": []string{"unit-agent"},
		"X-DeskFerry-Agent-Slot":     []string{"2"},
	}
	first := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-replace/ws", "agent", headers)
	defer first.Close(websocket.StatusNormalClosure, "")
	status := getStatus(t, server.URL, "unit-replace")
	if len(status.Rooms) != 1 || status.Rooms[0].WaitingAgents != 1 {
		t.Fatalf("initial waiting agents = %+v, want 1", status.Rooms)
	}

	second := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-replace/ws", "agent", headers)
	defer second.Close(websocket.StatusNormalClosure, "")
	status = getStatus(t, server.URL, "unit-replace")
	if len(status.Rooms) != 1 || status.Rooms[0].WaitingAgents != 1 {
		t.Fatalf("replacement waiting agents = %+v, want 1", status.Rooms)
	}

	readCtx, stopRead := context.WithTimeout(ctx, 2*time.Second)
	defer stopRead()
	if _, _, err := first.Read(readCtx); err == nil {
		t.Fatal("replaced agent socket stayed readable/open")
	}
}

func TestDashboardWebSocketReceivesSnapshot(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dashboard := dialRole(t, ctx, server.URL, "/relay/unit-dashboard/ws?role=dashboard", "")
	defer dashboard.Close(websocket.StatusNormalClosure, "")

	typ, payload, err := dashboard.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("dashboard message type = %v", typ)
	}
	var status StatusSnapshot
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatal(err)
	}
	if status.Service != serviceName {
		t.Fatalf("service = %q", status.Service)
	}
}

func dialRole(t *testing.T, ctx context.Context, baseURL, path, role string) *websocket.Conn {
	return dialRoleHeaders(t, ctx, baseURL, path, role, nil)
}

func dialRoleHeaders(t *testing.T, ctx context.Context, baseURL, path, role string, headers http.Header) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(baseURL, "http") + path
	if headers == nil {
		headers = http.Header{}
	} else {
		headers = headers.Clone()
	}
	if role != "" {
		headers.Set("X-DeskFerry-Role", role)
	}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:      headers,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func getStatus(t *testing.T, baseURL, room string) StatusSnapshot {
	t.Helper()
	resp, err := http.Get(baseURL + "/relay/status?room=" + room)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status StatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func expectText(t *testing.T, ctx context.Context, c *websocket.Conn, want string) {
	t.Helper()
	typ, payload, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText || string(payload) != want {
		t.Fatalf("message = (%v, %q), want text %q", typ, payload, want)
	}
}

func expectTextPrefix(t *testing.T, ctx context.Context, c *websocket.Conn, prefix string) string {
	t.Helper()
	typ, payload, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText || !strings.HasPrefix(string(payload), prefix) {
		t.Fatalf("message = (%v, %q), want text prefix %q", typ, payload, prefix)
	}
	return string(payload)
}

func expectBinary(t *testing.T, ctx context.Context, c *websocket.Conn, want string) {
	t.Helper()
	typ, payload, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || string(payload) != want {
		t.Fatalf("message = (%v, %q), want binary %q", typ, payload, want)
	}
}

func assertStreamTransfer(t *testing.T, ctx context.Context, source, destination io.ReadWriter, payload []byte) {
	t.Helper()
	errCh := make(chan error, 2)
	go func() {
		_, err := source.Write(payload)
		errCh <- err
	}()
	got := make([]byte, len(payload))
	go func() {
		_, err := io.ReadFull(destination, got)
		errCh <- err
	}()
	for range 2 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if string(got) != string(payload) {
		t.Fatalf("stream received %q, want %q", got, payload)
	}
}
