package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExternalHTTPProxyAuthentication is an opt-in probe for the HTTP
// transport used by the non-CONNECT fallback. On Windows it verifies that a
// forward proxy can authenticate the current user before carrying requests.
func TestExternalHTTPProxyAuthentication(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if baseURL == "" || proxySpec == "" || strings.EqualFold(proxySpec, "direct") {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL and DESKFERRY_COMPAT_PROXY are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/relay/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpStreamHTTPClient(baseURL, proxySpec).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy health probe returned HTTP %s", resp.Status)
	}
	_ = resp.Body.Close()
}

// TestExternalRelayProxyFallbackSelection verifies automatic WebSocket-first
// selection through a real proxy, then confirms the learned proxy capability
// makes a second connection go directly to the acknowledged HTTP transport.
func TestExternalRelayProxyFallbackSelection(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if baseURL == "" || proxySpec == "" || strings.EqualFold(proxySpec, "direct") {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL and DESKFERRY_COMPAT_PROXY are not set")
	}
	ClearProxyHTTPStreamPreferred(proxySpec)
	ClearProxyCONNECTUnsupported(proxySpec)
	defer ClearProxyHTTPStreamPreferred(proxySpec)
	defer ClearProxyCONNECTUnsupported(proxySpec)
	relayAddr := baseURL + "/relay/compat-proxy-" + time.Now().Format("150405.000000")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	first, err := DialMessageConn(ctx, relayAddr, proxySpec, RoleProbe, "")
	if err != nil {
		t.Fatal(err)
	}
	firstElapsed := time.Since(started)
	firstProtocol := MessageConnProtocol(first)
	CloseMessageConn(first)
	if firstProtocol != "http-stream" {
		t.Fatalf("first protocol = %q, want http-stream", firstProtocol)
	}
	if !ProxyHTTPStreamPreferred(proxySpec) {
		t.Fatal("successful proxy fallback did not record HTTP-stream preference")
	}

	started = time.Now()
	second, err := DialMessageConn(ctx, relayAddr, proxySpec, RoleProbe, "")
	if err != nil {
		t.Fatal(err)
	}
	secondElapsed := time.Since(started)
	secondProtocol := MessageConnProtocol(second)
	CloseMessageConn(second)
	if secondProtocol != "http-stream" {
		t.Fatalf("second protocol = %q, want http-stream", secondProtocol)
	}
	if secondElapsed >= firstElapsed {
		t.Fatalf("remembered fallback did not improve setup: first=%s second=%s", firstElapsed, secondElapsed)
	}
	t.Logf("proxy fallback selected first=%s second=%s", firstElapsed, secondElapsed)
}

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
	compatTransfer(t, ctx, clientConn, agentConn, bytes.Repeat([]byte{0xa5}, resumableChunkSize+resumableHeaderLen))
	// Interrupt the underlying transport without closing the logical stream.
	// Code-1000 handling is covered by the implementation-specific tests; using
	// Close here would race the resumable connection's WebSocket reader.
	_ = clientWS.CloseNow()
	compatTransfer(t, ctx, agentConn, clientConn, []byte("after-resume"))
}

