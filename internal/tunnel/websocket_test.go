package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRoomPasswordProofIsScopedToRoomAndDoesNotExposePassword(t *testing.T) {
	first := RoomPasswordProof("https://relay.example/relay/alpha", "", "correct horse")
	second := RoomPasswordProof("https://relay.example/relay/beta", "", "correct horse")
	if first == "" || first == second {
		t.Fatalf("proofs are not room scoped: %q %q", first, second)
	}
	if want := "0uUP8ZmTPoS4RX9lmqvjaIA4RT9k-hgqpFVzNSyzs1s"; first != want {
		t.Fatalf("proof = %q, want cross-platform vector %q", first, want)
	}
	if strings.Contains(first, "correct") {
		t.Fatalf("proof contains password: %q", first)
	}
	if got := RoomPasswordProof("https://relay.example/relay/alpha", "", ""); got != "" {
		t.Fatalf("empty password proof = %q", got)
	}
}

func TestRoomAndServiceHeaders(t *testing.T) {
	headers := http.Header{}
	AddRoomPasswordHeader(headers, "https://relay.example/relay/alpha", "", "secret")
	AddServiceHeader(headers, ServiceWinRM)
	if headers.Get(HeaderRoomProof) == "" {
		t.Fatal("missing room proof")
	}
	if got := headers.Get(HeaderService); got != ServiceWinRM {
		t.Fatalf("service = %q", got)
	}
}

func TestSMBServiceHeader(t *testing.T) {
	headers := http.Header{}
	AddServiceHeader(headers, ServiceSMB)
	if got := headers.Get(HeaderService); got != "smb" {
		t.Fatalf("service = %q", got)
	}
}

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

func TestHTTPSRelayThroughProxyUsesConnectTunnel(t *testing.T) {
	client := webSocketHTTPClient("https://test-officialwebsite.azurewebsites.net/relay/b", "http://192.9.200.25:3128")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil, want proxy CONNECT tunnel dialer")
	}
	if transport.Proxy != nil {
		t.Fatal("Proxy is set, want authenticated CONNECT tunnel")
	}
}

type fakeProxyAuthenticator struct{}

func (*fakeProxyAuthenticator) Scheme() string       { return "NTLM" }
func (*fakeProxyAuthenticator) InitialToken() []byte { return []byte("initial") }
func (*fakeProxyAuthenticator) Close() error         { return nil }
func (*fakeProxyAuthenticator) NextToken(challenge []byte) ([]byte, error) {
	if string(challenge) != "challenge" {
		return nil, fmt.Errorf("challenge = %q", challenge)
	}
	return []byte("response"), nil
}

func TestProxyConnectUsesIntegratedAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for attempt, expected := range []string{"initial", "response"} {
			req, err := http.ReadRequest(reader)
			if err != nil {
				serverErr <- err
				return
			}
			want := "NTLM " + base64.StdEncoding.EncodeToString([]byte(expected))
			if got := req.Header.Get("Proxy-Authorization"); got != want {
				serverErr <- fmt.Errorf("attempt %d authorization = %q, want %q", attempt, got, want)
				return
			}
			if attempt == 0 {
				challenge := base64.StdEncoding.EncodeToString([]byte("challenge"))
				_, err = fmt.Fprintf(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: NTLM %s\r\nContent-Length: 0\r\n\r\n", challenge)
			} else {
				_, err = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
			}
			if err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	proxyURL := &url.URL{Scheme: "http", Host: listener.Addr().String()}
	dial := proxyConnectDialContextWithAuth(proxyURL, func() (integratedProxyAuthenticator, error) {
		return &fakeProxyAuthenticator{}, nil
	})
	conn, err := dial(context.Background(), "tcp", "relay.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestTransportFailureResultClassifiesProxyFailures(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("dial: %w", ErrProxyAuthentication), "proxy-authentication-failure"},
		{fmt.Errorf("dial: %w", ErrProxyCONNECTRejected), "proxy-connect-rejected"},
		{fmt.Errorf("dial: %w", ErrWebSocketUpgradeRejected), "websocket-upgrade-rejected"},
		{errors.New("connection reset"), "transport-failure"},
	}
	for _, test := range tests {
		if got := TransportFailureResult(test.err); got != test.want {
			t.Errorf("TransportFailureResult(%v) = %q, want %q", test.err, got, test.want)
		}
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

func TestRelayBaseAndRoomURLMigration(t *testing.T) {
	legacy := []string{
		"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
		"http://217.142.228.117/relay/workdesk",
	}
	bases, room, err := SplitRelayRoomURLs(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if room != "workdesk" || len(bases) != 2 || bases[0] != "https://test-officialwebsite.azurewebsites.net/relay" || bases[1] != "http://217.142.228.117/relay" {
		t.Fatalf("bases=%v room=%q", bases, room)
	}
	got, err := RelayRoomURL(bases[0], room)
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy[0] {
		t.Fatalf("room URL=%q", got)
	}
}
