//go:build darwin

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"deskferry/internal/diaglog"
	"deskferry/internal/tunnel"
	"nhooyr.io/websocket"
)

const (
	defaultRelayURL   = "https://test-officialwebsite.azurewebsites.net/relay/workdesk"
	defaultListenAddr = "127.0.0.1:3389"
)

type config struct {
	RelayAddr  string
	RelayAddrs []string
	ListenAddr string
	Proxy      string
}

type relayURLFlag []string

func (f *relayURLFlag) Set(value string) error {
	*f = append(*f, splitRelayURLs(value)...)
	return nil
}

func (f *relayURLFlag) String() string {
	return joinRelayURLs([]string(*f))
}

type relaySnapshot struct {
	Service string              `json:"service"`
	Time    *time.Time          `json:"time"`
	Rooms   []relayRoomSnapshot `json:"rooms"`
}

type relayRoomSnapshot struct {
	ID                    string     `json:"id"`
	WaitingAgents         int        `json:"waiting_agents"`
	ActivePairs           int        `json:"active_pairs"`
	TotalPairs            int64      `json:"total_pairs"`
	LastAgentRemote       string     `json:"last_agent_remote"`
	LastAgentConnectedAt  *time.Time `json:"last_agent_connected_at"`
	HomeAgentConnected    bool       `json:"home_agent_connected"`
	HomeAgentRemote       string     `json:"home_agent_remote"`
	HomeAgentConnectedAt  *time.Time `json:"home_agent_connected_at"`
	LastClientRemote      string     `json:"last_client_remote"`
	LastClientConnectedAt *time.Time `json:"last_client_connected_at"`
}

