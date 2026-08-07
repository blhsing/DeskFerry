package tunnel

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"deskferry/internal/buildinfo"

	"nhooyr.io/websocket"
)

type dnsFallbackDialer struct {
	mu     sync.RWMutex
	cache  map[string][]net.IPAddr
	lookup func(context.Context, string) ([]net.IPAddr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

var resilientDNSDialer = newDNSFallbackDialer()

func newDNSFallbackDialer() *dnsFallbackDialer {
	var dialer net.Dialer
	return &dnsFallbackDialer{
		cache:  make(map[string][]net.IPAddr),
		lookup: net.DefaultResolver.LookupIPAddr,
		dial:   dialer.DialContext,
	}
}

func (d *dnsFallbackDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return d.dial(ctx, network, address)
	}

	addresses, lookupErr := d.lookup(ctx, host)
	if lookupErr == nil && len(addresses) == 0 {
		lookupErr = fmt.Errorf("resolver returned no addresses")
	}
	if lookupErr == nil && len(addresses) > 0 {
		d.mu.Lock()
		d.cache[host] = append([]net.IPAddr(nil), addresses...)
		d.mu.Unlock()
	} else {
		d.mu.RLock()
		addresses = append([]net.IPAddr(nil), d.cache[host]...)
		d.mu.RUnlock()
		if len(addresses) == 0 {
			return nil, fmt.Errorf("resolve %s: %w", host, lookupErr)
		}
	}

	var dialErr error
	for _, candidate := range addresses {
		if network == "tcp4" && candidate.IP.To4() == nil {
			continue
		}
		if network == "tcp6" && candidate.IP.To4() != nil {
			continue
		}
		conn, err := d.dial(ctx, network, net.JoinHostPort(candidate.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	if dialErr == nil {
		dialErr = fmt.Errorf("no %s address available", network)
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("dial %s using cached DNS after %v: %w", address, lookupErr, dialErr)
	}
	return nil, fmt.Errorf("dial %s: %w", address, dialErr)
}

const (
	RoleProbe         = "probe"
	RoleHomeAgent     = "home-agent"
	RoleResume        = "resume"
	RoleDiagnosticLog = "diagnostic-log"

	webSocketStartMessage  = "start"
	webSocketResumeMessage = "resume"

	HeaderResumable    = "X-DeskFerry-Resumable"
	HeaderSessionID    = "X-DeskFerry-Session"
	HeaderSessionSide  = "X-DeskFerry-Session-Side"
	HeaderRoomProof    = "X-DeskFerry-Room-Proof"
	HeaderService      = "X-DeskFerry-Service"
	HeaderLogComponent = "X-DeskFerry-Log-Component"
	HeaderLogInstance  = "X-DeskFerry-Log-Instance"

	ServiceRDP    = "rdp"
	ServiceWinRM  = "winrm"
	ServiceSMB    = "smb"
	ServiceScreen = "screen"

	// Resumable data messages contain a 64 KiB payload plus framing. Keep a
	// bounded amount of headroom above that protocol maximum.
	webSocketReadLimit = 1 << 20
)

// RoomPasswordProof derives the credential presented to a relay. The password
// itself is never placed in a relay URL, HTTP header, or diagnostic log.
func RoomPasswordProof(relayAddr, token, password string) string {
	if password == "" {
		return ""
	}
	room := strings.ToLower(strings.TrimSpace(RelayRoomToken(relayAddr, token)))
	sum := sha256.Sum256([]byte("DeskFerry room credential v1\x00" + room + "\x00" + password))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func AddRoomPasswordHeader(headers http.Header, relayAddr, token, password string) {
	if proof := RoomPasswordProof(relayAddr, token, password); proof != "" {
		headers.Set(HeaderRoomProof, proof)
	}
}

func AddServiceHeader(headers http.Header, service string) {
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" {
		service = ServiceRDP
	}
	headers.Set(HeaderService, service)
}

func IsWebSocketRelay(relayAddr string) bool {
	scheme := relayScheme(relayAddr)
	return scheme == "http" || scheme == "https" || scheme == "ws" || scheme == "wss"
}

func WebSocketEndpoint(relayAddr string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay URL must include a host")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported relay URL scheme %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/relay/ws"
	} else if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/ws") && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/dashboard") {
		u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	}
	return u.String(), nil
}

func HostFromRelayAddress(relayAddr string) string {
	if IsWebSocketRelay(relayAddr) {
		u, err := url.Parse(strings.TrimSpace(relayAddr))
		if err == nil {
			return u.Hostname()
		}
	}
	host, _, err := net.SplitHostPort(relayAddr)
	if err != nil {
		return relayAddr
	}
	return host
}

func RelayRoomToken(relayAddr, configuredToken string) string {
	if token := strings.TrimSpace(configuredToken); token != "" {
		return token
	}
	u, err := url.Parse(strings.TrimSpace(relayAddr))
	if err == nil {
		if room := RoomFromRelayPath(u.Path); room != "" {
			return room
		}
		if token := strings.TrimSpace(u.Query().Get("room")); token != "" {
			return token
		}
		if token := strings.TrimSpace(u.Query().Get("token")); token != "" {
			return token
		}
	}
	return "default"
}

func RoomFromRelayPath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] != "relay" {
		return ""
	}
	if parts[1] == "ws" || parts[1] == "status" || parts[1] == "health" || parts[1] == "dashboard" {
		return ""
	}
	return parts[1]
}

