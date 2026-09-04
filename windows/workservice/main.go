package workservice

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

	"deskferry/internal/buildinfo"
	"deskferry/internal/diaglog"
	"deskferry/internal/remotelog"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
	"deskferry/internal/winsecret"
)

const serviceName = "DeskFerryAgent"
const defaultRelayURL = "https://test-officialwebsite.azurewebsites.net/relay/workdesk;http://217.142.228.117/relay/workdesk"
const agentIDHeader = "X-DeskFerry-Agent-Instance"
const agentSlotHeader = "X-DeskFerry-Agent-Slot"

var relayLogs = remotelog.New("work-agent")

type config struct {
	RelayAddr        string   `json:"relay_addr"`
	RelayAddrs       []string `json:"relay_addrs,omitempty"`
	Proxy            string   `json:"proxy"`
	RDPAddr          string   `json:"rdp_addr"`
	WinRMAddr        string   `json:"winrm_addr,omitempty"`
	SMBAddr          string   `json:"smb_addr,omitempty"`
	RoomPasswordFile string   `json:"room_password_file,omitempty"`
	RoomPassword     string   `json:"-"`
	MinBackoff       string   `json:"min_backoff"`
	MaxBackoff       string   `json:"max_backoff"`
	ConcurrencyLimit int      `json:"concurrency_limit,omitempty"`
	LegacyMode       bool     `json:"legacy_mode,omitempty"`
	ScreenView       bool     `json:"screen_view,omitempty"`
}

type relayURLFlag []string

func (f *relayURLFlag) Set(value string) error {
	*f = append(*f, splitRelayURLs(value)...)
	return nil
}

func (f *relayURLFlag) String() string {
	return joinRelayURLs([]string(*f))
}

