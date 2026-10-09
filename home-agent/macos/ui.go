//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"deskferry/internal/remotelog"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

type macProfile struct {
	Name        string   `json:"name"`
	RelayBases  []string `json:"relay_bases"`
	Room        string   `json:"room"`
	RoomProof   string   `json:"-"`
	WindowsUser string   `json:"windows_user,omitempty"`
}

type macSettings struct {
	ListenAddr string       `json:"listen_addr"`
	Proxy      string       `json:"proxy"`
	Profiles   []macProfile `json:"profiles"`
	Selected   int          `json:"selected"`
}

type apiProfile struct {
	Name            string   `json:"name"`
	RelayBases      []string `json:"relay_bases"`
	Room            string   `json:"room"`
	HasPassword     bool     `json:"has_password"`
	WindowsUser     string   `json:"windows_user,omitempty"`
	HasWindowsLogin bool     `json:"has_windows_login"`
}

type apiSettings struct {
	ListenAddr string       `json:"listen_addr"`
	Proxy      string       `json:"proxy"`
	Profiles   []apiProfile `json:"profiles"`
	Selected   int          `json:"selected"`
}

type settingsRequest struct {
	Settings          apiSettings `json:"settings"`
	RoomPassword      string      `json:"room_password"`
	ClearPassword     bool        `json:"clear_password"`
	WindowsPassword   string      `json:"windows_password"`
	SaveWindowsLogin  bool        `json:"save_windows_login"`
	ClearWindowsLogin bool        `json:"clear_windows_login"`
}

type macUIController struct {
	root context.Context

	mu           sync.Mutex
	settings     macSettings
	cfg          config
	tunnelCancel context.CancelFunc
	tunnelDone   chan struct{}
	running      bool
	tunnelStatus string
	relayDetails string

	screenCancel context.CancelFunc
	screenStatus string
	screenPNG    []byte
	screenSeq    uint64
}