// RelayServiceBaseURL returns the relay service URL without a named room.
// It accepts both the new base-URL form and legacy room URLs.
func RelayServiceBaseURL(relayAddr string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay URL must include a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported relay URL scheme %q", u.Scheme)
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/ws") {
		path = strings.TrimSuffix(path, "/ws")
	}
	if room := RoomFromRelayPath(path); room != "" {
		path = strings.TrimSuffix(path, "/"+room)
	}
	if path == "" || path == "/" {
		path = "/relay"
	}
	u.Path = path
	query := u.Query()
	query.Del("room")
	query.Del("token")
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// RelayRoomURL combines a relay service base URL with a profile room name.
func RelayRoomURL(baseURL, room string) (string, error) {
	base, err := RelayServiceBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	room = strings.TrimSpace(room)
	if room == "" {
		return "", fmt.Errorf("room name is required")
	}
	if strings.ContainsAny(room, "/\\?#") || room == "." || room == ".." {
		return "", fmt.Errorf("room name must not contain URL separators")
	}
	u, _ := url.Parse(base)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + url.PathEscape(room)
	return u.String(), nil
}

// SplitRelayRoomURLs migrates legacy room URLs into service bases plus one
// common room name.
func SplitRelayRoomURLs(relayAddrs []string) ([]string, string, error) {
	bases := make([]string, 0, len(relayAddrs))
	room := ""
	for _, relayAddr := range relayAddrs {
		path := ""
		if parsed, err := url.Parse(strings.TrimSpace(relayAddr)); err == nil {
			path = parsed.Path
		}
		currentRoom := RoomFromRelayPath(path)
		if currentRoom == "" {
			if parsed, err := url.Parse(strings.TrimSpace(relayAddr)); err == nil {
				currentRoom = strings.TrimSpace(parsed.Query().Get("room"))
				if currentRoom == "" {
					currentRoom = strings.TrimSpace(parsed.Query().Get("token"))
				}
			}
		}
		if currentRoom != "" {
			if room == "" {
				room = currentRoom
			} else if !strings.EqualFold(room, currentRoom) {
				return nil, "", fmt.Errorf("all relay URLs must use the same room name")
			}
		}
		base, err := RelayServiceBaseURL(relayAddr)
		if err != nil {
			return nil, "", err
		}
		seen := false
		for _, existing := range bases {
			if strings.EqualFold(existing, base) {
				seen = true
				break
			}
		}
		if !seen {
			bases = append(bases, base)
		}
	}
	return bases, room, nil
}

func DialWebSocketStream(ctx context.Context, relayAddr, proxySpec, role, token string) (net.Conn, error) {
	c, err := DialWebSocket(ctx, relayAddr, proxySpec, role, token)
	if err != nil {
		return nil, err
	}
	if role == RoleClient {
		if err := AwaitWebSocketStart(ctx, c); err != nil {
			CloseWebSocket(c)
			return nil, err
		}
	}
	return WebSocketNetConn(ctx, c), nil
}

func WebSocketNetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	return websocket.NetConn(ctx, c, websocket.MessageBinary)
}

func DialWebSocket(ctx context.Context, relayAddr, proxySpec, role, token string) (*websocket.Conn, error) {
	return DialWebSocketWithHeaders(ctx, relayAddr, proxySpec, role, token, nil)
}

func DialWebSocketWithHeaders(ctx context.Context, relayAddr, proxySpec, role, token string, extraHeaders http.Header) (*websocket.Conn, error) {
	if err := validateWebSocketRole(role); err != nil {
		return nil, err
	}
	token = RelayRoomToken(relayAddr, token)
	endpoint, err := WebSocketEndpoint(relayAddr)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("X-DeskFerry-Role", role)
	header.Set("X-TunnelDesktop-Role", role)
	header.Set("User-Agent", "DeskFerry/"+buildinfo.Version)
	for name, values := range extraHeaders {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				header.Add(name, value)
			}
		}
	}
	c, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:      webSocketHTTPClient(relayAddr, proxySpec),
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("websocket dial failed: HTTP %s", resp.Status)
		}
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}
	c.SetReadLimit(webSocketReadLimit)
	return c, nil
}

func AwaitWebSocketStart(ctx context.Context, c *websocket.Conn) error {
	_, err := AwaitWebSocketStartSession(ctx, c)
	return err
}

