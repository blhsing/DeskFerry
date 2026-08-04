package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"deskferry/internal/diaglog"
	"deskferry/internal/tunnel"
	"deskferry/internal/winsecret"
)

const serviceName = "DeskFerryAgent"
const defaultRelayURL = "https://test-officialwebsite.azurewebsites.net/relay/"
const agentIDHeader = "X-DeskFerry-Agent-Instance"
const agentSlotHeader = "X-DeskFerry-Agent-Slot"

type config struct {
	RelayAddr        string   `json:"relay_addr"`
	RelayAddrs       []string `json:"relay_addrs,omitempty"`
	Proxy            string   `json:"proxy"`
	RDPAddr          string   `json:"rdp_addr"`
	WinRMAddr        string   `json:"winrm_addr,omitempty"`
	RoomPasswordFile string   `json:"room_password_file,omitempty"`
	RoomPassword     string   `json:"-"`
	MinBackoff       string   `json:"min_backoff"`
	MaxBackoff       string   `json:"max_backoff"`
}

type relayURLFlag []string

func (f *relayURLFlag) Set(value string) error {
	*f = append(*f, splitRelayURLs(value)...)
	return nil
}

func (f *relayURLFlag) String() string {
	return joinRelayURLs([]string(*f))
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	var relayURLs relayURLFlag
	var proxyFlag string
	var rdpFlag string
	var winrmFlag string
	var roomPasswordFile string
	var logRetentionDays int
	var consoleMode bool
	var serviceMode bool
	var installMode bool
	var uninstallMode bool
	var statusMode bool
	var selfTestMode bool
	var updateServiceTarget string
	flag.Var(&relayURLs, "relay-url", "relay service URL; repeat to add more relay URLs")
	flag.StringVar(&proxyFlag, "proxy", "", "HTTP proxy for Azure relay WebSocket, or direct/env/auto")
	flag.StringVar(&rdpFlag, "rdp", "", "local RDP target")
	flag.StringVar(&winrmFlag, "winrm", "", "local WinRM target; blank disables WinRM")
	flag.StringVar(&roomPasswordFile, "room-password-file", "", "DPAPI-protected room password file")
	flag.IntVar(&logRetentionDays, "log-retention-days", diaglog.DefaultRetentionDays, "number of calendar days of diagnostic logs to retain")
	flag.BoolVar(&consoleMode, "console", false, "run in the foreground for debugging")
	flag.BoolVar(&serviceMode, "service", false, "run under the Windows service control manager")
	flag.BoolVar(&installMode, "install", false, "install and start the Windows service")
	flag.BoolVar(&uninstallMode, "uninstall", false, "stop and remove the Windows service")
	flag.BoolVar(&statusMode, "status", false, "print Windows service status")
	flag.BoolVar(&selfTestMode, "self-test", false, "test local RDP and relay WebSocket connectivity")
	flag.StringVar(&updateServiceTarget, "update-service", "", "replace the installed service executable and restart it")
	flag.Parse()
	if path, err := diaglog.Enable("work-agent", true, logRetentionDays); err != nil {
		log.Printf("persistent diagnostic logging unavailable: %v", err)
	} else {
		log.Printf("diagnostic log file: %s retention_days=%d", path, logRetentionDays)
	}

	relayURL := relayURLs.String()
	if relayURL == "" {
		relayURL = defaultRelayURL
	}
	runningAsService := serviceMode
	if !runningAsService {
		var err error
		runningAsService, err = svc.IsWindowsService()
		if err != nil {
			log.Printf("could not determine Windows service context: %v", err)
		}
	}
	if runningAsService {
		if err := runService(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile); err != nil {
			log.Fatal(err)
		}
		return
	}
	if updateServiceTarget != "" {
		if err := updateServiceBinary(updateServiceTarget); err != nil {
			log.Fatal(err)
		}
		return
	}
	if uninstallMode {
		if err := uninstallService(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if statusMode {
		if err := printStatus(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if selfTestMode {
		cfg, err := loadConfig(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
		if err != nil {
			log.Fatal(err)
		}
		if err := selfTest(context.Background(), cfg); err != nil {
			log.Fatal(err)
		}
		fmt.Println("self-test OK")
		return
	}
	if installMode || !consoleMode {
		if err := installService(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func updateServiceBinary(target string) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(strings.TrimSpace(target))
	if err != nil {
		return err
	}
	if strings.EqualFold(source, target) {
		return fmt.Errorf("update source and installed service path are the same: %s", target)
	}
	if !isElevated() {
		verb, _ := windows.UTF16PtrFromString("runas")
		exe, _ := windows.UTF16PtrFromString(source)
		params, _ := windows.UTF16PtrFromString(joinWindowsArgs([]string{"-update-service", target}))
		if err := windows.ShellExecute(0, verb, exe, params, nil, windows.SW_NORMAL); err != nil {
			return fmt.Errorf("request elevation: %w", err)
		}
		fmt.Println("Elevation requested for DeskFerry Agent service update")
		return nil
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	staged := target + ".update"
	previous := target + ".previous"
	if err := copyExecutable(source, staged); err != nil {
		return fmt.Errorf("stage service executable: %w", err)
	}
	defer os.Remove(staged)

	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	if status.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
		if err := waitForServiceState(s, svc.Stopped, 20*time.Second); err != nil {
			return err
		}
	}

	_ = os.Remove(previous)
	if err := os.Rename(target, previous); err != nil {
		_ = s.Start()
		return fmt.Errorf("preserve previous service executable: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(previous, target)
		_ = s.Start()
		return fmt.Errorf("install service executable: %w", err)
	}
	if err := s.Start(); err != nil {
		_ = os.Remove(target)
		_ = os.Rename(previous, target)
		_ = s.Start()
		return fmt.Errorf("start updated service (previous executable restored): %w", err)
	}
	if err := waitForServiceState(s, svc.Running, 20*time.Second); err != nil {
		_, _ = s.Control(svc.Stop)
		_ = waitForServiceState(s, svc.Stopped, 20*time.Second)
		_ = os.Remove(target)
		_ = os.Rename(previous, target)
		_ = s.Start()
		return fmt.Errorf("updated service did not reach running state (previous executable restored): %w", err)
	}
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous service executable: %w", err)
	}
	fmt.Printf("Updated and started DeskFerry Agent service at %s\n", target)
	return nil
}

func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func waitForServiceState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service state %s, current state is %s", serviceState(want), serviceState(status.State))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func loadConfig(relayURL, proxyOverride, rdpOverride string, extras ...string) (config, error) {
	winrmOverride, passwordFile := "", ""
	if len(extras) > 0 {
		winrmOverride = extras[0]
	}
	if len(extras) > 1 {
		passwordFile = extras[1]
	}
	cfg := config{
		RelayAddrs:       splitRelayURLs(relayURL),
		Proxy:            strings.TrimSpace(proxyOverride),
		RDPAddr:          strings.TrimSpace(rdpOverride),
		WinRMAddr:        strings.TrimSpace(winrmOverride),
		RoomPasswordFile: strings.TrimSpace(passwordFile),
	}
	if cfg.RoomPasswordFile != "" {
		data, err := os.ReadFile(cfg.RoomPasswordFile)
		if err != nil {
			return config{}, fmt.Errorf("read room password file: %w", err)
		}
		cfg.RoomPassword, err = winsecret.Unprotect(data)
		if err != nil {
			return config{}, err
		}
	}
	cfg.applyDefaults()
	return cfg, cfg.validate()
}

func (c *config) applyDefaults() {
	c.normalizeRelayAddresses()
	if c.RDPAddr == "" {
		c.RDPAddr = "127.0.0.1:3389"
	}
	if c.RelayAddr == "" {
		c.RelayAddr = defaultRelayURL
		c.normalizeRelayAddresses()
	}
	if c.Proxy == "" {
		c.Proxy = "env"
	}
	if c.MinBackoff == "" {
		c.MinBackoff = "1s"
	}
	if c.MaxBackoff == "" {
		c.MaxBackoff = "60s"
	}
}

func (c config) validate() error {
	if c.WinRMAddr != "" && c.RoomPassword == "" {
		return fmt.Errorf("winrm requires a non-empty room password")
	}
	if c.WinRMAddr != "" {
		if _, _, err := net.SplitHostPort(c.WinRMAddr); err != nil {
			return fmt.Errorf("winrm target must be host:port: %w", err)
		}
	}
	relayAddrs := c.relayAddresses()
	if len(relayAddrs) == 0 {
		return fmt.Errorf("relay_addr is required")
	}
	room := ""
	for _, relayAddr := range relayAddrs {
		if !tunnel.IsWebSocketRelay(relayAddr) {
			return fmt.Errorf("relay URL %q must start with http://, https://, ws://, or wss://", relayAddr)
		}
		if _, err := tunnel.WebSocketEndpoint(relayAddr); err != nil {
			return err
		}
		currentRoom := strings.ToLower(tunnel.RelayRoomToken(relayAddr, ""))
		if room == "" {
			room = currentRoom
		} else if room != currentRoom {
			return fmt.Errorf("all relay URLs must use the same room name; found %q and %q", room, currentRoom)
		}
	}
	minBackoff, err := time.ParseDuration(c.MinBackoff)
	if err != nil || minBackoff <= 0 {
		return fmt.Errorf("min_backoff must be a positive duration")
	}
	maxBackoff, err := time.ParseDuration(c.MaxBackoff)
	if err != nil || maxBackoff < minBackoff {
		return fmt.Errorf("max_backoff must be a duration >= min_backoff")
	}
	return nil
}

func (c *config) normalizeRelayAddresses() {
	values := make([]string, 0, 1+len(c.RelayAddrs))
	values = append(values, splitRelayURLs(c.RelayAddr)...)
	for _, relayAddr := range c.RelayAddrs {
		values = append(values, splitRelayURLs(relayAddr)...)
	}
	c.RelayAddrs = uniqueRelayURLs(values)
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

func (c config) withRelayAddress(relayAddr string) config {
	next := c
	next.RelayAddr = strings.TrimSpace(relayAddr)
	next.RelayAddrs = []string{next.RelayAddr}
	return next
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

func installService(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile string) error {
	if strings.TrimSpace(relayURL) == "" {
		relayURL = defaultRelayURL
	}
	if !isElevated() {
		return relaunchElevated(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
	}
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	args := serviceArgs(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
	serviceConfig := serviceInstallConfig(exePath, args)
	s, err := m.CreateService(serviceName, exePath, serviceConfig, args...)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_EXISTS) || strings.Contains(strings.ToLower(err.Error()), "exists") {
			s, err = m.OpenService(serviceName)
			if err != nil {
				return err
			}
			if err := s.UpdateConfig(serviceConfig); err != nil {
				_ = s.Close()
				return fmt.Errorf("update existing service: %w", err)
			}
		} else if isAccessDenied(err) {
			return installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
		} else {
			return fmt.Errorf("create service: %w", err)
		}
	}
	defer s.Close()
	_ = eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err := s.Start(); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("DeskFerry Agent service installed and started")
	return nil
}

func serviceArgs(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile string) []string {
	args := []string{"-service"}
	if relayURL != "" {
		args = append(args, "-relay-url", relayURL)
	}
	if proxyFlag != "" {
		args = append(args, "-proxy", proxyFlag)
	}
	if rdpFlag != "" {
		args = append(args, "-rdp", rdpFlag)
	}
	if winrmFlag != "" {
		args = append(args, "-winrm", winrmFlag)
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", roomPasswordFile)
	}
	return args
}

func serviceInstallConfig(exePath string, args []string) mgr.Config {
	return mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: serviceBinaryPath(exePath, args),
		DisplayName:    "DeskFerry Agent",
		Description:    "Work-side RDP backend for DeskFerry.",
	}
}

func serviceBinaryPath(exePath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, syscall.EscapeArg(exePath))
	for _, arg := range args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	if err := s.Delete(); err != nil {
		return err
	}
	_ = eventlog.Remove(serviceName)
	fmt.Println("DeskFerry Agent service removed")
	return nil
}

func printStatus() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return err
	}
	fmt.Printf("service=%s state=%s accepts=%d\n", serviceName, serviceState(status.State), status.Accepts)
	return nil
}

func runService(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile string) error {
	return svc.Run(serviceName, &agentService{relayURL: relayURL, proxyFlag: proxyFlag, rdpFlag: rdpFlag, winrmFlag: winrmFlag, roomPasswordFile: roomPasswordFile})
}

type agentService struct {
	relayURL         string
	proxyFlag        string
	rdpFlag          string
	winrmFlag        string
	roomPasswordFile string
}

func (s *agentService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	cfg, err := loadConfig(s.relayURL, s.proxyFlag, s.rdpFlag, s.winrmFlag, s.roomPasswordFile)
	if err != nil {
		logEvent(eventlog.Error, "load config failed: %v", err)
		return false, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, cfg)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-errCh
				return false, 0
			case svc.Interrogate:
				changes <- req.CurrentStatus
			}
		case err := <-errCh:
			cancel()
			if err != nil {
				logEvent(eventlog.Error, "agent stopped: %v", err)
				return false, 1
			}
			return false, 0
		}
	}
}

func run(ctx context.Context, cfg config) error {
	return runWebSocketPools(ctx, cfg)
}

func runWebSocketPools(ctx context.Context, cfg config) error {
	const slots = 4
	targets := []serviceTarget{{Service: tunnel.ServiceRDP, Address: cfg.RDPAddr}}
	if cfg.WinRMAddr != "" {
		targets = append(targets, serviceTarget{Service: tunnel.ServiceWinRM, Address: cfg.WinRMAddr})
	}
	var wg sync.WaitGroup
	relayAddrs := cfg.relayAddresses()
	agentID, err := loadOrCreateAgentID()
	if err != nil {
		log.Printf("using temporary agent identity: %v", err)
	}
	log.Printf("starting websocket agent pools for %d relay URL(s) and %d service(s)", len(relayAddrs), len(targets))
	for _, relayAddr := range relayAddrs {
		relayCfg := cfg.withRelayAddress(relayAddr)
		for _, target := range targets {
			for i := 0; i < slots; i++ {
				wg.Add(1)
				go func(slot int, slotCfg config, service serviceTarget) {
					defer wg.Done()
					runWebSocketSlot(ctx, slotCfg, slot, agentID, service)
				}(i+1, relayCfg, target)
			}
		}
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

type serviceTarget struct {
	Service string
	Address string
}

func runWebSocketSlot(ctx context.Context, cfg config, slot int, agentID string, target serviceTarget) {
	minBackoff, _ := time.ParseDuration(cfg.MinBackoff)
	maxBackoff, _ := time.ParseDuration(cfg.MaxBackoff)
	backoff := minBackoff
	for ctx.Err() == nil {
		connected, err := runWebSocketOnce(ctx, cfg, slot, agentID, target)
		if ctx.Err() != nil {
			return
		}
		delay, nextBackoff := reconnectDelay(backoff, minBackoff, maxBackoff, connected)
		backoff = nextBackoff
		log.Printf("websocket agent service=%s slot=%d for relay %s disconnected: %v; reconnecting in %s", target.Service, slot, cfg.RelayAddr, err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func reconnectDelay(backoff, minBackoff, maxBackoff time.Duration, connected bool) (time.Duration, time.Duration) {
	if connected {
		return minBackoff, minBackoff
	}
	nextBackoff := backoff * 2
	if nextBackoff > maxBackoff {
		nextBackoff = maxBackoff
	}
	return backoff, nextBackoff
}

func runWebSocketOnce(ctx context.Context, cfg config, slot int, agentID string, target serviceTarget) (bool, error) {
	connectedAt := time.Now()
	headers := http.Header{}
	if agentID != "" {
		headers.Set(agentIDHeader, agentID)
		headers.Set(agentSlotHeader, strconv.Itoa(slot))
	}
	headers.Set(tunnel.HeaderResumable, "1")
	tunnel.AddRoomPasswordHeader(headers, cfg.RelayAddr, "", cfg.RoomPassword)
	tunnel.AddServiceHeader(headers, target.Service)
	ws, err := tunnel.DialWebSocketWithHeaders(ctx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleAgent, "", headers)
	if err != nil {
		return false, fmt.Errorf("dial after %s: %w", time.Since(connectedAt).Round(time.Millisecond), err)
	}
	defer tunnel.CloseWebSocket(ws)

	log.Printf("websocket agent service=%s slot=%d connected to relay %s via %s", target.Service, slot, cfg.RelayAddr, tunnel.ProxySpecForLog(cfg.Proxy))
	sessionID, err := tunnel.AwaitWebSocketStartSession(ctx, ws)
	if err != nil {
		return true, fmt.Errorf("wait for pairing after %s: %w", time.Since(connectedAt).Round(time.Millisecond), err)
	}
	pairedAt := time.Now()
	log.Printf("websocket agent service=%s slot=%d paired on relay %s after idle=%s; forwarding to %s", target.Service, slot, cfg.RelayAddr, pairedAt.Sub(connectedAt).Round(time.Millisecond), target.Address)
	stream := net.Conn(tunnel.WebSocketNetConn(ctx, ws))
	if sessionID != "" {
		stream = tunnel.NewResumableWebSocketConn(ctx, ws, tunnel.ResumableWebSocketOptions{
			RelayAddr: cfg.RelayAddr,
			Proxy:     cfg.Proxy,
			SessionID: sessionID,
			Side:      "agent",
			RoomProof: tunnel.RoomPasswordProof(cfg.RelayAddr, "", cfg.RoomPassword),
			Service:   target.Service,
		})
	}
	handleStream(ctx, stream, target, cfg.RelayAddr, slot)
	return true, fmt.Errorf("paired stream completed after %s", time.Since(pairedAt).Round(time.Millisecond))
}

func loadOrCreateAgentID() (string, error) {
	path, pathErr := agentIDPath()
	if pathErr == nil {
		if data, err := os.ReadFile(path); err == nil {
			if id := cleanAgentIdentity(string(data)); id != "" {
				return id, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			if id, genErr := randomAgentID(); genErr == nil {
				return id, fmt.Errorf("read agent identity: %w", err)
			}
			return "", fmt.Errorf("read agent identity: %w", err)
		}
	}

	id, err := randomAgentID()
	if err != nil {
		if pathErr != nil {
			return "", fmt.Errorf("%v; generate agent identity: %w", pathErr, err)
		}
		return "", err
	}
	if pathErr != nil {
		return id, pathErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return id, fmt.Errorf("create agent identity directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0600); err != nil {
		return id, fmt.Errorf("write agent identity: %w", err)
	}
	return id, nil
}

func agentIDPath() (string, error) {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("APPDATA"))
	}
	if base == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = dir
	}
	return filepath.Join(base, "DeskFerry", "agent-id"), nil
}

func randomAgentID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func cleanAgentIdentity(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func selfTest(parent context.Context, cfg config) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var d net.Dialer
	log.Printf("self-test local RDP target: %s", cfg.RDPAddr)
	rdpConn, err := d.DialContext(ctx, "tcp", cfg.RDPAddr)
	if err != nil {
		return fmt.Errorf("local RDP target %s is not reachable: %w", cfg.RDPAddr, err)
	}
	_ = rdpConn.Close()
	if cfg.WinRMAddr != "" {
		log.Printf("self-test local WinRM target: %s", cfg.WinRMAddr)
		winrmConn, err := d.DialContext(ctx, "tcp", cfg.WinRMAddr)
		if err != nil {
			return fmt.Errorf("local WinRM target %s is not reachable: %w", cfg.WinRMAddr, err)
		}
		_ = winrmConn.Close()
	}

	var failures []error
	for _, relayAddr := range cfg.relayAddresses() {
		relayCfg := cfg.withRelayAddress(relayAddr)
		if err := selfTestRelay(ctx, relayCfg); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", relayAddr, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func selfTestRelay(ctx context.Context, cfg config) error {
	log.Printf("self-test relay target: %s via %s", cfg.RelayAddr, tunnel.ProxySpecForLog(cfg.Proxy))
	headers := http.Header{}
	tunnel.AddRoomPasswordHeader(headers, cfg.RelayAddr, "", cfg.RoomPassword)
	ws, err := tunnel.DialWebSocketWithHeaders(ctx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleProbe, "", headers)
	if err != nil {
		return fmt.Errorf("websocket relay connection test failed: %w. %s", err, relayDialHint(err, cfg))
	}
	tunnel.CloseWebSocket(ws)
	return nil
}

func relayDialHint(err error, cfg config) string {
	errText := strings.ToLower(err.Error())
	if strings.Contains(errText, "proxy connect") {
		return fmt.Sprintf(
			"The proxy returned an HTTP error before the WebSocket handshake. Confirm that the proxy allows CONNECT to %s, that it can reach the relay host, and that port %s is permitted. If the work network allows direct outbound connections, rerun the agent without -proxy.",
			cfg.RelayAddr,
			relayPortForHint(cfg.RelayAddr),
		)
	}
	if strings.Contains(errText, "dial proxy") {
		return fmt.Sprintf("Confirm that the configured proxy %s is reachable from this PC.", tunnel.ProxySpecForLog(cfg.Proxy))
	}
	return "Confirm that the Azure relay URL is current, WebSockets are enabled, and the selected relay endpoint is reachable from this PC."
}

func relayPortForHint(addr string) string {
	if tunnel.IsWebSocketRelay(addr) {
		u, err := url.Parse(strings.TrimSpace(addr))
		if err == nil {
			if port := u.Port(); port != "" {
				return port
			}
			switch strings.ToLower(u.Scheme) {
			case "https", "wss":
				return "443"
			case "http", "ws":
				return "80"
			}
		}
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "the configured relay port"
	}
	return port
}

func handleStream(ctx context.Context, stream net.Conn, target serviceTarget, relayAddr string, slot int) {
	started := time.Now()
	var d net.Dialer
	localConn, err := d.DialContext(ctx, "tcp", target.Address)
	if err != nil {
		log.Printf("%s stream slot=%d relay=%s target=%s dial failed after %s: %v", strings.ToUpper(target.Service), slot, relayAddr, target.Address, time.Since(started).Round(time.Millisecond), err)
		_ = stream.Close()
		return
	}
	log.Printf("%s stream slot=%d relay=%s target=%s opened local=%s remote=%s dial_duration=%s", strings.ToUpper(target.Service), slot, relayAddr, target.Address, localConn.LocalAddr(), localConn.RemoteAddr(), time.Since(started).Round(time.Millisecond))
	result := tunnel.PipeWithResult(stream, localConn)
	log.Printf("%s stream slot=%d relay=%s target=%s ended duration=%s end_initiator=%s relay_to_local_bytes=%d relay_to_local_error=%v local_to_relay_bytes=%d local_to_relay_error=%v", strings.ToUpper(target.Service), slot, relayAddr, target.Address, result.Duration.Round(time.Millisecond), result.EndInitiator("relay", "local_"+target.Service), result.AToB.Bytes, result.AToB.CopyErr, result.BToA.Bytes, result.BToA.CopyErr)
}

func installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{strconv.Quote(exePath), "-console"}
	if relayURL != "" {
		args = append(args, "-relay-url", strconv.Quote(relayURL))
	}
	if proxyFlag != "" {
		args = append(args, "-proxy", strconv.Quote(proxyFlag))
	}
	if rdpFlag != "" {
		args = append(args, "-rdp", strconv.Quote(rdpFlag))
	}
	if winrmFlag != "" {
		args = append(args, "-winrm", strconv.Quote(winrmFlag))
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", strconv.Quote(roomPasswordFile))
	}
	out, err := exec.Command("schtasks", "/Create", "/TN", "DeskFerry Agent", "/SC", "ONLOGON", "/TR", strings.Join(args, " "), "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service install requires admin and Scheduled Task fallback failed: %v: %s", err, out)
	}
	fmt.Println("Installed Scheduled Task fallback for current-user logon")
	return nil
}

func relaunchElevated(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"-install"}
	if relayURL != "" {
		args = append(args, "-relay-url", relayURL)
	}
	if proxyFlag != "" {
		args = append(args, "-proxy", proxyFlag)
	}
	if rdpFlag != "" {
		args = append(args, "-rdp", rdpFlag)
	}
	if winrmFlag != "" {
		args = append(args, "-winrm", winrmFlag)
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", roomPasswordFile)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	exe, _ := windows.UTF16PtrFromString(exePath)
	params, _ := windows.UTF16PtrFromString(joinWindowsArgs(args))
	if err := windows.ShellExecute(0, verb, exe, params, nil, windows.SW_NORMAL); err != nil {
		if isAccessDenied(err) {
			return installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, roomPasswordFile)
		}
		return err
	}
	fmt.Println("Elevation requested; continue in the UAC-launched configurator window")
	return nil
}

func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func isAccessDenied(err error) bool {
	return err == windows.ERROR_ACCESS_DENIED || strings.Contains(strings.ToLower(err.Error()), "access is denied")
}

func joinWindowsArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

func serviceState(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue_pending"
	case svc.PausePending:
		return "pause_pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func logEvent(kind uint16, format string, args ...any) {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		log.Printf(format, args...)
		return
	}
	defer elog.Close()
	msg := fmt.Sprintf(format, args...)
	switch kind {
	case eventlog.Error:
		_ = elog.Error(1, msg)
	case eventlog.Warning:
		_ = elog.Warning(1, msg)
	default:
		_ = elog.Info(1, msg)
	}
}
