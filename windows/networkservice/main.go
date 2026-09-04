//go:build windows

package homenetworkservice

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"deskferry/internal/diaglog"
	"deskferry/internal/homenetwork"
	"deskferry/internal/tunnel"
)

const networkServiceName = "DeskFerryHomeNetwork"

func Main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	var configPath string
	var consoleMode bool
	var serviceMode bool
	var checkMode bool
	flag.StringVar(&configPath, "config", defaultConfigPath(), "home network configuration file")
	flag.BoolVar(&consoleMode, "console", false, "run in the foreground for diagnostics")
	flag.BoolVar(&serviceMode, "service", false, "run under the Windows service manager")
	flag.BoolVar(&checkMode, "check", false, "validate the configuration and installed components")
	flag.Parse()
	if path, err := diaglog.Enable("home-network", true, diaglog.DefaultRetentionDays); err == nil {
		log.Printf("diagnostic log file: %s", path)
	}

	runningAsService := serviceMode
	if !consoleMode && !runningAsService {
		if detected, err := svc.IsWindowsService(); err == nil {
			runningAsService = detected
		}
	}
	if runningAsService {
		if err := svc.Run(networkServiceName, &networkService{configPath: configPath}); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}
	if checkMode {
		fmt.Printf("configuration OK; UNC path is \\\\%s\\<share>\n", cfg.Alias)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

type networkService struct{ configPath string }

func (s *networkService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		logEvent(eventlog.Error, "load config failed: %v", err)
		return false, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()
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
				logEvent(eventlog.Error, "home network stopped: %v", err)
				return false, 1
			}
			return false, 0
		}
	}
}

func defaultConfigPath() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "DeskFerry", "home-network.json")
}

func loadConfig(path string) (homenetwork.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return homenetwork.Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg homenetwork.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg = cfg.WithDefaults(filepath.Dir(path))
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("validate %s: %w", path, err)
	}
	if _, err := os.Stat(cfg.Tun2SocksPath); err != nil {
		return cfg, fmt.Errorf("tun2socks is not available at %s: %w", cfg.Tun2SocksPath, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Tun2SocksPath), "wintun.dll")); err != nil {
		return cfg, fmt.Errorf("Wintun is not available beside tun2socks: %w", err)
	}
	return cfg, nil
}

func run(ctx context.Context, cfg homenetwork.Config) error {
	listener, err := net.Listen("tcp", cfg.SOCKSAddress)
	if err != nil {
		return fmt.Errorf("listen on internal SOCKS address %s: %w", cfg.SOCKSAddress, err)
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	cmd := exec.CommandContext(ctx, cfg.Tun2SocksPath,
		"-device", tunDeviceSpec(cfg.InterfaceName),
		"-proxy", "socks5://"+cfg.SOCKSAddress,
		"-loglevel", "info",
	)
	cmd.Dir = filepath.Dir(cfg.Tun2SocksPath)
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tun2socks: %w", err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- cmd.Wait() }()
	if err := configureAdapter(ctx, cfg); err != nil {
		_ = cmd.Process.Kill()
		<-processDone
		return err
	}
	log.Printf("DeskFerry virtual network ready: \\\\%s\\<share> -> %s:445", cfg.Alias, cfg.RemoteAddress)

	acceptErr := make(chan error, 1)
	go func() { acceptErr <- serveSOCKS(ctx, listener, cfg) }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-processDone
		return nil
	case err := <-processDone:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return errors.New("tun2socks exited unexpectedly")
		}
		return fmt.Errorf("tun2socks exited: %w", err)
	case err := <-acceptErr:
		_ = cmd.Process.Kill()
		<-processDone
		if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

func tunDeviceSpec(interfaceName string) string {
	// tun2socks uses the portable "tun" driver name; on Windows that driver is
	// backed by the signed Wintun DLL next to the executable.
	return "tun://" + interfaceName
}

func configureAdapter(ctx context.Context, cfg homenetwork.Config) error {
	ip, subnet, _ := net.ParseCIDR(cfg.InterfaceAddress)
	mask := net.IP(subnet.Mask).String()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := hiddenCommand(ctx, "netsh", "interface", "show", "interface", "name="+cfg.InterfaceName).Run(); err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("virtual adapter %q did not appear", cfg.InterfaceName)
	}
	if out, err := hiddenCommand(ctx, "netsh", "interface", "ipv4", "set", "address", "name="+cfg.InterfaceName, "source=static", "address="+ip.String(), "mask="+mask, "gateway=none").CombinedOutput(); err != nil {
		return fmt.Errorf("configure virtual adapter address: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func serveSOCKS(ctx context.Context, listener net.Listener, cfg homenetwork.Config) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			if err := handleSOCKS(ctx, conn, cfg); err != nil && ctx.Err() == nil {
				log.Printf("SMB connection rejected or ended: %v", err)
			}
		}()
	}
}