type relaySummary struct {
	Room       string
	RelayAddr  string
	WorkOnline bool
	HomeOnline bool
	Waiting    int
	Active     int
	Total      int64
	LastClient string
	LastAgent  string
	LastHome   string
	CheckedAt  *time.Time
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var relayURLs relayURLFlag
	var listenAddr string
	var proxyFlag string
	var logRetentionDays int
	var openRDP bool
	var statusOnly bool
	flag.Var(&relayURLs, "relay-url", "relay room URL; repeat to add fallback URLs")
	flag.StringVar(&listenAddr, "listen", "", "local RDP listen address")
	flag.StringVar(&proxyFlag, "proxy", "", "proxy: env, direct, or http(s)://host:port")
	flag.IntVar(&logRetentionDays, "log-retention-days", diaglog.DefaultRetentionDays, "number of calendar days of diagnostic logs to retain")
	flag.BoolVar(&openRDP, "open-rdp", false, "open the local RDP profile after the tunnel starts")
	flag.BoolVar(&statusOnly, "status", false, "print relay room status and exit")
	flag.Parse()
	if path, err := diaglog.Enable("home-agent", false, logRetentionDays); err != nil {
		log.Printf("persistent diagnostic logging unavailable: %v", err)
	} else {
		log.Printf("diagnostic log file: %s retention_days=%d", path, logRetentionDays)
	}

	cfg, err := loadConfig(relayURLs.String(), listenAddr, proxyFlag)
	if err != nil {
		log.Fatal(err)
	}

	if statusOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		summary, err := queryRelaySummary(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(formatRelayDetails(summary, cfg))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, openRDP); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func loadConfig(relayURL, listenAddr, proxyFlag string) (config, error) {
	cfg := config{
		RelayAddr:  strings.TrimSpace(relayURL),
		ListenAddr: strings.TrimSpace(listenAddr),
		Proxy:      strings.TrimSpace(proxyFlag),
	}
	cfg.applyDefaults()
	normalized, err := normalizeRelayURLs(cfg.RelayAddr, cfg.RelayAddrs)
	if err != nil {
		return config{}, err
	}
	cfg.setRelayAddresses(normalized)
	return cfg, cfg.validate()
}

func (c *config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.RelayAddr == "" && len(c.RelayAddrs) == 0 {
		c.RelayAddr = defaultRelayURL
	}
	if c.Proxy == "" {
		c.Proxy = "env"
	}
}

func (c config) validate() error {
	relayAddrs := c.relayAddresses()
	if len(relayAddrs) == 0 {
		return errors.New("relay URL is required")
	}
	for _, relayAddr := range relayAddrs {
		if !tunnel.IsWebSocketRelay(relayAddr) {
			return fmt.Errorf("relay URL %q must start with https:// or http://", relayAddr)
		}
		if _, err := url.ParseRequestURI(relayAddr); err != nil {
			return fmt.Errorf("relay URL %q is invalid: %w", relayAddr, err)
		}
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("local RDP address must be host:port: %w", err)
	}
	return nil
}

func run(ctx context.Context, cfg config, openRDP bool) error {
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go homePresenceLoop(ctx, cfg)

	log.Printf("DeskFerry Home listening on %s; point your RDP client at %s; relay URLs: %s", listener.Addr(), rdpTarget(cfg.ListenAddr), cfg.relayURLText())
	if openRDP {
		if err := launchRDP(cfg); err != nil {
			log.Printf("open RDP profile: %v", err)
		}
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		remote := conn.RemoteAddr().String()
		log.Printf("RDP connection from %s", remote)
		go handleLocalConn(ctx, cfg, conn, remote)
	}
}

func handleLocalConn(ctx context.Context, cfg config, localConn net.Conn, remote string) {
	started := time.Now()
	relayConn, relayAddr, err := dialRelay(ctx, cfg)
	if err != nil {
		log.Printf("RDP session remote=%s relay dial failed after %s: %v", remote, time.Since(started).Round(time.Millisecond), err)
		_ = localConn.Close()
		return
	}
	log.Printf("RDP session remote=%s connected relay=%s local=%s relay_stream=%s dial_duration=%s", remote, relayAddr, localConn.LocalAddr(), relayConn.RemoteAddr(), time.Since(started).Round(time.Millisecond))
	result := tunnel.PipeWithResult(localConn, relayConn)
	log.Printf("RDP session remote=%s relay=%s ended duration=%s local_to_relay_bytes=%d local_to_relay_error=%v local_to_relay_half_close_error=%v relay_to_local_bytes=%d relay_to_local_error=%v relay_to_local_half_close_error=%v local_close_error=%v relay_close_error=%v", remote, relayAddr, result.Duration.Round(time.Millisecond), result.AToB.Bytes, result.AToB.CopyErr, result.AToB.CloseWriteErr, result.BToA.Bytes, result.BToA.CopyErr, result.BToA.CloseWriteErr, result.ACloseErr, result.BCloseErr)
}

func homePresenceLoop(ctx context.Context, cfg config) {
	for {
		started := time.Now()
		conn, relayAddr, err := dialWebSocketFallback(ctx, cfg, tunnel.RoleHomeAgent)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("home status connection failed: %v", err)
		} else {
			log.Printf("home status connected to %s after %s", relayAddr, time.Since(started).Round(time.Millisecond))
			_, _, err = conn.Read(ctx)
			tunnel.CloseWebSocket(conn)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("home status disconnected relay=%s duration=%s error=%v close_status=%d context_error=%v", relayAddr, time.Since(started).Round(time.Millisecond), err, websocket.CloseStatus(err), ctx.Err())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func dialRelay(ctx context.Context, cfg config) (net.Conn, string, error) {
	deadline := time.Now().Add(5 * time.Minute)
	backoff := 250 * time.Millisecond
	var errs []string
	for ctx.Err() == nil && time.Now().Before(deadline) {
		errs = errs[:0]
		for _, relayAddr := range cfg.relayAddresses() {
			attemptStarted := time.Now()
			attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			headers := http.Header{}
			headers.Set(tunnel.HeaderResumable, "1")
			ws, err := tunnel.DialWebSocketWithHeaders(attemptCtx, relayAddr, cfg.Proxy, tunnel.RoleClient, "", headers)
			sessionID := ""
			if err == nil {
				sessionID, err = tunnel.AwaitWebSocketStartSession(attemptCtx, ws)
			}
			if err == nil {
				cancel()
				log.Printf("relay client paired relay=%s via=%s duration=%s", relayAddr, tunnel.ProxySpecForLog(cfg.Proxy), time.Since(attemptStarted).Round(time.Millisecond))
				if sessionID != "" {
					return tunnel.NewResumableWebSocketConn(ctx, ws, tunnel.ResumableWebSocketOptions{
						RelayAddr: relayAddr,
						Proxy:     cfg.Proxy,
						SessionID: sessionID,
						Side:      "client",
					}), relayAddr, nil
				}
				return tunnel.WebSocketNetConn(ctx, ws), relayAddr, nil
			}
			cancel()
			tunnel.CloseWebSocket(ws)
			errs = append(errs, fmt.Sprintf("%s after %s: %v", relayAddr, time.Since(attemptStarted).Round(time.Millisecond), err))
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
	return nil, "", fmt.Errorf("relay retry window ended: %s", strings.Join(errs, "; "))
}

func launchRDP(cfg config) error {
	profile, err := writeRDPProfile(cfg)
	if err != nil {
		return err
	}
	return exec.Command("open", profile).Start()
}

func writeRDPProfile(cfg config) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	path := filepath.Join(dir, "home-agent.rdp")
	if err := os.WriteFile(path, []byte(rdpProfileContent(cfg)), 0600); err != nil {
		return "", fmt.Errorf("write RDP profile: %w", err)
	}
	return path, nil
}

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "DeskFerry"), nil
}

func rdpProfileContent(cfg config) string {
	lines := []string{
		"screen mode id:i:2",
		"use multimon:i:0",
		"session bpp:i:32",
		"full address:s:" + sanitizeRDPValue(rdpTarget(cfg.ListenAddr)),
		"prompt for credentials:i:1",
		"authentication level:i:2",
		"redirectclipboard:i:1",
		"redirectprinters:i:0",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func sanitizeRDPValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func rdpTarget(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = "127.0.0.1"
		port = "3389"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return net.JoinHostPort(host, port)
}

func queryRelaySummary(ctx context.Context, cfg config) (relaySummary, error) {
	var errs []string
	for _, relayAddr := range cfg.relayAddresses() {
		summary, err := queryRelaySummaryFor(ctx, cfg.withRelayAddress(relayAddr))
		if err == nil {
			return summary, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", relayAddr, err))
		if ctx.Err() != nil {
			break
		}
	}
	return relaySummary{}, fmt.Errorf("all relay status checks failed: %s", strings.Join(errs, "; "))
}

func queryRelaySummaryFor(ctx context.Context, cfg config) (relaySummary, error) {
	statusURL, room, err := relayStatusURL(cfg.RelayAddr)
	if err != nil {
		return relaySummary{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return relaySummary{}, err
	}
	resp, err := httpClient(cfg).Do(req)
	if err != nil {
		return relaySummary{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return relaySummary{}, fmt.Errorf("relay status returned HTTP %s", resp.Status)
	}
	var snapshot relaySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return relaySummary{}, err
	}
	summary := relaySummary{Room: room, RelayAddr: cfg.RelayAddr, CheckedAt: snapshot.Time}
	for _, r := range snapshot.Rooms {
		summary.Waiting += r.WaitingAgents
		summary.Active += r.ActivePairs
		summary.Total += r.TotalPairs
		summary.WorkOnline = summary.WorkOnline || r.WaitingAgents+r.ActivePairs > 0
		summary.HomeOnline = summary.HomeOnline || r.HomeAgentConnected
		if summary.Room == "" {
			summary.Room = r.ID
		}
		if r.LastClientRemote != "" {
			summary.LastClient = r.LastClientRemote
		}
		if r.LastAgentRemote != "" {
			summary.LastAgent = r.LastAgentRemote
		}
		if r.HomeAgentRemote != "" {
			summary.LastHome = r.HomeAgentRemote
		}
	}
	return summary, nil
}

func formatRelayDetails(summary relaySummary, cfg config) string {
	lines := []string{
		"Room: " + emptyAs(summary.Room, "default"),
		"Relay URL: " + emptyAs(summary.RelayAddr, cfg.primaryRelayAddress()),
		"Configured relays: " + cfg.relayURLText(),
		fmt.Sprintf("Work agent: %s (%d waiting sockets)", onlineText(summary.WorkOnline), summary.Waiting),
		fmt.Sprintf("Home app: %s", onlineText(summary.HomeOnline)),
		fmt.Sprintf("Active RDP streams: %d (%d total)", summary.Active, summary.Total),
		"Local RDP address: " + rdpTarget(cfg.ListenAddr),
	}
	if summary.LastAgent != "" {
		lines = append(lines, "Last work agent: "+summary.LastAgent)
	}
	if summary.LastHome != "" {
		lines = append(lines, "Home app remote: "+summary.LastHome)
	}
	if summary.LastClient != "" {
		lines = append(lines, "Last home client: "+summary.LastClient)
	}
	if summary.CheckedAt != nil && !summary.CheckedAt.IsZero() {
		lines = append(lines, "Checked: "+summary.CheckedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return strings.Join(lines, "\n")
}

func httpClient(cfg config) *http.Client {
	return &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: tunnel.HostFromRelayAddress(cfg.RelayAddr),
			},
			Proxy: httpProxyFunc(cfg.Proxy),
		},
	}
}

func httpProxyFunc(proxySpec string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		spec := strings.TrimSpace(proxySpec)
		if spec == "" || strings.EqualFold(spec, "direct") {
			return nil, nil
		}
		if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
			return http.ProxyFromEnvironment(req)
		}
		return url.Parse(spec)
	}
}

func normalizeRelayURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultRelayURL
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if parsed.Host == "" {
		return "", errors.New("relay URL must include a host")
	}
	switch parsed.Scheme {
	case "https", "http":
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	default:
		return "", errors.New("relay URL must start with https:// or http://")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(parsed.Path, "/ws") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/ws")
	}
	if parsed.Path == "" {
		parsed.Path = "/relay"
	}
	return parsed.String(), nil
}

func normalizeRelayURLs(value string, extra []string) ([]string, error) {
	values := splitRelayURLs(value)
	for _, relayAddr := range extra {
		values = append(values, splitRelayURLs(relayAddr)...)
	}
	if len(values) == 0 {
		values = []string{defaultRelayURL}
	}
	out := make([]string, 0, len(values))
	for _, relayAddr := range values {
		normalized, err := normalizeRelayURL(relayAddr)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return uniqueRelayURLs(out), nil
}

func splitRelayURLs(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '\r' || r == '\n' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func uniqueRelayURLs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func joinRelayURLs(values []string) string {
	return strings.Join(uniqueRelayURLs(values), ";")
}

func (c *config) setRelayAddresses(values []string) {
	c.RelayAddrs = append([]string(nil), values...)
	if len(c.RelayAddrs) > 0 {
		c.RelayAddr = c.RelayAddrs[0]
	}
}

func (c config) relayAddresses() []string {
	if len(c.RelayAddrs) > 0 {
		return append([]string(nil), c.RelayAddrs...)
	}
	return uniqueRelayURLs(splitRelayURLs(c.RelayAddr))
}

func (c config) relayURLText() string {
	return joinRelayURLs(c.relayAddresses())
}

func (c config) primaryRelayAddress() string {
	if relays := c.relayAddresses(); len(relays) > 0 {
		return relays[0]
	}
	return defaultRelayURL
}

func (c config) withRelayAddress(relayAddr string) config {
	next := c
	next.RelayAddr = strings.TrimSpace(relayAddr)
	next.RelayAddrs = []string{next.RelayAddr}
	return next
}

func dialWebSocketFallback(ctx context.Context, cfg config, role string) (*websocket.Conn, string, error) {
	var errs []string
	for _, relayAddr := range cfg.relayAddresses() {
		conn, err := tunnel.DialWebSocket(ctx, relayAddr, cfg.Proxy, role, "")
		if err == nil {
			return conn, relayAddr, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", relayAddr, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, "", fmt.Errorf("all relay URLs failed: %s", strings.Join(errs, "; "))
}

func relayStatusURL(relayAddr string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil {
		return "", "", fmt.Errorf("parse relay URL: %w", err)
	}
	if parsed.Host == "" {
		return "", "", errors.New("relay URL must include a host")
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	}
	room := tunnel.RoomFromRelayPath(parsed.Path)
	if room == "" {
		room = strings.TrimSpace(parsed.Query().Get("room"))
	}
	parsed.Path = "/relay/status"
	q := url.Values{}
	if room != "" {
		q.Set("room", room)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), room, nil
}

func onlineText(ok bool) string {
	if ok {
		return "online"
	}
	return "waiting"
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
