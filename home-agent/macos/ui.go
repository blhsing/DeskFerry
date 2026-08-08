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

	"deskferry/internal/buildinfo"
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
		writeHTML(w, strings.ReplaceAll(macControlPanelHTML, "{{VERSION}}", buildinfo.Version))
	case r.Method == http.MethodGet && r.URL.Path == "/screen":
		writeHTML(w, strings.ReplaceAll(macScreenViewerHTML, "{{VERSION}}", buildinfo.Version))
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
	if err := updateMacWindowsCredential(*selected, request.WindowsPassword, request.ClearWindowsLogin); err != nil {
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
	for {
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(c.root, 8*time.Second)
		summary, err := queryRelaySummary(ctx, cfg)
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

func writeHTML(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(value))
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func logUI(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

const macControlPanelHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>DeskFerry Home {{VERSION}}</title><style>
body{font:14px -apple-system,BlinkMacSystemFont,sans-serif;margin:0;background:#f4f5f7;color:#1f2937}main{max-width:1000px;margin:24px auto;padding:0 18px}.card{background:white;border:1px solid #d9dde5;border-radius:12px;padding:18px;margin:12px 0;box-shadow:0 2px 8px #0000000a}h1{font-size:24px}h2{font-size:17px;margin-top:0}.grid{display:grid;grid-template-columns:170px 1fr;gap:10px;align-items:center}input,select,button,textarea{font:inherit;padding:8px;border:1px solid #bcc3cf;border-radius:7px}textarea{width:100%;box-sizing:border-box;min-height:76px}button{background:#fff;cursor:pointer}button.primary{background:#1666d6;color:white;border-color:#1666d6}.row{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px}.relay{display:flex;gap:7px;margin:6px 0}.relay input{flex:1}pre{white-space:pre-wrap;background:#f7f8fa;padding:12px;border-radius:8px;min-height:70px}.good{color:#087a45}.bad{color:#b42318}</style></head><body><main><h1>DeskFerry Home for macOS <small>v{{VERSION}}</small></h1>
<div class="card"><h2>Destination profile</h2><div class="grid"><label>Profile</label><select id="profile"></select><label>Profile name</label><input id="name"><label>Room name</label><input id="room"><label>Relay service bases</label><div><div id="relays"></div><div class="row"><input id="relayEdit" placeholder="https://host/relay" style="flex:1"><button onclick="addRelay()">Add</button></div></div><label>Local RDP address</label><input id="listen"><label>Proxy</label><input id="proxy" placeholder="env, direct, or http(s)://host:port"><label>Room password</label><input id="password" type="password" placeholder="blank keeps saved credential"><label>Room credential</label><label><input id="clear" type="checkbox"> Clear saved room credential</label><label>Windows username</label><input id="windowsUser" placeholder="DOMAIN\\user or local user"><label>Windows password</label><input id="windowsPassword" type="password" placeholder="blank keeps saved Keychain login"><label>Windows login</label><label><input id="clearWindows" type="checkbox"> Forget saved Windows login</label></div><div class="row"><button onclick="addProfile()">Add profile</button><button onclick="deleteProfile()">Delete profile</button><button class="primary" onclick="save()">Save</button></div><div id="saveStatus"></div></div>
<div class="card"><h2>Connection</h2><div class="row"><button class="primary" onclick="post('api/connect')">Connect</button><button onclick="post('api/stop')">Stop</button><button onclick="post('api/open-rdp')">Open Remote Desktop</button><button onclick="window.open('screen','DeskFerryScreen','width=1100,height=760')">Screen Viewer</button></div><p id="tunnel"></p></div>
<div class="card"><h2>WinRM Commands</h2><textarea id="winrmCommand">Get-ComputerInfo | Select-Object CsName, WindowsProductName, WindowsVersion</textarea><div class="row"><button class="primary" onclick="runWinRM()">Execute</button></div><pre id="winrmOutput">Ready</pre></div>
<div class="card"><h2>Relay status</h2><pre id="relay">Checking...</pre></div></main><script>
let s={profiles:[],selected:0};const $=id=>document.getElementById(id);async function load(){s=await (await fetch('api/settings')).json();render();state()}function commit(){if(!s.profiles.length)return;let p=s.profiles[s.selected];p.name=$('name').value;p.room=$('room').value;p.windows_user=$('windowsUser').value}function render(){let q=$('profile');q.innerHTML='';s.profiles.forEach((p,i)=>{let marks=(p.has_password?' room-key':'')+(p.has_windows_login?' Windows-login':'');let o=new Option(p.name+marks,i,i===s.selected,i===s.selected);q.add(o)});let p=s.profiles[s.selected];if(!p)return;$('name').value=p.name;$('room').value=p.room;$('windowsUser').value=p.windows_user||'';$('listen').value=s.listen_addr;$('proxy').value=s.proxy;renderRelays()}$('profile').onchange=()=>{commit();s.selected=+$('profile').value;render()};function renderRelays(){let d=$('relays');d.innerHTML='';s.profiles[s.selected].relay_bases.forEach((v,i)=>{let r=document.createElement('div');r.className='relay';r.innerHTML='<input><button>Update</button><button>Up</button><button>Down</button><button>Delete</button>';r.children[0].value=v;r.children[1].onclick=()=>{s.profiles[s.selected].relay_bases[i]=r.children[0].value};r.children[2].onclick=()=>move(i,-1);r.children[3].onclick=()=>move(i,1);r.children[4].onclick=()=>{s.profiles[s.selected].relay_bases.splice(i,1);renderRelays()};d.appendChild(r)})}function addRelay(){let v=$('relayEdit').value.trim();if(v){s.profiles[s.selected].relay_bases.push(v);$('relayEdit').value='';renderRelays()}}function move(i,n){let a=s.profiles[s.selected].relay_bases,j=i+n;if(j<0||j>=a.length)return;[a[i],a[j]]=[a[j],a[i]];renderRelays()}function addProfile(){commit();s.profiles.push({name:'Work '+(s.profiles.length+1),room:'workdesk',relay_bases:['https://test-officialwebsite.azurewebsites.net/relay','http://217.142.228.117/relay'],has_password:false,windows_user:'',has_windows_login:false});s.selected=s.profiles.length-1;render()}function deleteProfile(){if(s.profiles.length<=1)return alert('At least one profile is required.');s.profiles.splice(s.selected,1);s.selected=Math.max(0,s.selected-1);render()}async function save(){commit();s.listen_addr=$('listen').value;s.proxy=$('proxy').value;let res=await fetch('api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({settings:s,room_password:$('password').value,clear_password:$('clear').checked,windows_password:$('windowsPassword').value,clear_windows_login:$('clearWindows').checked})});let text=await res.text();$('saveStatus').textContent=res.ok?'Saved.':text;if(res.ok){$('password').value='';$('clear').checked=false;$('windowsPassword').value='';$('clearWindows').checked=false;await load()}}async function runWinRM(){let o=$('winrmOutput');o.textContent='Running...';let r=await fetch('api/winrm',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({command:$('winrmCommand').value})});let text=await r.text();if(r.ok){try{text=JSON.parse(text).output}catch(e){}}o.textContent=text||'(no output)'}async function post(path){let r=await fetch(path,{method:'POST'});if(!r.ok)alert(await r.text());state()}async function state(){let x=await (await fetch('api/state')).json();$('tunnel').textContent=x.tunnel_status;$('tunnel').className=x.running?'good':'bad';$('relay').textContent=x.relay_details||'Checking...'}setInterval(state,2000);load();</script></body></html>`

const macScreenViewerHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>DeskFerry Screen Viewer</title>
<style>
html,body{height:100%;margin:0;overflow:hidden}body{font:14px -apple-system,sans-serif;background:#171a20;color:white;display:flex;flex-direction:column}
header{display:flex;gap:8px;align-items:center;padding:10px;background:#252a33;flex:none;white-space:nowrap;overflow-x:auto}
button,select,input{font:inherit;padding:7px 10px;border-radius:7px;border:1px solid #727987;background:#343a46;color:white}input{width:78px}
#status{overflow:hidden;text-overflow:ellipsis}#view{flex:1;min-height:0;overflow:auto;background:#000;cursor:grab;touch-action:none;user-select:none}
#view.dragging{cursor:grabbing}#stage{min-width:100%;min-height:100%;display:flex;justify-content:center;align-items:center}
#image{display:block;pointer-events:none;-webkit-user-drag:none}#stage.fit #image{width:100%;height:100%;object-fit:contain}
</style></head><body>
<header>
<button onclick="start('single')">Capture Once</button><button onclick="start('stream')">Start Stream</button><button onclick="post('api/screen/stop')">Stop</button>
<label>Interval <select id="interval"><option value="500">0.5 s</option><option value="1000" selected>1 s</option><option value="2000">2 s</option><option value="5000">5 s</option></select></label>
<label>Zoom <input id="zoom" value="Auto Fit" list="zoom-levels" aria-label="Zoom level"></label>
<datalist id="zoom-levels"><option>Auto Fit</option><option>25%</option><option>50%</option><option>75%</option><option>100%</option><option>125%</option><option>150%</option><option>200%</option><option>300%</option><option>400%</option></datalist>
<button onclick="setZoom(0)">Auto Fit</button><button onclick="toggleFullscreen()">Full Screen</button>
<a id="save" href="api/screen/frame.png" download="DeskFerry-Screenshot.png"><button>Save PNG</button></a><span id="status">Ready</span>
</header>
<div id="view"><div id="stage" class="fit"><img id="image" draggable="false" alt="Capture the Work computer screen"></div></div>
<script>
const view=document.getElementById('view'),stage=document.getElementById('stage'),image=document.getElementById('image'),zoomInput=document.getElementById('zoom');
let seq=0,zoom=0,gestureBase=1,drag=null;
function clamp(value){return Math.max(.1,Math.min(16,value))}
function effectiveZoom(){if(zoom)return zoom;if(!image.naturalWidth||!image.naturalHeight)return 1;return Math.min(view.clientWidth/image.naturalWidth,view.clientHeight/image.naturalHeight)}
function formatZoom(value){return value===0?'Auto Fit':(Math.round(value*1000)/10).toString().replace(/\.0$/,'')+'%'}
function applyZoom(){if(!image.naturalWidth||!image.naturalHeight)return;if(zoom===0){stage.classList.add('fit');stage.style.width='100%';stage.style.height='100%';image.style.width='100%';image.style.height='100%'}else{stage.classList.remove('fit');let w=Math.round(image.naturalWidth*zoom),h=Math.round(image.naturalHeight*zoom);stage.style.width=Math.max(view.clientWidth,w)+'px';stage.style.height=Math.max(view.clientHeight,h)+'px';image.style.width=w+'px';image.style.height=h+'px'}zoomInput.value=formatZoom(zoom)}
function setZoom(value,anchor){let oldWidth=image.getBoundingClientRect().width||1,oldHeight=image.getBoundingClientRect().height||1,rect=view.getBoundingClientRect(),ax=anchor?anchor.x-rect.left:view.clientWidth/2,ay=anchor?anchor.y-rect.top:view.clientHeight/2,rx=(view.scrollLeft+ax)/oldWidth,ry=(view.scrollTop+ay)/oldHeight;zoom=value===0?0:clamp(value);applyZoom();if(zoom!==0){view.scrollLeft=rx*(image.getBoundingClientRect().width||1)-ax;view.scrollTop=ry*(image.getBoundingClientRect().height||1)-ay}}
function parseZoom(){let text=zoomInput.value.trim();if(/^(auto( fit)?|fit)$/i.test(text)){setZoom(0);return}let value=Number(text.replace('%','').trim());if(Number.isFinite(value)&&value>=10&&value<=1600)setZoom(value/100);else{zoomInput.value=formatZoom(zoom);document.getElementById('status').textContent='Zoom must be Auto Fit or 10% through 1600%.'}}
zoomInput.addEventListener('change',parseZoom);zoomInput.addEventListener('keydown',event=>{if(event.key==='Enter'){parseZoom();zoomInput.blur()}});
view.addEventListener('wheel',event=>{event.preventDefault();let factor=event.ctrlKey?Math.exp(-event.deltaY*.01):(event.deltaY<0?1.1:1/1.1);setZoom(effectiveZoom()*factor,{x:event.clientX,y:event.clientY})},{passive:false});
view.addEventListener('gesturestart',event=>{event.preventDefault();gestureBase=effectiveZoom()},{passive:false});
view.addEventListener('gesturechange',event=>{event.preventDefault();setZoom(gestureBase*event.scale,{x:event.clientX,y:event.clientY})},{passive:false});
view.addEventListener('pointerdown',event=>{if(view.scrollWidth<=view.clientWidth&&view.scrollHeight<=view.clientHeight)return;drag={x:event.clientX,y:event.clientY,left:view.scrollLeft,top:view.scrollTop};view.setPointerCapture(event.pointerId);view.classList.add('dragging')});
view.addEventListener('pointermove',event=>{if(!drag)return;view.scrollLeft=drag.left-(event.clientX-drag.x);view.scrollTop=drag.top-(event.clientY-drag.y)});
function endDrag(){drag=null;view.classList.remove('dragging')}view.addEventListener('pointerup',endDrag);view.addEventListener('pointercancel',endDrag);
image.addEventListener('load',applyZoom);window.addEventListener('resize',applyZoom);
async function toggleFullscreen(){if(document.fullscreenElement)await document.exitFullscreen();else await document.documentElement.requestFullscreen()}
async function start(mode){let r=await fetch('api/screen/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({mode:mode,interval_ms:+document.getElementById('interval').value})});if(!r.ok)alert(await r.text())}
async function post(p){await fetch(p,{method:'POST'})}
async function poll(){let s=await (await fetch('api/state')).json();document.getElementById('status').textContent=s.screen_status;if(s.screen_seq&&s.screen_seq!==seq){seq=s.screen_seq;image.src='api/screen/frame.png?seq='+seq}}
setInterval(poll,350);poll();start('single');
</script></body></html>`
