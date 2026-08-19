package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deskferry/internal/buildinfo"
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

func TestRoomPasswordAuthorizationAndIdleRotation(t *testing.T) {
	room := NewRelayRoom("protected")
	if !room.AuthorizeAgent("proof-a") {
		t.Fatal("first agent proof rejected")
	}
	if !room.AuthorizeClient("proof-a") {
		t.Fatal("matching client proof rejected")
	}
	if room.AuthorizeClient("") || room.AuthorizeClient("proof-b") {
		t.Fatal("mismatched client proof accepted")
	}
	if !room.AuthorizeAgent("proof-b") {
		t.Fatal("idle room did not accept password rotation")
	}
	if !room.AuthorizeClient("proof-b") || room.AuthorizeClient("proof-a") {
		t.Fatal("rotated proof was not enforced")
	}
}

func TestRoomSelectsWaitingAgentByService(t *testing.T) {
	room := NewRelayRoom("services")
	rdp, _ := room.EnqueueAgent(nil, "rdp", AgentIdentity{}, false, serviceRDP)
	winrm, _ := room.EnqueueAgent(nil, "winrm", AgentIdentity{}, false, serviceWinRM)
	smb, _ := room.EnqueueAgent(nil, "smb", AgentIdentity{}, false, serviceSMB)
	if got := room.TryTakeAgent(serviceSMB); got != smb {
		t.Fatalf("SMB selection = %p, want %p", got, smb)
	}
	if got := room.TryTakeAgent(serviceWinRM); got != winrm {
		t.Fatalf("WinRM selection = %p, want %p", got, winrm)
	}
	if got := room.TryTakeAgent(serviceRDP); got != rdp {
		t.Fatalf("RDP selection = %p, want %p", got, rdp)
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
	if health["version"] != buildinfo.Version {
		t.Fatalf("version = %v", health["version"])
	}

	dashboard, err := http.Get(server.URL + "/relay/")
	if err != nil {
		t.Fatal(err)
	}
	defer dashboard.Body.Close()
	dashboardBody, err := io.ReadAll(dashboard.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dashboardBody, []byte("v"+buildinfo.Version)) {
		t.Fatalf("dashboard does not display version %s", buildinfo.Version)
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

func TestDiagnosticLogBatchIsAcknowledged(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/relay/unit-logs/ws"
	headers := http.Header{}
	headers.Set("X-DeskFerry-Role", diagnosticLogRole)
	headers.Set(tunnel.HeaderLogComponent, "home-agent-test")
	headers.Set(tunnel.HeaderLogInstance, "unit")
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"entries":["queued before connect","connected"]}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]int
	if json.Unmarshal(payload, &ack) != nil || ack["accepted"] != 2 {
		t.Fatalf("ack = %s", payload)
	}
}

func TestAgentClientPairAndBridgeBytes(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := dialRole(t, ctx, server.URL, "/relay/unit-bridge/ws", "agent")
	waitForWaitingAgents(t, server.URL, "unit-bridge", 1)
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

func TestV2OnDemandSessionPairingAndBusyRejection(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relayAddr := server.URL + "/relay/unit-v2"
	controlHeaders := http.Header{}
	tunnel.AddProtocolV2Header(controlHeaders)
	controlHeaders.Set(tunnel.HeaderAgentInstance, "unit-agent")
	controlHeaders.Set(tunnel.HeaderAgentServices, tunnel.ServiceScreen)
	controlHeaders.Set(tunnel.HeaderConcurrency, "1")
	control, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleAgentControl, "", controlHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(control)
	if err := tunnel.AwaitControlReady(ctx, control); err != nil {
		t.Fatal(err)
	}

	clientHeaders := http.Header{}
	tunnel.AddProtocolV2Header(clientHeaders)
	clientHeaders.Set(tunnel.HeaderResumable, "1")
	tunnel.AddHeartbeatHeader(clientHeaders)
	tunnel.AddServiceHeader(clientHeaders, tunnel.ServiceScreen)
	client, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleClient, "", clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(client)
	offer, err := tunnel.ReadControlMessage(ctx, control)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.ValidateSessionOffer(offer, "unit-v2", "unit-agent", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !offer.Heartbeat {
		t.Fatal("session offer omitted heartbeat capability")
	}
	if err := tunnel.WriteControlMessage(ctx, control, tunnel.ControlMessage{Type: tunnel.MessageAccept, SessionID: offer.SessionID, Heartbeat: true}); err != nil {
		t.Fatal(err)
	}

	agentHeaders := http.Header{}
	tunnel.AddProtocolV2Header(agentHeaders)
	agentHeaders.Set(tunnel.HeaderAgentInstance, "unit-agent")
	agentHeaders.Set(tunnel.HeaderSessionID, offer.SessionID)
	agentHeaders.Set(tunnel.HeaderResumable, "1")
	tunnel.AddServiceHeader(agentHeaders, tunnel.ServiceScreen)
	agent, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleAgentSession, "", agentHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(agent)
	agentReady, err := tunnel.ReadControlMessage(ctx, agent)
	if err != nil || agentReady.SessionID != offer.SessionID || agentReady.Service != tunnel.ServiceScreen || !agentReady.Heartbeat {
		t.Fatalf("agent ready=%#v error=%v", agentReady, err)
	}
	if ready, err := tunnel.AwaitSessionReadyCompatibleServiceInfo(ctx, client, tunnel.ServiceScreen); err != nil || !ready.ProtocolV2 || ready.SessionID != offer.SessionID || !ready.Heartbeat {
		t.Fatalf("client ready=%+v error=%v", ready, err)
	}

	busyClient, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleClient, "", clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(busyClient)
	started := time.Now()
	_, _, err = tunnel.AwaitSessionReadyCompatible(ctx, busyClient)
	var rejected *tunnel.SessionResultError
	if !errors.As(err, &rejected) || rejected.Result != tunnel.MessageBusy {
		t.Fatalf("busy rejection = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("busy rejection took %s", time.Since(started))
	}

	if err := client.Write(ctx, websocket.MessageBinary, []byte("from-home-v2")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, agent, "from-home-v2")
	status := getStatus(t, server.URL, "unit-v2")
	if len(status.Rooms) != 1 || status.Rooms[0].ControlConnections != 1 || status.Rooms[0].PendingRequests != 0 || status.Rooms[0].BusyRejections != 1 {
		t.Fatalf("unexpected v2 status: %+v", status.Rooms)
	}
}

func TestV2NoAgentIsImmediateTypedResult(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	headers := http.Header{}
	tunnel.AddProtocolV2Header(headers)
	client, err := tunnel.DialWebSocketWithHeaders(ctx, server.URL+"/relay/unit-no-agent", "direct", tunnel.RoleClient, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(client)
	started := time.Now()
	_, _, err = tunnel.AwaitSessionReadyCompatible(ctx, client)
	var rejected *tunnel.SessionResultError
	if !errors.As(err, &rejected) || rejected.Result != tunnel.MessageNoAgent {
		t.Fatalf("no-agent rejection = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("no-agent rejection took %s", time.Since(started))
	}
}

func TestLegacyClientPairsThroughV2Control(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relayAddr := server.URL + "/relay/unit-mixed"
	controlHeaders := http.Header{}
	tunnel.AddProtocolV2Header(controlHeaders)
	controlHeaders.Set(tunnel.HeaderAgentInstance, "unit-agent")
	controlHeaders.Set(tunnel.HeaderAgentServices, tunnel.ServiceSMB)
	controlHeaders.Set(tunnel.HeaderConcurrency, "1")
	control, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleAgentControl, "", controlHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(control)
	if err := tunnel.AwaitControlReady(ctx, control); err != nil {
		t.Fatal(err)
	}
	legacyHeaders := http.Header{}
	tunnel.AddServiceHeader(legacyHeaders, tunnel.ServiceSMB)
	client, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleClient, "", legacyHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(client)
	offer, err := tunnel.ReadControlMessage(ctx, control)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Resumable || offer.Heartbeat || offer.Service != tunnel.ServiceSMB {
		t.Fatalf("mixed offer resumable=%t heartbeat=%t service=%q", offer.Resumable, offer.Heartbeat, offer.Service)
	}
	if err := tunnel.WriteControlMessage(ctx, control, tunnel.ControlMessage{Type: tunnel.MessageAccept, SessionID: offer.SessionID}); err != nil {
		t.Fatal(err)
	}
	agentHeaders := http.Header{}
	tunnel.AddProtocolV2Header(agentHeaders)
	agentHeaders.Set(tunnel.HeaderAgentInstance, "unit-agent")
	agentHeaders.Set(tunnel.HeaderSessionID, offer.SessionID)
	tunnel.AddServiceHeader(agentHeaders, tunnel.ServiceSMB)
	agent, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, "direct", tunnel.RoleAgentSession, "", agentHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.CloseWebSocket(agent)
	if _, err := tunnel.AwaitSessionReady(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if id, err := tunnel.AwaitWebSocketStartSession(ctx, client); err != nil || id != offer.SessionID {
		t.Fatalf("legacy start id=%q error=%v", id, err)
	}
	if err := client.Write(ctx, websocket.MessageBinary, []byte("legacy-smb")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, agent, "legacy-smb")
}

func TestResumablePairReattachesAfterWebSocketDrop(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	capability := http.Header{"X-DeskFerry-Resumable": []string{"1"}}
	agent := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-resume/ws", "agent", capability)
	waitForWaitingAgents(t, server.URL, "unit-resume", 1)
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
	// A proxy can report a transport failure as code 1000 without DeskFerry's
	// logical-close marker. The session must remain available for resumption.
	_ = home.Close(websocket.StatusNormalClosure, "")
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

func TestResumablePairReconstructsAfterRelayRestart(t *testing.T) {
	server := httptest.NewServer(newServer())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionID := strings.Repeat("a", 32)
	proof := strings.Repeat("p", 43)
	resumeHeaders := func(side string) http.Header {
		return http.Header{
			"X-DeskFerry-Session":      []string{sessionID},
			"X-DeskFerry-Session-Side": []string{side},
			"X-DeskFerry-Room-Proof":   []string{proof},
			"X-DeskFerry-Service":      []string{serviceRDP},
		}
	}

	earlyHome := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-restart/ws", resumeRole, resumeHeaders("client"))
	if _, _, err := earlyHome.Read(ctx); websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("early client resume error = %v, want retryable close", err)
	}

	// The agent-side resume is allowed to reconstruct the protected room and
	// session after a relay process restart. The client then proves knowledge
	// of the same room credential and random session ID.
	agent := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-restart/ws", resumeRole, resumeHeaders("agent"))
	home := dialRoleHeaders(t, ctx, server.URL, "/relay/unit-restart/ws", resumeRole, resumeHeaders("client"))
	defer agent.Close(websocket.StatusNormalClosure, "session closed")
	defer home.Close(websocket.StatusNormalClosure, "session closed")
	expectText(t, ctx, agent, resumeMessage+" "+sessionID)
	expectText(t, ctx, home, resumeMessage+" "+sessionID)

	if err := home.Write(ctx, websocket.MessageBinary, []byte("after-relay-restart")); err != nil {
		t.Fatal(err)
	}
	expectBinary(t, ctx, agent, "after-relay-restart")
	status := getStatus(t, server.URL, "unit-restart")
	if len(status.Rooms) != 1 || status.Rooms[0].ActivePairs != 1 || status.Rooms[0].TotalPairs != 1 {
		t.Fatalf("reconstructed bridge counts = %+v", status.Rooms)
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
	waitForWaitingAgents(t, server.URL, "unit-resume-stream", 1)
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

func TestProtocolV2ControlRemovesSameAgentLegacySlots(t *testing.T) {
	room := NewRelayRoom("rollout")
	for slot := 1; slot <= 4; slot++ {
		identity := AgentIdentity{Instance: "agent-a", Slot: fmt.Sprint(slot), Service: serviceRDP}
		room.EnqueueAgent(nil, "legacy", identity, true, serviceRDP)
	}
	other, _ := room.EnqueueAgent(nil, "other", AgentIdentity{Instance: "agent-b", Slot: "1", Service: serviceRDP}, true, serviceRDP)
	if removed := room.RemoveLegacyAgents("agent-a"); removed != 4 {
		t.Fatalf("removed legacy slots = %d, want 4", removed)
	}
	if got := room.TryTakeAgent(serviceRDP); got != other {
		t.Fatalf("remaining legacy agent = %p, want %p", got, other)
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

func waitForWaitingAgents(t *testing.T, baseURL, room string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var status StatusSnapshot
	for time.Now().Before(deadline) {
		status = getStatus(t, baseURL, room)
		if len(status.Rooms) == 1 && status.Rooms[0].WaitingAgents == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("room %q waiting agents did not reach %d before timeout: %+v", room, want, status.Rooms)
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
