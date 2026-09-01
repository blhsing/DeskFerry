package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestHTTPStreamForwardProxyUsesIntegratedAuthentication(t *testing.T) {
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
			if req.Method != http.MethodGet || !req.URL.IsAbs() {
				serverErr <- fmt.Errorf("authentication probe method=%s URL=%s", req.Method, req.URL)
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
				_, err = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
			}
			if err != nil {
				serverErr <- err
				return
			}
		}

		req, err := http.ReadRequest(reader)
		if err != nil {
			serverErr <- err
			return
		}
		if req.Method != http.MethodGet || !req.URL.IsAbs() || req.URL.Host != "relay.example" {
			serverErr <- fmt.Errorf("forwarded request method=%s URL=%s", req.Method, req.URL)
			return
		}
		_, err = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		serverErr <- err
	}()

	client := httpStreamHTTPClientWithAuth("http://relay.example/relay/b", "http://"+listener.Addr().String(), func() (integratedProxyAuthenticator, error) {
		return &fakeProxyAuthenticator{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://relay.example/relay/b/stream/test/down", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("response body=%q error=%v", body, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSHTTPStreamUsesIntegratedAuthenticationForCONNECT(t *testing.T) {
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
			if req.Method != http.MethodConnect || req.Host != "relay.example:443" {
				serverErr <- fmt.Errorf("CONNECT request method=%s host=%s", req.Method, req.Host)
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

	client := httpStreamHTTPClientWithAuth("https://relay.example/relay/b", "http://"+listener.Addr().String(), func() (integratedProxyAuthenticator, error) {
		return &fakeProxyAuthenticator{}, nil
	})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("HTTPS stream transport retained net/http proxy handling instead of authenticated CONNECT")
	}
	conn, err := transport.DialContext(context.Background(), "tcp", "relay.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestCONNECTRejectionIsNotPersistedBeforePlainHTTPFallbackWorks(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "CONNECT is not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "unexpected forward request", http.StatusBadRequest)
	}))
	defer proxy.Close()
	defer ClearProxyHTTPStreamPreferred(proxy.URL)
	defer ClearProxyCONNECTUnsupported(proxy.URL)
	defer clearProxyCONNECTRejectedCandidate(proxy.URL)
	MarkProxyHTTPStreamPreferred(proxy.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DialMessageConnWithHeaders(ctx, "https://relay.example/relay/unit", proxy.URL, RoleProbe, "", nil)
	if !errors.Is(err, ErrProxyCONNECTRejected) {
		t.Fatalf("HTTPS dial error = %v, want CONNECT rejection", err)
	}
	if ProxyCONNECTUnsupported(proxy.URL) {
		t.Fatal("CONNECT rejection was persisted before a plain-HTTP POST/GET path succeeded")
	}
}