func handleSOCKS(ctx context.Context, conn net.Conn, cfg homenetwork.Config) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != 5 || header[1] == 0 {
		return errors.New("unsupported SOCKS greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	noAuth := false
	for _, method := range methods {
		noAuth = noAuth || method == 0
	}
	if !noAuth {
		_, _ = conn.Write([]byte{5, 0xff})
		return errors.New("SOCKS client did not offer no-authentication")
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(reader, request); err != nil {
		return err
	}
	if request[0] != 5 || request[1] != 1 {
		writeSOCKSReply(conn, 7)
		return errors.New("only SOCKS CONNECT is supported")
	}
	destination, err := readSOCKSAddress(reader, request[3])
	if err != nil {
		writeSOCKSReply(conn, 8)
		return err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portBytes)
	if port != 445 || (!strings.EqualFold(destination, cfg.Alias) && destination != cfg.RemoteAddress) {
		writeSOCKSReply(conn, 2)
		return fmt.Errorf("destination %s:%d is outside the DeskFerry SMB endpoint", destination, port)
	}

	stream, relayAddr, err := dialSMBRelay(ctx, cfg)
	if err != nil {
		writeSOCKSReply(conn, 4)
		return err
	}
	if err := writeSOCKSReply(conn, 0); err != nil {
		stream.Close()
		return err
	}
	log.Printf("SMB stream paired through %s for %s:%d", relayAddr, destination, port)
	result := tunnel.PipeWithResult(conn, stream)
	log.Printf("SMB stream ended duration=%s home_to_work_bytes=%d work_to_home_bytes=%d", result.Duration.Round(time.Millisecond), result.AToB.Bytes, result.BToA.Bytes)
	return nil
}

func readSOCKSAddress(reader io.Reader, atyp byte) (string, error) {
	switch atyp {
	case 1:
		data := make([]byte, 4)
		_, err := io.ReadFull(reader, data)
		return net.IP(data).String(), err
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(reader, size[:]); err != nil {
			return "", err
		}
		data := make([]byte, int(size[0]))
		_, err := io.ReadFull(reader, data)
		return string(data), err
	case 4:
		data := make([]byte, 16)
		_, err := io.ReadFull(reader, data)
		return net.IP(data).String(), err
	default:
		return "", fmt.Errorf("unsupported SOCKS address type %d", atyp)
	}
}

func writeSOCKSReply(writer io.Writer, code byte) error {
	_, err := writer.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

func dialSMBRelay(ctx context.Context, cfg homenetwork.Config) (net.Conn, string, error) {
	var failures []error
	for _, relayAddr := range cfg.RelayAddrs {
		attemptStarted := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		headers := http.Header{}
		tunnel.AddProtocolV2Header(headers)
		headers.Set(tunnel.HeaderResumable, "1")
		tunnel.AddHeartbeatHeader(headers)
		headers.Set(tunnel.HeaderRoomProof, cfg.RoomProof)
		tunnel.AddServiceHeader(headers, tunnel.ServiceSMB)
		ws, err := tunnel.DialMessageConnWithHeaders(attemptCtx, relayAddr, cfg.Proxy, tunnel.RoleClient, "", headers)
		if err != nil {
			cancel()
			log.Printf("relay attempt failed relay=%s service=%s elapsed=%s result=%s error=%v", relayAddr, tunnel.ServiceSMB, time.Since(attemptStarted).Round(time.Millisecond), tunnel.TransportFailureResult(err), err)
			failures = append(failures, fmt.Errorf("%s: %w", relayAddr, err))
			continue
		}
		ready, err := tunnel.AwaitSessionReadyCompatibleInfo(attemptCtx, ws)
		if err != nil {
			cancel()
			tunnel.CloseMessageConn(ws)
			log.Printf("relay attempt failed relay=%s service=%s elapsed=%s error=%v", relayAddr, tunnel.ServiceSMB, time.Since(attemptStarted).Round(time.Millisecond), err)
			failures = append(failures, fmt.Errorf("%s: %w", relayAddr, err))
			var rejected *tunnel.SessionResultError
			if errors.As(err, &rejected) && (rejected.Result == tunnel.MessageAuthFailed || rejected.Result == tunnel.MessageServiceDisabled || rejected.Result == tunnel.MessageInvalidRequest) {
				break
			}
			continue
		}
		cancel()
		log.Printf("relay attempt selected relay=%s service=%s protocol_v2=%t heartbeat=%t elapsed=%s", relayAddr, tunnel.ServiceSMB, ready.ProtocolV2, ready.Heartbeat, time.Since(attemptStarted).Round(time.Millisecond))
		if ready.SessionID != "" {
			return tunnel.NewResumableWebSocketConn(ctx, ws, tunnel.ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: cfg.Proxy, SessionID: ready.SessionID, Side: "client", RoomProof: cfg.RoomProof, Service: tunnel.ServiceSMB, Heartbeat: ready.Heartbeat}), relayAddr, nil
		}
		return tunnel.MessageNetConn(ctx, ws), relayAddr, nil
	}
	return nil, "", fmt.Errorf("no relay could open the SMB room: %w", errors.Join(failures...))
}

func logEvent(kind uint32, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	log.Print(message)
	if event, err := eventlog.Open(networkServiceName); err == nil {
		defer event.Close()
		switch kind {
		case eventlog.Error:
			_ = event.Error(1, message)
		case eventlog.Warning:
			_ = event.Warning(1, message)
		default:
			_ = event.Info(1, message)
		}
	}
}
