//go:build darwin

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"deskferry/internal/buildinfo"
	"deskferry/internal/diaglog"
	"deskferry/internal/remotelog"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
	"nhooyr.io/websocket"
)

const (
	defaultRelayURL       = "https://test-officialwebsite.azurewebsites.net/relay/workdesk"
	defaultAzureRelayBase = "https://test-officialwebsite.azurewebsites.net/relay"
	defaultOCIRelayBase   = "http://217.142.228.117/relay"
	defaultListenAddr     = "127.0.0.1:3389"
)

var relayLogs = remotelog.New("home-agent-macos")

type config struct {
	RelayAddr    string
	RelayAddrs   []string
	ListenAddr   string
	Proxy        string
	RoomPassword string
	RoomProof    string
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
	var relayBases relayURLFlag
	var roomName string
	var listenAddr string
	var proxyFlag string
	var roomPassword string
	var logRetentionDays int
	var openRDP bool
	var statusOnly bool
	var uiMode bool
	var destinationFlag string
	var winRMCommand string
	var winRMCommandFile string
	var winRMTimeout time.Duration
	var screenshot string
	var screenshotStream string
	var screenInterval time.Duration
	var screenCount int
	flag.Var(&relayURLs, "relay-url", "relay room URL; repeat to add fallback URLs")
	flag.Var(&relayBases, "relay-base-url", "relay service base URL; repeat to add fallback relay services")
	flag.StringVar(&roomName, "room", "workdesk", "room name appended to each relay service base URL")
	flag.StringVar(&listenAddr, "listen", "", "local RDP listen address")
	flag.StringVar(&proxyFlag, "proxy", "", "proxy: env, direct, or http(s)://host:port")
	flag.StringVar(&roomPassword, "room-password", "", "optional room password")
	flag.IntVar(&logRetentionDays, "log-retention-days", diaglog.DefaultRetentionDays, "number of calendar days of diagnostic logs to retain")
	flag.BoolVar(&openRDP, "open-rdp", false, "open the local RDP profile after the tunnel starts")
	flag.BoolVar(&statusOnly, "status", false, "print relay room status and exit")
	flag.BoolVar(&uiMode, "ui", true, "open the macOS Home control panel")
	flag.StringVar(&destinationFlag, "destination", "", "saved destination profile to use")
	flag.StringVar(&winRMCommand, "winrm-command", "", "run a PowerShell command through the selected destination's WinRM service")
	flag.StringVar(&winRMCommandFile, "winrm-command-file", "", "read a WinRM PowerShell command from a file, or - for standard input")
	flag.DurationVar(&winRMTimeout, "winrm-timeout", 2*time.Minute, "timeout for CLI WinRM command execution")
	flag.StringVar(&screenshot, "screenshot", "", "capture one Work screen PNG to this file, or - for standard output")
	flag.StringVar(&screenshotStream, "screenshot-stream", "", "stream complete Work screen PNGs into this directory")
	flag.DurationVar(&screenInterval, "screen-interval", time.Second, "screenshot stream interval")
	flag.IntVar(&screenCount, "screen-count", 0, "number of streamed screenshots to save; 0 runs until interrupted")
	flag.Parse()
	if path, err := diaglog.Enable("home-agent", false, logRetentionDays, relayLogs); err != nil {
		log.Printf("persistent diagnostic logging unavailable: %v", err)
	} else {
		log.Printf("diagnostic log file: %s retention_days=%d", path, logRetentionDays)
	}
	log.Printf("DeskFerry Home Agent version=%s platform=macos", buildinfo.Version)

	relayURLText := relayURLs.String()
	if len(relayBases) > 0 {
		if len(relayURLs) > 0 {
			log.Fatal("use either -relay-url or -relay-base-url, not both")
		}
		var values []string
		for _, base := range relayBases {
			value, composeErr := tunnel.RelayRoomURL(base, roomName)
			if composeErr != nil {
				log.Fatal(composeErr)
			}
			values = append(values, value)
		}
		relayURLText = joinRelayURLs(values)
	}
	cfg, err := loadConfig(relayURLText, listenAddr, proxyFlag, roomPassword)
	if err != nil {
		log.Fatal(err)
	}
	winRMCommandMode := winRMCommand != "" || winRMCommandFile != ""
	screenOptions := screenview.CLIOptions{Screenshot: screenshot, StreamDirectory: screenshotStream, Interval: screenInterval, Count: screenCount}
	screenCommandMode := screenOptions.Active()
	flag.Visit(func(option *flag.Flag) {
		if strings.HasPrefix(option.Name, "screen-") {
			screenCommandMode = true
		}
	})
	var selectedProfile macProfile
	useSavedScreenProfile := screenCommandMode && (destinationFlag != "" || (len(relayURLs) == 0 && len(relayBases) == 0 && strings.TrimSpace(roomPassword) == ""))
	if winRMCommandMode || useSavedScreenProfile {
		settings, err := loadMacSettings(cfg)
		if err != nil {
			log.Fatal(err)
		}
		if err := selectMacDestination(&settings, destinationFlag); err != nil {
			log.Fatal(err)
		}
		cfg, err = settingsConfig(settings)
		if err != nil {
			log.Fatal(err)
		}
		selectedProfile = settings.Profiles[settings.Selected]
	}

	if statusOnly {
		if screenCommandMode {
			log.Fatal("use screenshot options separately from -status")
		}
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
	relayLogs.SetInstance(hostInstance())
	for _, relayAddr := range cfg.relayAddresses() {
		relayLogs.AddTarget(ctx, remotelog.Target{RelayAddr: relayAddr, Proxy: cfg.Proxy, RoomPassword: cfg.RoomPassword, RoomProof: cfg.RoomProof})
	}
	if winRMCommandMode {
		command, err := readCLIWinRMCommand(winRMCommand, winRMCommandFile, os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		commandCtx, cancel := context.WithTimeout(ctx, winRMTimeout)
		defer cancel()
		if err := executeMacWinRM(commandCtx, cfg, selectedProfile, command, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if screenCommandMode {
		if winRMCommandMode {
			log.Fatal("use screenshot options separately from WinRM command options")
		}
		if err := screenOptions.Validate(); err != nil {
			log.Fatal(err)
		}
		conn, relayAddr, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(os.Stderr, "Connected to the Work screen service through %s.\n", relayAddr)
		if err := screenview.RunCLI(ctx, conn, screenOptions); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}
	if uiMode {
		if err := runMacUI(ctx, cfg, openRDP); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(ctx, cfg, openRDP); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func selectMacDestination(settings *macSettings, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		if settings.Selected < 0 || settings.Selected >= len(settings.Profiles) {
			return errors.New("selected destination profile is invalid")
		}
		return nil
	}
	for index, profile := range settings.Profiles {
		if strings.EqualFold(profile.Name, name) {
			settings.Selected = index
			return nil
		}
	}
	return fmt.Errorf("destination profile %q was not found", name)
}

func readCLIWinRMCommand(command, commandFile string, stdin io.Reader) (string, error) {
	if command != "" && commandFile != "" {
		return "", errors.New("use either -winrm-command or -winrm-command-file, not both")
	}
	if commandFile != "" {
		var data []byte
		var err error
		if commandFile == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(commandFile)
		}
		if err != nil {
			return "", fmt.Errorf("read WinRM command: %w", err)
		}
		command = string(data)
	}
	if strings.TrimSpace(command) == "" {
		return "", errors.New("WinRM command is empty")
	}
	return command, nil
}

func hostInstance() string {
	name, _ := os.Hostname()
	if strings.TrimSpace(name) == "" {
		return "macos-home"
	}
	return name
}

func loadConfig(relayURL, listenAddr, proxyFlag string, roomPassword ...string) (config, error) {
	cfg := config{
		RelayAddr:  strings.TrimSpace(relayURL),
		ListenAddr: strings.TrimSpace(listenAddr),
		Proxy:      strings.TrimSpace(proxyFlag),
	}
	if len(roomPassword) > 0 {
		cfg.RoomPassword = roomPassword[0]
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
		c.RelayAddrs = []string{defaultRelayURL, defaultOCIRelayBase + "/workdesk"}
		c.RelayAddr = c.RelayAddrs[0]
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
	room := ""
	for _, relayAddr := range relayAddrs {
		if !tunnel.IsWebSocketRelay(relayAddr) {
			return fmt.Errorf("relay URL %q must start with https:// or http://", relayAddr)
		}
		if _, err := url.ParseRequestURI(relayAddr); err != nil {
			return fmt.Errorf("relay URL %q is invalid: %w", relayAddr, err)
		}
		currentRoom := strings.ToLower(tunnel.RelayRoomToken(relayAddr, ""))
		if room == "" {
			room = currentRoom
		} else if room != currentRoom {
			return fmt.Errorf("all relay URLs must use the same room name")
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

	var localSessions tunnel.ConnGroup
	defer localSessions.Close()
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
		connCtx, release := localSessions.Begin(ctx, conn)
		go func() {
			defer release()
			handleLocalConn(connCtx, cfg, conn, remote)
		}()
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
	log.Printf("RDP session remote=%s relay=%s ended duration=%s end_initiator=%s local_to_relay_bytes=%d local_to_relay_error=%v local_to_relay_half_close_error=%v relay_to_local_bytes=%d relay_to_local_error=%v relay_to_local_half_close_error=%v local_close_error=%v relay_close_error=%v", remote, relayAddr, result.Duration.Round(time.Millisecond), result.EndInitiator("local_rdp", "relay"), result.AToB.Bytes, result.AToB.CopyErr, result.AToB.CloseWriteErr, result.BToA.Bytes, result.BToA.CopyErr, result.BToA.CloseWriteErr, result.ACloseErr, result.BCloseErr)
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
			tunnel.CloseMessageConn(conn)
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
	return dialRelayService(ctx, cfg, tunnel.ServiceRDP)
}

func dialRelayService(ctx context.Context, cfg config, service string) (net.Conn, string, error) {
	deadline := time.Now().Add(5 * time.Minute)
	backoff := 250 * time.Millisecond
	var errs []string
	for ctx.Err() == nil && time.Now().Before(deadline) {
		errs = errs[:0]
		for _, relayAddr := range cfg.relayAddresses() {
			attemptStarted := time.Now()
			attemptCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			headers := http.Header{}
			tunnel.AddProtocolV2Header(headers)
			if service != tunnel.ServiceScreen {
				headers.Set(tunnel.HeaderResumable, "1")
				tunnel.AddHeartbeatHeader(headers)
			}
			addRoomCredentialHeader(headers, cfg, relayAddr)
			tunnel.AddServiceHeader(headers, service)
			ws, err := tunnel.DialMessageConnWithHeaders(attemptCtx, relayAddr, cfg.Proxy, tunnel.RoleClient, "", headers)
			ready := tunnel.SessionReadyInfo{}
			if err == nil {
				if service == tunnel.ServiceScreen {
					ready, err = tunnel.AwaitSessionReadyCompatibleServiceInfo(attemptCtx, ws, service)
				} else {
					ready, err = tunnel.AwaitSessionReadyCompatibleInfo(attemptCtx, ws)
				}
			}
			if err == nil {
				cancel()
				log.Printf("relay attempt selected relay=%s service=%s protocol_v2=%t heartbeat=%t via=%s elapsed=%s", relayAddr, service, ready.ProtocolV2, ready.Heartbeat, tunnel.ProxySpecForLog(cfg.Proxy), time.Since(attemptStarted).Round(time.Millisecond))
				if ready.SessionID != "" && service != tunnel.ServiceScreen {
					return tunnel.NewResumableWebSocketConn(ctx, ws, tunnel.ResumableWebSocketOptions{
						RelayAddr: relayAddr,
						Proxy:     cfg.Proxy,
						SessionID: ready.SessionID,
						Side:      "client",
						RoomProof: roomProof(cfg, relayAddr),
						Service:   service,
						Heartbeat: ready.Heartbeat,
					}), relayAddr, nil
				}
				return tunnel.MessageNetConn(ctx, ws), relayAddr, nil
			}
			cancel()
			tunnel.CloseMessageConn(ws)
			elapsed := time.Since(attemptStarted).Round(time.Millisecond)
			log.Printf("relay attempt failed relay=%s service=%s elapsed=%s result=%s error=%v", relayAddr, service, elapsed, relayAttemptResult(err), err)
			errs = append(errs, fmt.Sprintf("%s after %s: %v", relayAddr, elapsed, err))
			var rejected *tunnel.SessionResultError
			if errors.As(err, &rejected) && (rejected.Result == tunnel.MessageAuthFailed || rejected.Result == tunnel.MessageServiceDisabled || rejected.Result == tunnel.MessageInvalidRequest) {
				return nil, "", fmt.Errorf("relay rejected non-retryable %s session: %w", service, err)
			}
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

func relayAttemptResult(err error) string {
	if err == nil {
		return "selected"
	}
	var rejected *tunnel.SessionResultError
	if errors.As(err, &rejected) {
		return rejected.Result
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return "timeout"
	}
	return tunnel.TransportFailureResult(err)
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
	client := httpClient(cfg)
	defer client.CloseIdleConnections()
	return queryRelaySummaryWithClient(ctx, cfg, client)
}

func queryRelaySummaryWithClient(ctx context.Context, cfg config, client *http.Client) (relaySummary, error) {
	var errs []string
	for _, relayAddr := range cfg.relayAddresses() {
		summary, err := queryRelaySummaryFor(ctx, cfg.withRelayAddress(relayAddr), client)
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

func queryRelaySummaryFor(ctx context.Context, cfg config, client *http.Client) (relaySummary, error) {
	statusURL, room, err := relayStatusURL(cfg.RelayAddr)
	if err != nil {
		return relaySummary{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return relaySummary{}, err
	}
	resp, err := client.Do(req)
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	tunnel.ConfigureHTTPTransport(transport, cfg.Proxy)
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 2
	transport.MaxConnsPerHost = 4
	transport.IdleConnTimeout = 30 * time.Second
	return &http.Client{
		Timeout:   8 * time.Second,
		Transport: transport,
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

func dialWebSocketFallback(ctx context.Context, cfg config, role string) (tunnel.MessageConn, string, error) {
	var errs []string
	for _, relayAddr := range cfg.relayAddresses() {
		headers := http.Header{}
		addRoomCredentialHeader(headers, cfg, relayAddr)
		conn, err := tunnel.DialMessageConnWithHeaders(ctx, relayAddr, cfg.Proxy, role, "", headers)
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

func addRoomCredentialHeader(headers http.Header, cfg config, relayAddr string) {
	if cfg.RoomProof != "" {
		headers.Set(tunnel.HeaderRoomProof, cfg.RoomProof)
		return
	}
	tunnel.AddRoomPasswordHeader(headers, relayAddr, "", cfg.RoomPassword)
}

func roomProof(cfg config, relayAddr string) string {
	if cfg.RoomProof != "" {
		return cfg.RoomProof
	}
	return tunnel.RoomPasswordProof(relayAddr, "", cfg.RoomPassword)
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