func Main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	var relayURLs relayURLFlag
	var relayBases relayURLFlag
	var roomName string
	var proxyFlag string
	var rdpFlag string
	var winrmFlag string
	var smbFlag string
	var roomPasswordFile string
	var screenView bool
	var screenCaptureHelper bool
	var logRetentionDays int
	var consoleMode bool
	var serviceMode bool
	var installMode bool
	var uninstallMode bool
	var statusMode bool
	var selfTestMode bool
	var updateServiceTarget string
	flag.Var(&relayURLs, "relay-url", "relay service URL; repeat to add more relay URLs")
	flag.Var(&relayBases, "relay-base-url", "relay service base URL; repeat to add more relay services")
	flag.StringVar(&roomName, "room", "workdesk", "room name appended to each relay service base URL")
	flag.StringVar(&proxyFlag, "proxy", "", "HTTP proxy for Azure relay WebSocket, or direct/env/auto")
	flag.StringVar(&rdpFlag, "rdp", "", "local RDP target")
	flag.StringVar(&winrmFlag, "winrm", "", "local WinRM target; blank disables WinRM")
	flag.StringVar(&smbFlag, "smb", "", "local SMB target; blank disables SMB")
	flag.StringVar(&roomPasswordFile, "room-password-file", "", "DPAPI-protected room password file")
	flag.BoolVar(&screenView, "screen-view", false, "allow authenticated screen capture and streaming")
	flag.BoolVar(&screenCaptureHelper, "screen-capture-helper", false, "internal interactive desktop capture helper")
	flag.IntVar(&logRetentionDays, "log-retention-days", diaglog.DefaultRetentionDays, "number of calendar days of diagnostic logs to retain")
	flag.BoolVar(&consoleMode, "console", false, "run in the foreground for debugging")
	flag.BoolVar(&serviceMode, "service", false, "run under the Windows service control manager")
	flag.BoolVar(&installMode, "install", false, "install and start the Windows service")
	flag.BoolVar(&uninstallMode, "uninstall", false, "stop and remove the Windows service")
	flag.BoolVar(&statusMode, "status", false, "print Windows service status")
	flag.BoolVar(&selfTestMode, "self-test", false, "test local RDP and relay WebSocket connectivity")
	flag.StringVar(&updateServiceTarget, "update-service", "", "replace the installed service executable and restart it")
	flag.Parse()
	if screenCaptureHelper {
		if err := screenview.RunCaptureHelper(); err != nil {
			log.Printf("screen capture helper failed: %v", err)
			os.Exit(1)
		}
		return
	}
	if path, err := diaglog.Enable("work-agent", true, logRetentionDays, relayLogs); err != nil {
		log.Printf("persistent diagnostic logging unavailable: %v", err)
	} else {
		log.Printf("diagnostic log file: %s retention_days=%d", path, logRetentionDays)
	}
	log.Printf("DeskFerry Work Agent version=%s", buildinfo.Version)

	relayURL := relayURLs.String()
	if len(relayBases) > 0 {
		if len(relayURLs) > 0 {
			log.Fatal("use either -relay-url or -relay-base-url, not both")
		}
		var roomURLs []string
		for _, base := range relayBases {
			value, err := tunnel.RelayRoomURL(base, roomName)
			if err != nil {
				log.Fatal(err)
			}
			roomURLs = append(roomURLs, value)
		}
		relayURL = joinRelayURLs(roomURLs)
	}
	if relayURL == "" {
		relayURL = defaultRelayURL
	}
	// Maintenance actions must take precedence over service-context detection.
	// A remote administrator may intentionally launch the updater through a
	// one-shot LocalSystem service so replacement does not depend on the agent's
	// own WinRM tunnel. Treating that process as the normal agent service would
	// ignore -update-service and leave the installed binary unchanged.
	if updateServiceTarget != "" {
		if err := updateServiceBinary(updateServiceTarget); err != nil {
			log.Fatal(err)
		}
		return
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
		if err := runService(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView); err != nil {
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
		cfg, err := loadConfig(relayURL, proxyFlag, rdpFlag, screenView, winrmFlag, smbFlag, roomPasswordFile)
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
		if err := installService(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig(relayURL, proxyFlag, rdpFlag, screenView, winrmFlag, smbFlag, roomPasswordFile)
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

func loadConfig(relayURL, proxyOverride, rdpOverride string, screenView bool, extras ...string) (config, error) {
	winrmOverride, smbOverride, passwordFile := "", "", ""
	if len(extras) > 0 {
		winrmOverride = extras[0]
	}
	if len(extras) > 1 {
		smbOverride = extras[1]
	}
	if len(extras) > 2 {
		passwordFile = extras[2]
	}
	cfg := config{
		RelayAddrs:       splitRelayURLs(relayURL),
		Proxy:            strings.TrimSpace(proxyOverride),
		RDPAddr:          strings.TrimSpace(rdpOverride),
		WinRMAddr:        strings.TrimSpace(winrmOverride),
		SMBAddr:          strings.TrimSpace(smbOverride),
		RoomPasswordFile: strings.TrimSpace(passwordFile),
		ScreenView:       screenView,
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
	if c.ConcurrencyLimit == 0 {
		c.ConcurrencyLimit = envPositiveInt("DESKFERRY_MAX_SESSIONS", 32)
	}
	if envBool("DESKFERRY_FORCE_LEGACY") {
		c.LegacyMode = true
	}
}

func (c config) validate() error {
	if c.WinRMAddr != "" && c.RoomPassword == "" {
		return fmt.Errorf("winrm requires a non-empty room password")
	}
	if c.SMBAddr != "" && c.RoomPassword == "" {
		return fmt.Errorf("smb requires a non-empty room password")
	}
	if c.ScreenView && c.RoomPassword == "" {
		return fmt.Errorf("screen viewing requires a non-empty room password")
	}
	if c.SMBAddr != "" {
		if _, _, err := net.SplitHostPort(c.SMBAddr); err != nil {
			return fmt.Errorf("smb target must be host:port: %w", err)
		}
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
	if c.ConcurrencyLimit < 1 || c.ConcurrencyLimit > 256 {
		return fmt.Errorf("concurrency_limit must be between 1 and 256")
	}
	return nil
}

func envPositiveInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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

func installService(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile string, screenView bool) error {
	if strings.TrimSpace(relayURL) == "" {
		relayURL = defaultRelayURL
	}
	if !isElevated() {
		return relaunchElevated(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView)
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

	args := serviceArgs(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView)
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
			return installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView)
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

func serviceArgs(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile string, screenView bool) []string {
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
	if smbFlag != "" {
		args = append(args, "-smb", smbFlag)
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", roomPasswordFile)
	}
	if screenView {
		args = append(args, "-screen-view")
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
		Description:    "Work-side RDP, WinRM, SMB, and optional screen-view backend for DeskFerry.",
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
	fmt.Printf("service=%s version=%s state=%s accepts=%d\n", serviceName, buildinfo.Version, serviceState(status.State), status.Accepts)
	return nil
}

func runService(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile string, screenView bool) error {
	return svc.Run(serviceName, &agentService{relayURL: relayURL, proxyFlag: proxyFlag, rdpFlag: rdpFlag, winrmFlag: winrmFlag, smbFlag: smbFlag, roomPasswordFile: roomPasswordFile, screenView: screenView})
}

type agentService struct {
	relayURL         string
	proxyFlag        string
	rdpFlag          string
	winrmFlag        string
	smbFlag          string
	roomPasswordFile string
	screenView       bool
}

func (s *agentService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	cfg, err := loadConfig(s.relayURL, s.proxyFlag, s.rdpFlag, s.screenView, s.winrmFlag, s.smbFlag, s.roomPasswordFile)
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
	targets := []serviceTarget{{Service: tunnel.ServiceRDP, Address: cfg.RDPAddr}}
	if cfg.WinRMAddr != "" {
		targets = append(targets, serviceTarget{Service: tunnel.ServiceWinRM, Address: cfg.WinRMAddr})
	}
	if cfg.SMBAddr != "" {
		targets = append(targets, serviceTarget{Service: tunnel.ServiceSMB, Address: cfg.SMBAddr})
	}
	if cfg.ScreenView {
		targets = append(targets, serviceTarget{Service: tunnel.ServiceScreen, Address: "interactive desktop", Interactive: true})
	}
	var wg sync.WaitGroup
	relayAddrs := cfg.relayAddresses()
	agentID, err := loadOrCreateAgentID()
	if err != nil {
		log.Printf("using temporary agent identity: %v", err)
	}
	if agentID == "" {
		agentID, _ = randomAgentID()
	}
	relayLogs.SetInstance(agentID)
	for _, relayAddr := range relayAddrs {
		relayLogs.AddTarget(ctx, remotelog.Target{RelayAddr: relayAddr, Proxy: cfg.Proxy, RoomPassword: cfg.RoomPassword})
	}
	limiter := make(chan struct{}, cfg.ConcurrencyLimit)
	log.Printf("starting websocket agent controls for %d relay URL(s), %d service(s), concurrency=%d legacy=%t", len(relayAddrs), len(targets), cfg.ConcurrencyLimit, cfg.LegacyMode)
	for _, relayAddr := range relayAddrs {
		relayCfg := cfg.withRelayAddress(relayAddr)
		wg.Add(1)
		go func(slotCfg config) {
			defer wg.Done()
			runRelayAgent(ctx, slotCfg, agentID, targets, limiter)
		}(relayCfg)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

var errControlUnsupported = errors.New("relay does not support protocol v2 control channels")

func runRelayAgent(ctx context.Context, cfg config, agentID string, targets []serviceTarget, limiter chan struct{}) {
	if cfg.LegacyMode {
		runLegacyPools(ctx, cfg, agentID, targets)
		return
	}
	minBackoff, _ := time.ParseDuration(cfg.MinBackoff)
	maxBackoff, _ := time.ParseDuration(cfg.MaxBackoff)
	backoff := minBackoff
	for ctx.Err() == nil {
		connected, err := runAgentControlOnce(ctx, cfg, agentID, targets, limiter)
		if errors.Is(err, errControlUnsupported) {
			log.Printf("relay %s does not support protocol v2; using legacy slots for rollback compatibility", cfg.RelayAddr)
			runLegacyPools(ctx, cfg, agentID, targets)
			return
		}
		if ctx.Err() != nil {
			return
		}
		delay, next := reconnectDelay(backoff, minBackoff, maxBackoff, connected)
		backoff = next
		log.Printf("agent control relay=%s disconnected: %v; reconnecting in %s", cfg.RelayAddr, err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func runLegacyPools(ctx context.Context, cfg config, agentID string, targets []serviceTarget) {
	const slots = 4
	var wg sync.WaitGroup
	for _, target := range targets {
		if target.Interactive {
			continue
		}
		for i := 0; i < slots; i++ {
			wg.Add(1)
			go func(slot int, service serviceTarget) {
				defer wg.Done()
				runWebSocketSlot(ctx, cfg, slot, agentID, service)
			}(i+1, target)
		}
	}
	<-ctx.Done()
	wg.Wait()
}

type controlWriter struct {
	mu sync.Mutex
	ws tunnel.MessageConn
}

func (w *controlWriter) send(ctx context.Context, message tunnel.ControlMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return tunnel.WriteControlMessage(writeCtx, w.ws, message)
}

func runAgentControlOnce(ctx context.Context, cfg config, agentID string, targets []serviceTarget, limiter chan struct{}) (bool, error) {
	headers := http.Header{}
	tunnel.AddProtocolV2Header(headers)
	headers.Set(tunnel.HeaderAgentInstance, agentID)
	headers.Set(tunnel.HeaderConcurrency, strconv.Itoa(cfg.ConcurrencyLimit))
	services := make([]string, 0, len(targets))
	targetByService := make(map[string]serviceTarget, len(targets))
	for _, target := range targets {
		services = append(services, target.Service)
		targetByService[target.Service] = target
	}
	headers.Set(tunnel.HeaderAgentServices, strings.Join(services, ","))
	tunnel.AddRoomPasswordHeader(headers, cfg.RelayAddr, "", cfg.RoomPassword)
	ws, err := tunnel.DialMessageConnWithHeaders(ctx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleAgentControl, "", headers)
	if err != nil {
		return false, err
	}
	defer tunnel.CloseMessageConn(ws)
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = tunnel.AwaitControlReady(readyCtx, ws)
	cancel()
	if err != nil {
		if tunnel.IsTerminalSessionError(err) && strings.Contains(strings.ToLower(err.Error()), "missing relay role") {
			return false, errControlUnsupported
		}
		return false, err
	}
	writer := &controlWriter{ws: ws}
	log.Printf("agent control connected relay=%s room=%s services=%s concurrency=%d via=%s", cfg.RelayAddr, tunnel.RelayRoomToken(cfg.RelayAddr, ""), strings.Join(services, ","), cfg.ConcurrencyLimit, tunnel.ProxySpecForLog(cfg.Proxy))
	for {
		message, err := tunnel.ReadControlMessage(ctx, ws)
		if err != nil {
			return true, err
		}
		if message.Type != tunnel.MessageSessionOffer {
			continue
		}
		if err := tunnel.ValidateSessionOffer(message, tunnel.RelayRoomToken(cfg.RelayAddr, ""), agentID, time.Now().UTC()); err != nil {
			result := tunnel.MessageUnsupportedVersion
			if message.ProtocolVersion == tunnel.ProtocolVersion2 {
				result = tunnel.MessageServiceDisabled
			}
			_ = writer.send(ctx, tunnel.ControlMessage{Type: result, SessionID: message.SessionID, Reason: err.Error()})
			continue
		}
		target, enabled := targetByService[message.Service]
		if !enabled {
			_ = writer.send(ctx, tunnel.ControlMessage{Type: tunnel.MessageServiceDisabled, SessionID: message.SessionID, Reason: "requested service is disabled"})
			continue
		}
		select {
		case limiter <- struct{}{}:
			go serveOfferedSession(ctx, cfg, agentID, target, message, writer, limiter)
		default:
			_ = writer.send(ctx, tunnel.ControlMessage{Type: tunnel.MessageBusy, SessionID: message.SessionID, Reason: "work agent concurrency limit reached"})
			log.Printf("session rejected busy relay=%s session=%s service=%s concurrency=%d", cfg.RelayAddr, message.SessionID, message.Service, cfg.ConcurrencyLimit)
		}
	}
}

func serveOfferedSession(parent context.Context, cfg config, agentID string, target serviceTarget, offer tunnel.ControlMessage, writer *controlWriter, limiter chan struct{}) {
	defer func() { <-limiter }()
	sessionCtx, cancel := context.WithDeadline(parent, offer.ExpiresAt)
	defer cancel()
	started := time.Now()
	var localConn net.Conn
	var err error
	if target.Interactive {
		localConn, err = launchScreenCaptureHelper()
	} else {
		var dialer net.Dialer
		localConn, err = dialer.DialContext(sessionCtx, "tcp", target.Address)
	}
	if err != nil {
		_ = writer.send(parent, tunnel.ControlMessage{Type: tunnel.MessageServiceDisabled, SessionID: offer.SessionID, Reason: "local service unavailable"})
		log.Printf("session offer local dial failed relay=%s session=%s service=%s target=%s duration=%s error=%v", cfg.RelayAddr, offer.SessionID, target.Service, target.Address, time.Since(started).Round(time.Millisecond), err)
		return
	}
	if err := writer.send(parent, tunnel.ControlMessage{Type: tunnel.MessageAccept, SessionID: offer.SessionID, Heartbeat: offer.Heartbeat}); err != nil {
		_ = localConn.Close()
		return
	}
	headers := http.Header{}
	tunnel.AddProtocolV2Header(headers)
	headers.Set(tunnel.HeaderAgentInstance, agentID)
	headers.Set(tunnel.HeaderSessionID, offer.SessionID)
	if offer.Resumable {
		headers.Set(tunnel.HeaderResumable, "1")
	}
	tunnel.AddRoomPasswordHeader(headers, cfg.RelayAddr, "", cfg.RoomPassword)
	tunnel.AddServiceHeader(headers, target.Service)
	ws, err := tunnel.DialMessageConnWithHeaders(sessionCtx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleAgentSession, "", headers)
	if err != nil {
		_ = localConn.Close()
		log.Printf("agent session dial failed relay=%s session=%s service=%s duration=%s error=%v", cfg.RelayAddr, offer.SessionID, target.Service, time.Since(started).Round(time.Millisecond), err)
		return
	}
	ready, err := tunnel.AwaitSessionReadyInfoResult(sessionCtx, ws)
	if err != nil {
		_ = localConn.Close()
		tunnel.CloseMessageConn(ws)
		log.Printf("agent session rejected relay=%s session=%s service=%s error=%v", cfg.RelayAddr, offer.SessionID, target.Service, err)
		return
	}
	cancel()
	stream := net.Conn(tunnel.MessageNetConn(parent, ws))
	if offer.Resumable {
		stream = tunnel.NewResumableWebSocketConn(parent, ws, tunnel.ResumableWebSocketOptions{RelayAddr: cfg.RelayAddr, Proxy: cfg.Proxy, SessionID: offer.SessionID, Side: "agent", RoomProof: tunnel.RoomPasswordProof(cfg.RelayAddr, "", cfg.RoomPassword), Service: target.Service, Heartbeat: ready.Heartbeat})
	}
	log.Printf("session ready relay=%s session=%s service=%s target=%s heartbeat=%t setup_duration=%s", cfg.RelayAddr, offer.SessionID, target.Service, target.Address, ready.Heartbeat, time.Since(started).Round(time.Millisecond))
	pipeStream := stream
	if target.Interactive {
		pipeStream = drainScreenResponse(stream)
	}
	result := tunnel.PipeWithResult(pipeStream, localConn)
	_ = writer.send(parent, tunnel.ControlMessage{Type: tunnel.MessageSessionClosed, SessionID: offer.SessionID, Service: target.Service, Reason: result.EndInitiator("relay", "local_"+target.Service)})
	log.Printf("session closed relay=%s session=%s service=%s duration=%s end_initiator=%s relay_to_local_bytes=%d local_to_relay_bytes=%d", cfg.RelayAddr, offer.SessionID, target.Service, result.Duration.Round(time.Millisecond), result.EndInitiator("relay", "local_"+target.Service), result.AToB.Bytes, result.BToA.Bytes)
}

type serviceTarget struct {
	Service     string
	Address     string
	Interactive bool
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
	ws, err := tunnel.DialMessageConnWithHeaders(ctx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleAgent, "", headers)
	if err != nil {
		return false, fmt.Errorf("dial after %s: %w", time.Since(connectedAt).Round(time.Millisecond), err)
	}
	defer tunnel.CloseMessageConn(ws)

	log.Printf("websocket agent service=%s slot=%d connected to relay %s via %s", target.Service, slot, cfg.RelayAddr, tunnel.ProxySpecForLog(cfg.Proxy))
	sessionID, err := tunnel.AwaitWebSocketStartSession(ctx, ws)
	if err != nil {
		return true, fmt.Errorf("wait for pairing after %s: %w", time.Since(connectedAt).Round(time.Millisecond), err)
	}
	pairedAt := time.Now()
	log.Printf("websocket agent service=%s slot=%d paired on relay %s after idle=%s; forwarding to %s", target.Service, slot, cfg.RelayAddr, pairedAt.Sub(connectedAt).Round(time.Millisecond), target.Address)
	stream := net.Conn(tunnel.MessageNetConn(ctx, ws))
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
	if cfg.SMBAddr != "" {
		log.Printf("self-test local SMB target: %s", cfg.SMBAddr)
		smbConn, err := d.DialContext(ctx, "tcp", cfg.SMBAddr)
		if err != nil {
			return fmt.Errorf("local SMB target %s is not reachable: %w", cfg.SMBAddr, err)
		}
		_ = smbConn.Close()
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
	ws, err := tunnel.DialMessageConnWithHeaders(ctx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleProbe, "", headers)
	if err != nil {
		return fmt.Errorf("websocket relay connection test failed: %w. %s", err, relayDialHint(err, cfg))
	}
	tunnel.CloseMessageConn(ws)
	return nil
}

func relayDialHint(err error, cfg config) string {
	errText := strings.ToLower(err.Error())
	if strings.Contains(errText, "proxy connect") {
		return fmt.Sprintf(
			"The proxy rejected WebSocket CONNECT and the HTTP-stream fallback also failed. Confirm that the proxy can forward ordinary POST/GET requests to a configured plain-HTTP relay, or permits CONNECT to %s on port %s. If the work network allows direct outbound connections, rerun the agent without -proxy.",
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

func installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile string, screenView bool) error {
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
	if smbFlag != "" {
		args = append(args, "-smb", strconv.Quote(smbFlag))
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", strconv.Quote(roomPasswordFile))
	}
	if screenView {
		args = append(args, "-screen-view")
	}
	out, err := exec.Command("schtasks", "/Create", "/TN", "DeskFerry Agent", "/SC", "ONLOGON", "/TR", strings.Join(args, " "), "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service install requires admin and Scheduled Task fallback failed: %v: %s", err, out)
	}
	fmt.Println("Installed Scheduled Task fallback for current-user logon")
	return nil
}

func relaunchElevated(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile string, screenView bool) error {
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
	if smbFlag != "" {
		args = append(args, "-smb", smbFlag)
	}
	if roomPasswordFile != "" {
		args = append(args, "-room-password-file", roomPasswordFile)
	}
	if screenView {
		args = append(args, "-screen-view")
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	exe, _ := windows.UTF16PtrFromString(exePath)
	params, _ := windows.UTF16PtrFromString(joinWindowsArgs(args))
	if err := windows.ShellExecute(0, verb, exe, params, nil, windows.SW_NORMAL); err != nil {
		if isAccessDenied(err) {
			return installScheduledTask(relayURL, proxyFlag, rdpFlag, winrmFlag, smbFlag, roomPasswordFile, screenView)
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