func runMacUI(ctx context.Context, initial config, openRDP bool) error {
	settings, err := loadMacSettings(initial)
	if err != nil {
		logUI("load saved macOS settings: %v", err)
		settings = settingsFromConfig(initial)
	}
	cfg, err := settingsConfig(settings)
	if err != nil {
		return err
	}
	controller := &macUIController{root: ctx, settings: settings, cfg: cfg, tunnelStatus: "Stopped", screenStatus: "Ready"}
	controller.startTunnel()
	if openRDP {
		_ = launchRDP(cfg)
	}
	go controller.statusLoop()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open macOS Home UI: %w", err)
	}
	token, err := randomUIToken()
	if err != nil {
		listener.Close()
		return err
	}
	prefix := "/" + token
	mux := http.NewServeMux()
	mux.HandleFunc("/", controller.handleUI)
	server := &http.Server{Handler: http.StripPrefix(prefix, mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logUI("macOS Home UI stopped: %v", serveErr)
		}
	}()
	url := "http://" + listener.Addr().String() + prefix + "/"
	if err := exec.Command("open", url).Start(); err != nil {
		_ = server.Close()
		return fmt.Errorf("open macOS Home UI: %w", err)
	}
	logUI("macOS Home control panel: %s", url)
	<-ctx.Done()
	controller.stopTunnel()
	controller.stopScreen()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func (c *macUIController) handleUI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		serveUIPage(w, "index.html")
	case r.Method == http.MethodGet && r.URL.Path == "/screen":
		serveUIPage(w, "screen.html")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/static/"):
		serveUIStatic(w, r, strings.TrimPrefix(r.URL.Path, "/static/"))
	case r.Method == http.MethodGet && r.URL.Path == "/api/settings":
		c.mu.Lock()
		value := apiSettingsFrom(c.settings)
		c.mu.Unlock()
		writeJSON(w, value)
	case r.Method == http.MethodPost && r.URL.Path == "/api/settings":
		var request settingsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.applySettings(request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/connect":
		c.startTunnel()
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/stop":
		c.stopTunnel()
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/open-rdp":
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		if err := launchRDP(cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/winrm":
		var request struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(request.Command) == "" {
			http.Error(w, "PowerShell command is required", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		cfg := c.cfg
		settings := c.settings
		c.mu.Unlock()
		if settings.Selected < 0 || settings.Selected >= len(settings.Profiles) {
			http.Error(w, "selected destination profile is invalid", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		var output bytes.Buffer
		if err := executeMacWinRM(ctx, cfg, settings.Profiles[settings.Selected], request.Command, &output); err != nil {
			message := strings.TrimSpace(output.String())
			if message != "" {
				message += "\n"
			}
			http.Error(w, message+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"output": output.String()})
	case r.Method == http.MethodGet && r.URL.Path == "/api/state":
		c.mu.Lock()
		state := map[string]any{"running": c.running, "tunnel_status": c.tunnelStatus, "relay_details": c.relayDetails, "screen_status": c.screenStatus, "screen_seq": c.screenSeq}
		c.mu.Unlock()
		writeJSON(w, state)
	case r.Method == http.MethodPost && r.URL.Path == "/api/screen/start":
		var request struct {
			Mode       string `json:"mode"`
			IntervalMS int    `json:"interval_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.startScreen(request.Mode, request.IntervalMS); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/screen/stop":
		c.stopScreen()
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/api/screen/frame.png":
		c.mu.Lock()
		data := append([]byte(nil), c.screenPNG...)
		c.mu.Unlock()
		if len(data) == 0 {
			http.Error(w, "no screenshot is available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", "inline; filename=DeskFerry-Screenshot.png")
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (c *macUIController) applySettings(request settingsRequest) error {
	settings, err := settingsFromAPI(request.Settings)
	if err != nil {
		return err
	}
	c.mu.Lock()
	old := c.settings
	c.mu.Unlock()
	proofs := make(map[string]string, len(old.Profiles))
	for _, profile := range old.Profiles {
		proofs[profile.Name+"\x00"+profile.Room] = profile.RoomProof
	}
	for index := range settings.Profiles {
		profile := &settings.Profiles[index]
		profile.RoomProof = proofs[profile.Name+"\x00"+profile.Room]
	}
	// A profile rename keeps its credential when its room and profile count
	// are unchanged. Add/delete operations intentionally require an exact
	// name-and-room match so a credential cannot move to another profile.
	if len(settings.Profiles) == len(old.Profiles) && settings.Selected == old.Selected && settings.Selected >= 0 && settings.Selected < len(old.Profiles) {
		selected := &settings.Profiles[settings.Selected]
		previous := old.Profiles[old.Selected]
		if selected.RoomProof == "" && selected.Room == previous.Room {
			selected.RoomProof = previous.RoomProof
		}
	}
	selected := &settings.Profiles[settings.Selected]
	if request.ClearPassword {
		selected.RoomProof = ""
	} else if request.RoomPassword != "" {
		relay, composeErr := tunnel.RelayRoomURL(selected.RelayBases[0], selected.Room)
		if composeErr != nil {
			return composeErr
		}
		selected.RoomProof = tunnel.RoomPasswordProof(relay, "", request.RoomPassword)
	}
	cfg, err := settingsConfig(settings)
	if err != nil {
		return err
	}
	if err := updateMacWindowsCredential(*selected, request.WindowsPassword, request.SaveWindowsLogin, request.ClearWindowsLogin); err != nil {
		return err
	}
	if err := saveMacSettings(settings); err != nil {
		return err
	}
	c.mu.Lock()
	wasRunning := c.running
	c.settings = settings
	c.cfg = cfg
	c.mu.Unlock()
	if wasRunning {
		c.stopTunnel()
		c.startTunnel()
	}
	return nil
}

func (c *macUIController) startTunnel() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(c.root)
	done := make(chan struct{})
	c.tunnelCancel = cancel
	c.tunnelDone = done
	c.running = true
	c.tunnelStatus = "Connecting"
	cfg := c.cfg
	c.mu.Unlock()
	for _, relayAddr := range cfg.relayAddresses() {
		relayLogs.StartTarget(ctx, remotelog.Target{RelayAddr: relayAddr, Proxy: cfg.Proxy, RoomPassword: cfg.RoomPassword, RoomProof: cfg.RoomProof})
	}
	go func() {
		err := run(ctx, cfg, false)
		c.mu.Lock()
		if c.tunnelDone == done {
			c.running = false
			if err != nil && ctx.Err() == nil {
				c.tunnelStatus = "Stopped: " + err.Error()
			} else {
				c.tunnelStatus = "Stopped"
			}
		}
		close(done)
		c.mu.Unlock()
	}()
	c.mu.Lock()
	c.tunnelStatus = "Listening on " + rdpTarget(cfg.ListenAddr)
	c.mu.Unlock()
}

func (c *macUIController) stopTunnel() {
	c.mu.Lock()
	cancel := c.tunnelCancel
	done := c.tunnelDone
	c.tunnelCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *macUIController) statusLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var client *http.Client
	var clientProxy string
	defer func() {
		if client != nil {
			client.CloseIdleConnections()
		}
	}()
	for {
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		proxyKey := strings.TrimSpace(cfg.Proxy)
		if client == nil || !strings.EqualFold(clientProxy, proxyKey) {
			if client != nil {
				client.CloseIdleConnections()
			}
			client = httpClient(cfg)
			clientProxy = proxyKey
		}
		ctx, cancel := context.WithTimeout(c.root, 8*time.Second)
		summary, err := queryRelaySummaryWithClient(ctx, cfg, client)
		cancel()
		c.mu.Lock()
		if err != nil {
			c.relayDetails = "Relay status unavailable: " + err.Error()
		} else {
			c.relayDetails = formatRelayDetails(summary, cfg)
		}
		c.mu.Unlock()
		select {
		case <-c.root.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *macUIController) startScreen(mode string, interval int) error {
	request := screenview.Request{Mode: mode, IntervalMS: interval, TileSize: screenview.DefaultTileSize}
	if err := request.Normalize(); err != nil {
		return err
	}
	c.mu.Lock()
	if roomProof(c.cfg, c.cfg.primaryRelayAddress()) == "" {
		c.mu.Unlock()
		return errors.New("save a room password for this profile before viewing its screen")
	}
	if c.screenCancel != nil {
		c.screenCancel()
	}
	ctx, cancel := context.WithCancel(c.root)
	c.screenCancel = cancel
	c.screenStatus = "Connecting to the Work screen service"
	cfg := c.cfg
	c.mu.Unlock()
	go c.receiveScreen(ctx, cfg, request)
	return nil
}

func (c *macUIController) receiveScreen(ctx context.Context, cfg config, request screenview.Request) {
	conn, relayAddr, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		c.setScreenStatus("Screen connection failed: " + err.Error())
		return
	}
	defer conn.Close()
	c.setScreenStatus("Connected through " + relayAddr)
	err = screenview.Receive(conn, request, func(frame screenview.Frame, canvas *image.RGBA) error {
		var output bytes.Buffer
		if err := png.Encode(&output, canvas); err != nil {
			return err
		}
		c.mu.Lock()
		c.screenPNG = output.Bytes()
		c.screenSeq++
		if request.Mode == screenview.ModeStream {
			c.screenStatus = fmt.Sprintf("Streaming frame %d (%d changed tiles)", frame.Seq, len(frame.Rects))
		} else {
			c.screenStatus = "Screenshot captured"
		}
		c.mu.Unlock()
		return nil
	})
	if err != nil && ctx.Err() == nil {
		c.setScreenStatus("Screen stream ended: " + err.Error())
	}
}

func (c *macUIController) stopScreen() {
	c.mu.Lock()
	cancel := c.screenCancel
	c.screenCancel = nil
	c.screenStatus = "Screen stream stopped"
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *macUIController) setScreenStatus(value string) {
	c.mu.Lock()
	c.screenStatus = value
	c.mu.Unlock()
}

func settingsFromConfig(cfg config) macSettings {
	bases, room, _ := tunnel.SplitRelayRoomURLs(cfg.relayAddresses())
	if room == "" {
		room = "workdesk"
	}
	if len(bases) == 0 {
		bases = []string{defaultAzureRelayBase, defaultOCIRelayBase}
	}
	proof := cfg.RoomProof
	if proof == "" && cfg.RoomPassword != "" {
		proof = tunnel.RoomPasswordProof(cfg.primaryRelayAddress(), "", cfg.RoomPassword)
	}
	return macSettings{ListenAddr: cfg.ListenAddr, Proxy: cfg.Proxy, Profiles: []macProfile{{Name: "Work", RelayBases: bases, Room: room, RoomProof: proof}}}
}

func settingsConfig(settings macSettings) (config, error) {
	if len(settings.Profiles) == 0 || settings.Selected < 0 || settings.Selected >= len(settings.Profiles) {
		return config{}, errors.New("select a destination profile")
	}
	profile := settings.Profiles[settings.Selected]
	var relays []string
	for _, base := range profile.RelayBases {
		relay, err := tunnel.RelayRoomURL(base, profile.Room)
		if err != nil {
			return config{}, err
		}
		relays = append(relays, relay)
	}
	cfg := config{RelayAddrs: relays, ListenAddr: settings.ListenAddr, Proxy: settings.Proxy, RoomProof: profile.RoomProof}
	cfg.applyDefaults()
	cfg.setRelayAddresses(relays)
	return cfg, cfg.validate()
}

func settingsFromAPI(value apiSettings) (macSettings, error) {
	settings := macSettings{ListenAddr: strings.TrimSpace(value.ListenAddr), Proxy: strings.TrimSpace(value.Proxy), Selected: value.Selected}
	if len(value.Profiles) == 0 {
		return settings, errors.New("at least one destination profile is required")
	}
	for _, profile := range value.Profiles {
		name := strings.TrimSpace(profile.Name)
		room := strings.TrimSpace(profile.Room)
		if name == "" || room == "" {
			return settings, errors.New("profile name and room name are required")
		}
		if len(profile.RelayBases) == 0 {
			return settings, errors.New("each profile needs at least one relay service base URL")
		}
		bases := make([]string, 0, len(profile.RelayBases))
		for _, value := range profile.RelayBases {
			base, err := tunnel.RelayServiceBaseURL(value)
			if err != nil {
				return settings, err
			}
			bases = append(bases, base)
		}
		settings.Profiles = append(settings.Profiles, macProfile{Name: name, Room: room, RelayBases: bases, WindowsUser: strings.TrimSpace(profile.WindowsUser)})
	}
	if settings.Selected < 0 || settings.Selected >= len(settings.Profiles) {
		return settings, errors.New("selected profile is invalid")
	}
	return settings, nil
}

func apiSettingsFrom(settings macSettings) apiSettings {
	value := apiSettings{ListenAddr: settings.ListenAddr, Proxy: settings.Proxy, Selected: settings.Selected}
	for _, profile := range settings.Profiles {
		credential, credentialErr := readMacWindowsCredential(profile)
		windowsUser := profile.WindowsUser
		if strings.TrimSpace(windowsUser) == "" && credentialErr == nil {
			windowsUser = credential.User
		}
		value.Profiles = append(value.Profiles, apiProfile{Name: profile.Name, Room: profile.Room, RelayBases: append([]string(nil), profile.RelayBases...), HasPassword: profile.RoomProof != "", WindowsUser: windowsUser, HasWindowsLogin: credentialErr == nil})
	}
	return value
}

type persistedProfile struct {
	Name        string   `json:"name"`
	RelayBases  []string `json:"relay_bases"`
	Room        string   `json:"room"`
	RoomProof   string   `json:"room_proof,omitempty"`
	WindowsUser string   `json:"windows_user,omitempty"`
}

type persistedSettings struct {
	ListenAddr string             `json:"listen_addr"`
	Proxy      string             `json:"proxy"`
	Profiles   []persistedProfile `json:"profiles"`
	Selected   int                `json:"selected"`
}

func loadMacSettings(initial config) (macSettings, error) {
	path, err := macSettingsPath()
	if err != nil {
		return macSettings{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settingsFromConfig(initial), nil
	}
	if err != nil {
		return macSettings{}, err
	}
	var stored persistedSettings
	if err := json.Unmarshal(data, &stored); err != nil {
		return macSettings{}, err
	}
	settings := macSettings{ListenAddr: stored.ListenAddr, Proxy: stored.Proxy, Selected: stored.Selected}
	for _, profile := range stored.Profiles {
		settings.Profiles = append(settings.Profiles, macProfile{Name: profile.Name, RelayBases: profile.RelayBases, Room: profile.Room, RoomProof: profile.RoomProof, WindowsUser: profile.WindowsUser})
	}
	if _, err := settingsConfig(settings); err != nil {
		return macSettings{}, err
	}
	return settings, nil
}

func saveMacSettings(settings macSettings) error {
	path, err := macSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	stored := persistedSettings{ListenAddr: settings.ListenAddr, Proxy: settings.Proxy, Selected: settings.Selected}
	for _, profile := range settings.Profiles {
		stored.Profiles = append(stored.Profiles, persistedProfile{Name: profile.Name, RelayBases: profile.RelayBases, Room: profile.Room, RoomProof: profile.RoomProof, WindowsUser: profile.WindowsUser})
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func macSettingsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "home-agent.json"), nil
}

func randomUIToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func logUI(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