// TestExternalRelayV2OnDemand exercises the same v2 control/data flow against
// any of the Go, Azure .NET, or Python relay implementations.
func TestExternalRelayV2OnDemand(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	if baseURL == "" {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL is not set")
	}
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if proxySpec == "" {
		proxySpec = "direct"
	}
	room := "compat-v2-" + time.Now().Format("150405.000000")
	relayAddr := baseURL + "/relay/" + room
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	controlHeaders := http.Header{}
	AddProtocolV2Header(controlHeaders)
	controlHeaders.Set(HeaderAgentInstance, "compat-agent")
	controlHeaders.Set(HeaderAgentServices, ServiceRDP)
	controlHeaders.Set(HeaderConcurrency, "2")
	control, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleAgentControl, "", controlHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(control)
	if err := AwaitControlReady(ctx, control); err != nil {
		t.Fatal(err)
	}

	clientHeaders := http.Header{}
	AddProtocolV2Header(clientHeaders)
	AddServiceHeader(clientHeaders, ServiceRDP)
	client, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleClient, "", clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(client)
	offer, err := ReadControlMessage(ctx, control)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionOffer(offer, room, "compat-agent", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := WriteControlMessage(ctx, control, ControlMessage{Type: MessageAccept, SessionID: offer.SessionID}); err != nil {
		t.Fatal(err)
	}
	agentHeaders := http.Header{}
	AddProtocolV2Header(agentHeaders)
	agentHeaders.Set(HeaderAgentInstance, "compat-agent")
	agentHeaders.Set(HeaderSessionID, offer.SessionID)
	AddServiceHeader(agentHeaders, ServiceRDP)
	agent, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleAgentSession, "", agentHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(agent)
	if _, err := AwaitSessionReady(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AwaitSessionReadyCompatible(ctx, client); err != nil {
		t.Fatal(err)
	}
	agentStream := WebSocketNetConn(ctx, agent)
	clientStream := WebSocketNetConn(ctx, client)
	compatTransfer(t, ctx, clientStream, agentStream, []byte("protocol-v2"))
}

// TestExternalRelayHTTPStreamV2 exercises the non-CONNECT wire protocol
// directly so every relay implementation is checked against the shared Go
// client without depending on a particular forward-proxy test harness.
func TestExternalRelayHTTPStreamV2(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	if baseURL == "" {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL is not set")
	}
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if proxySpec == "" {
		proxySpec = "direct"
	}
	room := "compat-http-" + time.Now().Format("150405.000000")
	relayAddr := baseURL + "/relay/" + room
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()

	controlHeaders := http.Header{}
	AddProtocolV2Header(controlHeaders)
	controlHeaders.Set(HeaderAgentInstance, "compat-http-agent")
	controlHeaders.Set(HeaderAgentServices, ServiceRDP)
	controlHeaders.Set(HeaderConcurrency, "2")
	control, err := DialHTTPStreamWithHeaders(ctx, relayAddr, proxySpec, RoleAgentControl, "", controlHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseMessageConn(control)
	if err := AwaitControlReady(ctx, control); err != nil {
		t.Fatal(err)
	}
	t.Logf("control ready after %s", time.Since(started))

	clientHeaders := http.Header{}
	AddProtocolV2Header(clientHeaders)
	AddServiceHeader(clientHeaders, ServiceRDP)
	client, err := DialHTTPStreamWithHeaders(ctx, relayAddr, proxySpec, RoleClient, "", clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseMessageConn(client)
	t.Logf("client ready after %s", time.Since(started))
	offer, err := ReadControlMessage(ctx, control)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("offer received after %s expires=%s", time.Since(started), offer.ExpiresAt.Format(time.RFC3339Nano))
	if err := ValidateSessionOffer(offer, room, "compat-http-agent", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := WriteControlMessage(ctx, control, ControlMessage{Type: MessageAccept, SessionID: offer.SessionID}); err != nil {
		t.Fatal(err)
	}

	agentHeaders := http.Header{}
	AddProtocolV2Header(agentHeaders)
	agentHeaders.Set(HeaderAgentInstance, "compat-http-agent")
	agentHeaders.Set(HeaderSessionID, offer.SessionID)
	AddServiceHeader(agentHeaders, ServiceRDP)
	agent, err := DialHTTPStreamWithHeaders(ctx, relayAddr, proxySpec, RoleAgentSession, "", agentHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseMessageConn(agent)
	if _, err := AwaitSessionReady(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AwaitSessionReadyCompatible(ctx, client); err != nil {
		t.Fatal(err)
	}
	compatTransfer(t, ctx, MessageNetConn(ctx, client), MessageNetConn(ctx, agent), []byte("http-stream-v2"))
}

func TestExternalRelayLegacyClientThroughV2Control(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("DESKFERRY_COMPAT_RELAY_URL"), "/")
	if baseURL == "" {
		t.Skip("DESKFERRY_COMPAT_RELAY_URL is not set")
	}
	proxySpec := strings.TrimSpace(os.Getenv("DESKFERRY_COMPAT_PROXY"))
	if proxySpec == "" {
		proxySpec = "direct"
	}
	room := "compat-mixed-" + time.Now().Format("150405.000000")
	relayAddr := baseURL + "/relay/" + room
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	controlHeaders := http.Header{}
	AddProtocolV2Header(controlHeaders)
	controlHeaders.Set(HeaderAgentInstance, "compat-agent")
	controlHeaders.Set(HeaderAgentServices, ServiceSMB)
	controlHeaders.Set(HeaderConcurrency, "1")
	control, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleAgentControl, "", controlHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(control)
	if err := AwaitControlReady(ctx, control); err != nil {
		t.Fatal(err)
	}
	legacyHeaders := http.Header{}
	AddServiceHeader(legacyHeaders, ServiceSMB)
	client, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleClient, "", legacyHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(client)
	offer, err := ReadControlMessage(ctx, control)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Resumable || offer.Service != ServiceSMB {
		t.Fatalf("mixed offer resumable=%t service=%q", offer.Resumable, offer.Service)
	}
	if err := WriteControlMessage(ctx, control, ControlMessage{Type: MessageAccept, SessionID: offer.SessionID}); err != nil {
		t.Fatal(err)
	}
	agentHeaders := http.Header{}
	AddProtocolV2Header(agentHeaders)
	agentHeaders.Set(HeaderAgentInstance, "compat-agent")
	agentHeaders.Set(HeaderSessionID, offer.SessionID)
	AddServiceHeader(agentHeaders, ServiceSMB)
	agent, err := DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, RoleAgentSession, "", agentHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseWebSocket(agent)
	if _, err := AwaitSessionReady(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if id, err := AwaitWebSocketStartSession(ctx, client); err != nil || id != offer.SessionID {
		t.Fatalf("legacy start id=%q error=%v", id, err)
	}
	compatTransfer(t, ctx, WebSocketNetConn(ctx, client), WebSocketNetConn(ctx, agent), []byte("mixed-version"))
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