func TestHTTPStreamFallbackWithoutProxyCONNECT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	accepted := make(chan struct{}, 1)
	serverClosed := make(chan struct{}, 4)
	streams := NewHTTPStreamServer(func(ctx context.Context, conn MessageConn, _ *http.Request, _ string) {
		defer func() { serverClosed <- struct{}{} }()
		select {
		case accepted <- struct{}{}:
		default:
		}
		for {
			typ, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if err := conn.Write(ctx, typ, append([]byte("echo:"), payload...)); err != nil {
				return
			}
		}
	})
	defer streams.Close()

	var upstreams atomic.Int32
	var downstreams atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || parts[0] != "relay" || parts[2] != "stream" {
			http.NotFound(w, r)
			return
		}
		direction := parts[4]
		if direction == "up" && upstreams.Add(1) == 1 {
			http.Error(w, "forced upstream interruption", http.StatusBadGateway)
			return
		}
		if direction == "down" && downstreams.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		streams.Serve(w, r, parts[1], parts[3], direction)
	}))
	defer origin.Close()

	var connectAttempts atomic.Int32
	proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
	proxyTransport.Proxy = nil
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connectAttempts.Add(1)
			http.Error(w, "CONNECT is not allowed", http.StatusMethodNotAllowed)
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.Header.Del("Proxy-Authorization")
		// Model managed front ends such as Azure App Service that do not
		// forward a chunked upload until its request body reaches EOF.
		if request.Method == http.MethodPost {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.ContentLength = int64(len(body))
		}
		response, err := proxyTransport.RoundTrip(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
			buffer := make([]byte, 4096)
			for {
				n, readErr := response.Body.Read(buffer)
				if n > 0 {
					if _, err := w.Write(buffer[:n]); err != nil {
						return
					}
					flusher.Flush()
				}
				if readErr != nil {
					return
				}
			}
		}
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	defer ClearProxyHTTPStreamPreferred(proxy.URL)
	defer ClearProxyCONNECTUnsupported(proxy.URL)

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := DialMessageConnWithHeaders(dialCtx, origin.URL+"/relay/unit", proxy.URL, RoleProbe, "", nil)
	if err != nil {
		dialCancel()
		t.Fatalf("dial fallback: %v", err)
	}
	// Dial contexts are handshake-only, matching native WebSocket behavior.
	// Existing app callers cancel them immediately after a successful pair.
	dialCancel()
	if got := connectAttempts.Load(); got != 1 {
		t.Fatalf("WebSocket CONNECT attempts = %d, want 1", got)
	}
	if !ProxyHTTPStreamPreferred(proxy.URL) {
		t.Fatal("successful fallback did not record the POST/GET preference")
	}
	if !ProxyCONNECTUnsupported(proxy.URL) {
		t.Fatal("rejected CONNECT followed by successful POST/GET was not recorded")
	}

	// A successful fallback remembers the proxy capability, so a subsequent
	// RDP, WinRM, SMB, or presence connection does not repeat a WebSocket probe
	// that this proxy is known to reject.
	preferredCtx, preferredCancel := context.WithTimeout(ctx, 2*time.Second)
	preferred, err := DialMessageConnWithHeaders(preferredCtx, origin.URL+"/relay/unit", proxy.URL, RoleProbe, "", nil)
	preferredCancel()
	if err != nil {
		t.Fatalf("dial remembered fallback: %v", err)
	}
	CloseMessageConn(preferred)
	if got := connectAttempts.Load(); got != 1 {
		t.Fatalf("remembered fallback repeated WebSocket CONNECT probe: attempts=%d", got)
	}
	if _, err := DialMessageConnWithHeaders(ctx, "https://relay.example/relay/unit", proxy.URL, RoleProbe, "", nil); err == nil || !errors.Is(err, ErrProxyCONNECTRejected) {
		t.Fatalf("remembered CONNECT rejection accepted HTTPS relay: %v", err)
	}

	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("relay did not accept the HTTP stream")
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte("control")); err != nil {
		t.Fatalf("write text: %v", err)
	}
	typ, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read text: %v (CONNECT=%d upstream=%d downstream=%d)", err, connectAttempts.Load(), upstreams.Load(), downstreams.Load())
	}
	if typ != websocket.MessageText || string(payload) != "echo:control" {
		t.Fatalf("unexpected text echo type=%v payload=%q", typ, payload)
	}

	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0, 1, 2, 3}); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	typ, payload, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if typ != websocket.MessageBinary || string(payload) != string([]byte("echo:\x00\x01\x02\x03")) {
		t.Fatalf("unexpected binary echo type=%v payload=%v", typ, payload)
	}

	if connectAttempts.Load() == 0 {
		t.Fatal("WebSocket path did not attempt CONNECT")
	}
	if upstreams.Load() < 2 || downstreams.Load() < 2 {
		t.Fatalf("stream halves did not recover: upstream=%d downstream=%d", upstreams.Load(), downstreams.Load())
	}

	CloseMessageConn(conn)
	select {
	case <-serverClosed:
	case <-ctx.Done():
		t.Fatal("graceful close did not reach the relay through the buffered upload")
	}
}