// AwaitWebSocketStartSession waits for relay pairing and returns the optional
// resumable session identifier negotiated by upgraded peers.
func AwaitWebSocketStartSession(ctx context.Context, c *websocket.Conn) (string, error) {
	return awaitWebSocketControl(ctx, c, webSocketStartMessage)
}

func AwaitWebSocketResume(ctx context.Context, c *websocket.Conn, sessionID string) error {
	got, err := awaitWebSocketControl(ctx, c, webSocketResumeMessage)
	if err != nil {
		return err
	}
	if got != sessionID {
		return fmt.Errorf("relay resumed unexpected session %q", got)
	}
	return nil
}

func awaitWebSocketControl(ctx context.Context, c *websocket.Conn, command string) (string, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for {
		typ, data, err := c.Read(readCtx)
		if err != nil {
			return "", fmt.Errorf("wait for relay %s: %w", command, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) > 0 && fields[0] == command {
			if len(fields) > 1 {
				return fields[1], nil
			}
			return "", nil
		}
	}
}

func CloseWebSocket(c *websocket.Conn) {
	if c == nil {
		return
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func webSocketHTTPClient(relayAddr, proxySpec string) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: HostFromRelayAddress(relayAddr),
		},
		DialContext: resilientDNSDialer.DialContext,
	}
	if endpoint, err := WebSocketEndpoint(relayAddr); err == nil {
		if endpointURL, err := url.Parse(endpoint); err == nil && endpointURL.Scheme == "ws" {
			proxyURL, err := webSocketProxyURL(endpointURL.Host, proxySpec)
			if err == nil && proxyURL != nil {
				transport.DialContext = proxyConnectDialContext(proxyURL)
				return &http.Client{Transport: transport}
			}
		}
	}
	transport.Proxy = proxyFunc(relayAddr, proxySpec)
	return &http.Client{Transport: transport}
}

func webSocketProxyURL(targetAddr, proxySpec string) (*url.URL, error) {
	spec := strings.TrimSpace(proxySpec)
	if spec == "" || strings.EqualFold(spec, "direct") {
		return nil, nil
	}
	if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
		return resolveProxyURL(targetAddr, spec)
	}
	return resolveProxyURL(targetAddr, spec)
}

func proxyConnectDialContext(proxyURL *url.URL) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := resilientDNSDialer.DialContext(ctx, network, canonicalProxyAddr(proxyURL))
		if err != nil {
			return nil, err
		}
		if proxyURL.Scheme == "https" {
			tlsConn := tls.Client(conn, &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: proxyURL.Hostname(),
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("TLS handshake with proxy %s failed: %w", proxyURLForLog(proxyURL), err)
			}
			conn = tlsConn
		}
		if err := writeProxyConnect(conn, proxyURL, address); err != nil {
			conn.Close()
			return nil, err
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read proxy CONNECT response: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT %s via %s failed: %s", address, proxyURLForLog(proxyURL), resp.Status)
		}
		return conn, nil
	}
}

func writeProxyConnect(conn net.Conn, proxyURL *url.URL, address string) error {
	var builder strings.Builder
	builder.WriteString("CONNECT ")
	builder.WriteString(address)
	builder.WriteString(" HTTP/1.1\r\nHost: ")
	builder.WriteString(address)
	builder.WriteString("\r\nUser-Agent: DeskFerry/" + buildinfo.Version + "\r\nProxy-Connection: Keep-Alive\r\n")
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		builder.WriteString("Proxy-Authorization: Basic ")
		builder.WriteString(token)
		builder.WriteString("\r\n")
	}
	builder.WriteString("\r\n")
	_, err := conn.Write([]byte(builder.String()))
	return err
}

func canonicalProxyAddr(proxyURL *url.URL) string {
	if _, _, err := net.SplitHostPort(proxyURL.Host); err == nil {
		return proxyURL.Host
	}
	switch proxyURL.Scheme {
	case "https":
		return net.JoinHostPort(proxyURL.Host, "443")
	default:
		return net.JoinHostPort(proxyURL.Host, "80")
	}
}

func proxyFunc(relayAddr, proxySpec string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		spec := strings.TrimSpace(proxySpec)
		if spec == "" || strings.EqualFold(spec, "direct") {
			return nil, nil
		}
		if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
			return http.ProxyFromEnvironment(req)
		}
		target := req.URL.Host
		if target == "" {
			target = HostFromRelayAddress(relayAddr)
		}
		return resolveProxyURL(target, spec)
	}
}

func validateWebSocketRole(role string) error {
	switch role {
	case RoleAgent, RoleClient, RoleAgentControl, RoleAgentSession, RoleProbe, RoleHomeAgent, RoleResume, RoleDiagnosticLog:
		return nil
	default:
		return fmt.Errorf("invalid websocket role %q", role)
	}
}

func relayScheme(relayAddr string) string {
	u, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}
