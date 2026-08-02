package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestWebSocketEndpointUsesRelayPath(t *testing.T) {
	endpoint, err := WebSocketEndpoint("https://test-officialwebsite.azurewebsites.net/relay/")
	if err != nil {
		t.Fatalf("WebSocketEndpoint: %v", err)
	}
	want := "wss://test-officialwebsite.azurewebsites.net/relay/ws"
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestWebSocketEndpointPreservesNamedRoomPath(t *testing.T) {
	endpoint, err := WebSocketEndpoint("https://test-officialwebsite.azurewebsites.net/relay/workdesk")
	if err != nil {
		t.Fatalf("WebSocketEndpoint: %v", err)
	}
	want := "wss://test-officialwebsite.azurewebsites.net/relay/workdesk/ws"
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestRelayRoomTokenUsesPathRoom(t *testing.T) {
	token := RelayRoomToken("https://test-officialwebsite.azurewebsites.net/relay/workdesk", "")
	if token != "workdesk" {
		t.Fatalf("token = %q, want workdesk", token)
	}
}

func TestWebSocketEndpointDefaultsToRelayPath(t *testing.T) {
	endpoint, err := WebSocketEndpoint("https://test-officialwebsite.azurewebsites.net/")
	if err != nil {
		t.Fatalf("WebSocketEndpoint: %v", err)
	}
	want := "wss://test-officialwebsite.azurewebsites.net/relay/ws"
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestHTTPRelayThroughProxyUsesConnectTunnel(t *testing.T) {
	client := webSocketHTTPClient("http://217.142.228.117/relay/b", "http://192.9.200.25:3128")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil, want proxy CONNECT tunnel dialer")
	}
	if transport.Proxy != nil {
		t.Fatal("Proxy is set, want direct WebSocket handshake over CONNECT tunnel")
	}
}

func TestHTTPSRelayThroughProxyUsesStandardProxyTransport(t *testing.T) {
	client := webSocketHTTPClient("https://test-officialwebsite.azurewebsites.net/relay/b", "http://192.9.200.25:3128")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil, want DNS-resilient standard proxy transport")
	}
	if transport.Proxy == nil {
		t.Fatal("Proxy is nil, want standard HTTPS proxy transport")
	}
}

func TestDNSFallbackDialerUsesCachedAddressWhenLookupFails(t *testing.T) {
	lookupCalls := 0
	dialed := make([]string, 0, 2)
	d := &dnsFallbackDialer{
		cache: make(map[string][]net.IPAddr),
		lookup: func(context.Context, string) ([]net.IPAddr, error) {
			lookupCalls++
			if lookupCalls == 1 {
				return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil
			}
			return nil, errors.New("temporary DNS failure")
		},
		dial: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return &stubConn{}, nil
		},
	}

	for range 2 {
		conn, err := d.DialContext(context.Background(), "tcp", "relay.example:443")
		if err != nil {
			t.Fatalf("DialContext: %v", err)
		}
		_ = conn.Close()
	}
	if len(dialed) != 2 || dialed[0] != "192.0.2.10:443" || dialed[1] != "192.0.2.10:443" {
		t.Fatalf("dialed = %v, want cached relay address twice", dialed)
	}
}

type stubConn struct{ net.Conn }

func (*stubConn) Close() error { return nil }

func TestHTTPSProxyURLIsAccepted(t *testing.T) {
	proxyURL, err := resolveProxyURL("relay.example:443", "https://proxy.example:8443")
	if err != nil {
		t.Fatalf("resolveProxyURL: %v", err)
	}
	if proxyURL.Scheme != "https" || proxyURL.Host != "proxy.example:8443" {
		t.Fatalf("proxy URL = %s, want https://proxy.example:8443", proxyURL)
	}
}

func TestHTTPRelayThroughHTTPSProxyUsesConnectTunnel(t *testing.T) {
	client := webSocketHTTPClient("http://relay.example/relay/b", "https://proxy.example:8443")
	transport := client.Transport.(*http.Transport)
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil, want TLS proxy CONNECT tunnel dialer")
	}
	if transport.Proxy != nil {
		t.Fatal("Proxy is set, want direct WebSocket handshake over CONNECT tunnel")
	}
}
