//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
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
	"sync"
	"syscall"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"nhooyr.io/websocket"

	"deskferry/internal/diaglog"
	"deskferry/internal/tunnel"
	"deskferry/internal/wincred"
)

const (
	defaultRelayURL        = "https://test-officialwebsite.azurewebsites.net/relay/workdesk"
	defaultListenAddr      = "127.0.0.1:3390"
	defaultWinRMListenAddr = "127.0.0.1:3391"
	singleInstanceName     = `Global\DeskFerryHomeAgent`
	appIconResourceID      = 2
	statusTileWidth        = 150
	rdpStatusTileWidth     = 230
)

type config struct {
	ListenAddr          string               `json:"listen_addr"`
	RelayAddr           string               `json:"relay_addr"`
	RelayAddrs          []string             `json:"relay_addrs,omitempty"`
	Proxy               string               `json:"proxy"`
	RDPUser             string               `json:"rdp_user,omitempty"`
	WinRMListenAddr     string               `json:"winrm_listen_addr,omitempty"`
	WinRMUser           string               `json:"winrm_user,omitempty"`
	RoomProof           string               `json:"room_proof,omitempty"`
	Destinations        []destinationProfile `json:"destinations,omitempty"`
	SelectedDestination string               `json:"selected_destination,omitempty"`
}

type destinationProfile struct {
	Name        string   `json:"name"`
	RelayAddrs  []string `json:"relay_addrs"`
	RoomProof   string   `json:"room_proof,omitempty"`
	WindowsUser string   `json:"windows_user,omitempty"`
}

type relayURLFlag []string

func (f *relayURLFlag) Set(value string) error {
	*f = append(*f, splitRelayURLs(value)...)
	return nil
}

func (f *relayURLFlag) String() string {
	return joinRelayURLs([]string(*f))
}

type clientApp struct {
	mw *walk.MainWindow
	ni *walk.NotifyIcon

	relayList          *walk.ListBox
	relayEdit          *walk.LineEdit
	relayAdd           *walk.PushButton
	relayUpdate        *walk.PushButton
	relayDelete        *walk.PushButton
	relayUp            *walk.PushButton
	relayDown          *walk.PushButton
	destinationList    *walk.ComboBox
	destinationEdit    *walk.LineEdit
	destinationAdd     *walk.PushButton
	destinationRename  *walk.PushButton
	destinationDelete  *walk.PushButton
	listenAddr         *walk.LineEdit
	proxy              *walk.LineEdit
	rdpUser            *walk.LineEdit
	rdpPass            *walk.LineEdit
	roomPass           *walk.LineEdit
	clearRoomPassword  *walk.CheckBox
	winrmListen        *walk.LineEdit
	winrmCommand       *walk.TextEdit
	winrmOutput        *walk.TextEdit
	executeWinRMButton *walk.PushButton

	tunnelStatus *walk.Label
	workStatus   *walk.Label
	homeStatus   *walk.Label
	rdpStatus    *walk.Label
	details      *walk.TextEdit
	logView      *walk.TextEdit

	connectButton *walk.PushButton
	openRDPButton *walk.PushButton

	trayOpen    *walk.Action
	trayConnect *walk.Action
	trayStop    *walk.Action
	trayRDP     *walk.Action

	mu                  sync.Mutex
	cfg                 config
	relayURLs           []string
	destinations        []destinationProfile
	selectedDestination int
	changingDestination bool
	relayDragIndex      int
	relayDragStartY     int
	relayDragging       bool
	cancel              context.CancelFunc
	listener            net.Listener
	winrmListener       net.Listener
	activeLocal         int
	activeWinRM         int
	statusCancel        context.CancelFunc
	exiting             bool
}

type relaySnapshot struct {
	Service string              `json:"service"`
	Time    time.Time           `json:"time"`
	Rooms   []relayRoomSnapshot `json:"rooms"`
}

type relayRoomSnapshot struct {
	ID                       string    `json:"id"`
	WaitingAgents            int       `json:"waiting_agents"`
	ActivePairs              int       `json:"active_pairs"`
	TotalPairs               int64     `json:"total_pairs"`
	LastAgentRemote          string    `json:"last_agent_remote"`
	LastAgentConnectedAt     time.Time `json:"last_agent_connected_at"`
	HomeAgentConnected       bool      `json:"home_agent_connected"`
	HomeAgentRemote          string    `json:"home_agent_remote"`
	HomeAgentConnectedAt     time.Time `json:"home_agent_connected_at"`
	LastClientRemote         string    `json:"last_client_remote"`
	LastClientConnectedAt    time.Time `json:"last_client_connected_at"`
	LastClientDisconnectedAt time.Time `json:"last_client_disconnected_at"`
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
	CheckedAt  time.Time
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var relayURLs relayURLFlag
	var listenAddr string
	var proxyFlag string
	var logRetentionDays int
	var consoleMode bool
	var smokeTest bool
	flag.Var(&relayURLs, "relay-url", "relay room URL; repeat to add fallback URLs")
	flag.StringVar(&listenAddr, "listen", "", "local RDP listen address")
	flag.StringVar(&proxyFlag, "proxy", "", "proxy: env, direct, or http(s)://host:port")
	flag.IntVar(&logRetentionDays, "log-retention-days", diaglog.DefaultRetentionDays, "number of calendar days of diagnostic logs to retain")
	flag.BoolVar(&consoleMode, "console", false, "run in the foreground instead of the control panel")
	flag.BoolVar(&smokeTest, "ui-smoke-test", false, "start and close the GUI")
	flag.Parse()
	if path, err := diaglog.Enable("home-agent", false, logRetentionDays); err != nil {
		log.Printf("persistent diagnostic logging unavailable: %v", err)
	} else {
		log.Printf("diagnostic log file: %s retention_days=%d", path, logRetentionDays)
	}

	if !smokeTest {
		instance, alreadyRunning, err := acquireNamedInstanceMutex(singleInstanceName)
		if err != nil {
			windowsMessageBox(appTitle(), "Check for an existing DeskFerry Home instance: "+err.Error(), windows.MB_OK|windows.MB_ICONERROR)
			os.Exit(1)
		}
		if alreadyRunning {
			activateExistingHomeWindow()
			log.Print("DeskFerry Home is already running")
			return
		}
		defer windows.CloseHandle(instance)
	}

	cfg, err := loadConfig(relayURLs.String(), listenAddr, proxyFlag)
	if err != nil {
		windowsMessageBox(appTitle(), err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}
	if consoleMode {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := run(ctx, cfg, false); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
		return
	}

	app := &clientApp{cfg: cfg, relayDragIndex: -1}
	if err := app.run(smokeTest); err != nil {
		windowsMessageBox(appTitle(), err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}
}

func appTitle() string {
	return "DeskFerry Home"
}

func acquireNamedInstanceMutex(name string) (windows.Handle, bool, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	handle, err := windows.CreateMutex(nil, false, namePtr)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return 0, true, nil
	}
	// A mutex created by another Windows account can exist without granting this
	// account MUTEX_ALL_ACCESS. Treat access denied as an existing instance.
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return 0, true, nil
	}
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return 0, false, err
	}
	return handle, false, nil
}

func activateExistingHomeWindow() {
	title, err := windows.UTF16PtrFromString(appTitle())
	if err != nil {
		return
	}
	window := win.FindWindow(nil, title)
	if window == 0 {
		return
	}
	win.ShowWindow(window, win.SW_RESTORE)
	win.SetForegroundWindow(window)
}

func (a *clientApp) run(smokeTest bool) error {
	window := MainWindow{
		AssignTo: &a.mw,
		Title:    appTitle(),
		MinSize:  Size{Width: 1250, Height: 600},
		Size:     Size{Width: 1320, Height: 680},
		Icon:     appIcon(),
		Layout:   VBox{MarginsZero: true},
		Visible:  !smokeTest,
		Children: []Widget{
			Composite{
				Layout: VBox{Margins: Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 9},
				Children: []Widget{
					GroupBox{
						Title:  "Status",
						Layout: Grid{Columns: 4, Spacing: 8},
						Children: []Widget{
							statusTile("Tunnel", &a.tunnelStatus, "Stopped", statusTileWidth),
							statusTile("Work Agent", &a.workStatus, "Checking", statusTileWidth),
							statusTile("Home App", &a.homeStatus, "Connecting", statusTileWidth),
							statusTile("RDP", &a.rdpStatus, defaultListenAddr, rdpStatusTileWidth),
						},
					},
					Composite{
						StretchFactor: 1,
						Layout:        HBox{MarginsZero: true, Spacing: 9},
						Children: []Widget{
							GroupBox{
								Title:         "Connection",
								MinSize:       Size{Width: 580},
								StretchFactor: 2,
								Layout:        Grid{Columns: 4, Spacing: 7},
								Children: []Widget{
									Label{Text: "Destination"},
									ComboBox{
										AssignTo:              &a.destinationList,
										Model:                 destinationNames(a.cfg.Destinations),
										OnCurrentIndexChanged: a.destinationSelectionChanged,
									},
									LineEdit{AssignTo: &a.destinationEdit, CueBanner: "Work destination name"},
									Composite{
										Layout: Grid{Columns: 3, MarginsZero: true, Spacing: 5},
										Children: []Widget{
											PushButton{AssignTo: &a.destinationAdd, Text: "Add", OnClicked: a.addDestination},
											PushButton{AssignTo: &a.destinationRename, Text: "Rename", OnClicked: a.renameDestination},
											PushButton{AssignTo: &a.destinationDelete, Text: "Delete", OnClicked: a.deleteDestination},
										},
									},
									Label{Text: "Relay URLs"},
									Composite{
										ColumnSpan: 3,
										Layout:     VBox{Spacing: 6},
										Children: []Widget{
											ListBox{
												AssignTo:              &a.relayList,
												Model:                 a.cfg.relayAddresses(),
												MinSize:               Size{Height: 74},
												OnCurrentIndexChanged: a.relaySelectionChanged,
												OnMouseDown:           a.relayListMouseDown,
												OnMouseMove:           a.relayListMouseMove,
												OnMouseUp:             a.relayListMouseUp,
											},
											Composite{
												Layout: Grid{Columns: 4, Spacing: 6},
												Children: []Widget{
													Label{Text: "Selected URL"},
													LineEdit{AssignTo: &a.relayEdit, CueBanner: defaultRelayURL, ColumnSpan: 3},
												},
											},
											Composite{
												Layout: Grid{Columns: 5, Spacing: 6},
												Children: []Widget{
													PushButton{AssignTo: &a.relayAdd, Text: "Add", MinSize: Size{Height: 30}, OnClicked: a.addRelayURL},
													PushButton{AssignTo: &a.relayUpdate, Text: "Update", MinSize: Size{Height: 30}, OnClicked: a.updateRelayURL},
													PushButton{AssignTo: &a.relayDelete, Text: "Delete", MinSize: Size{Height: 30}, OnClicked: a.deleteRelayURL},
													PushButton{AssignTo: &a.relayUp, Text: "Up", MinSize: Size{Height: 30}, OnClicked: func() { a.moveRelayURL(-1) }},
													PushButton{AssignTo: &a.relayDown, Text: "Down", MinSize: Size{Height: 30}, OnClicked: func() { a.moveRelayURL(1) }},
												},
											},
										},
									},
									Composite{
										ColumnSpan: 4,
										Layout:     VBox{MarginsZero: true, Spacing: 7},
										Children: []Widget{
											connectionPair(
												"Local RDP address", LineEdit{AssignTo: &a.listenAddr, Text: a.cfg.ListenAddr, CueBanner: defaultListenAddr, StretchFactor: 1},
												"Proxy", LineEdit{AssignTo: &a.proxy, Text: a.cfg.Proxy, CueBanner: "env, direct, or http(s)://host:port", StretchFactor: 1},
											),
											connectionPair(
												"Room password", LineEdit{AssignTo: &a.roomPass, PasswordMode: true, CueBanner: "optional room password", MinSize: Size{Width: 162}, MaxSize: Size{Width: 162}, StretchFactor: 1},
												"Password options", CheckBox{AssignTo: &a.clearRoomPassword, Text: "Clear saved room credential", StretchFactor: 1},
											),
											connectionPair(
												"Windows username", LineEdit{AssignTo: &a.rdpUser, Text: a.cfg.RDPUser, CueBanner: `DOMAIN\user or user@example.com`, StretchFactor: 1},
												"Windows password", LineEdit{AssignTo: &a.rdpPass, PasswordMode: true, CueBanner: "blank uses the saved profile login", StretchFactor: 1},
											),
										},
									},
									Composite{
										ColumnSpan: 4,
										Layout:     Grid{Columns: 4, Spacing: 6},
										Children: []Widget{
											PushButton{AssignTo: &a.connectButton, Text: "Connect", MinSize: Size{Height: 30}, OnClicked: func() { a.connectFromUI() }},
											PushButton{AssignTo: &a.openRDPButton, Text: "Open Remote Desktop", MinSize: Size{Height: 30}, OnClicked: a.openRemoteDesktop},
											PushButton{Text: "Save", MinSize: Size{Height: 30}, OnClicked: func() { a.saveFromUI(true) }},
											PushButton{Text: "Copy RDP Address", MinSize: Size{Height: 30}, OnClicked: a.copyRDPAddress},
											PushButton{Text: "Save Windows Login", MinSize: Size{Height: 30}, OnClicked: a.saveWindowsCredentials},
											PushButton{Text: "Forget Windows Login", MinSize: Size{Height: 30}, OnClicked: a.forgetWindowsCredentials},
											PushButton{Text: "Relay Dashboard", MinSize: Size{Height: 30}, OnClicked: a.openDashboard},
											PushButton{Text: "Refresh", MinSize: Size{Height: 30}, OnClicked: a.refreshRelayStatusAsync},
										},
									},
								},
							},
							GroupBox{
								Title:         "WinRM Commands (uses the shared Windows login at left)",
								MinSize:       Size{Width: 320},
								StretchFactor: 2,
								Layout:        VBox{Spacing: 6},
								Children: []Widget{
									Composite{
										Layout: Grid{Columns: 3, Spacing: 7},
										Children: []Widget{
											Label{Text: "Local WinRM address"},
											LineEdit{AssignTo: &a.winrmListen, Text: a.cfg.WinRMListenAddr, CueBanner: defaultWinRMListenAddr},
											PushButton{AssignTo: &a.executeWinRMButton, Text: "Execute", OnClicked: a.executeWinRM},
										},
									},
									Label{Text: "PowerShell command"},
									TextEdit{AssignTo: &a.winrmCommand, MinSize: Size{Height: 48}, Text: "Get-ComputerInfo | Select-Object CsName, WindowsProductName, WindowsVersion"},
									Label{Text: "Output"},
									TextEdit{AssignTo: &a.winrmOutput, ReadOnly: true, VScroll: true, MinSize: Size{Height: 72}},
								},
							},
							GroupBox{
								Title:         "Monitoring",
								MinSize:       Size{Width: 320},
								StretchFactor: 2,
								Layout:        VBox{Spacing: 5},
								Children: []Widget{
									Label{Text: "Room Details"},
									TextEdit{AssignTo: &a.details, ReadOnly: true, VScroll: true, MinSize: Size{Height: 90}, StretchFactor: 1, Text: "Checking relay room..."},
									Label{Text: "Activity"},
									TextEdit{AssignTo: &a.logView, ReadOnly: true, VScroll: true, MinSize: Size{Height: 90}, StretchFactor: 1},
								},
							},
						},
					},
				},
			},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	a.setDestinations(a.cfg.Destinations, a.cfg.SelectedDestination)
	a.setRelayURLList(a.cfg.relayAddresses(), 0)
	if err := a.setupNotifyIcon(); err != nil {
		return err
	}
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if a.exiting || smokeTest {
			return
		}
		*canceled = true
		a.mw.Hide()
		_ = a.ni.ShowInfo(appTitle(), "Still running in the notification area.")
	})

	a.appendLog("Ready.")
	a.refreshLocalState()
	a.restartHomePresence()
	a.refreshRelayStatusAsync()
	a.startStatusPoller()
	if !smokeTest {
		if err := a.startTunnel(false); err != nil {
			a.appendLog("Could not restore local RDP listener: %v", err)
		}
	}

	if smokeTest {
		time.AfterFunc(350*time.Millisecond, func() {
			a.onUI(func() {
				a.exiting = true
				_ = a.mw.Close()
			})
		})
	}
	a.mw.Run()
	a.shutdown()
	return nil
}

func connectionPair(leftTitle string, leftField Widget, rightTitle string, rightField Widget) Widget {
	return Composite{
		Layout: HBox{MarginsZero: true, Spacing: 7},
		Children: []Widget{
			connectionField(leftTitle, leftField),
			connectionField(rightTitle, rightField),
		},
	}
}

func connectionField(title string, field Widget) Widget {
	return Composite{
		MinSize:       Size{Width: 270},
		StretchFactor: 1,
		Layout:        Grid{Columns: 2, MarginsZero: true, Spacing: 6},
		Children: []Widget{
			Label{Text: title, MinSize: Size{Width: 108}, MaxSize: Size{Width: 108}},
			field,
		},
	}
}

func statusTile(title string, assignTo **walk.Label, initial string, width int) Widget {
	return Composite{
		MinSize:       Size{Width: width, Height: 66},
		StretchFactor: 1,
		Layout:        VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 8}, Spacing: 4},
		Children: []Widget{
			Label{Text: title, TextColor: walk.RGB(93, 104, 116), Font: Font{Bold: true}, MinSize: Size{Width: width - 16}},
			Label{
				AssignTo:      assignTo,
				Text:          initial,
				Font:          Font{PointSize: 13, Bold: true},
				EllipsisMode:  EllipsisEnd,
				MinSize:       Size{Width: width - 16, Height: 26},
				TextColor:     walk.RGB(31, 41, 55),
				TextAlignment: AlignNear,
			},
		},
	}
}

func (a *clientApp) relayURLListValues() []string {
	return append([]string(nil), a.relayURLs...)
}

func destinationNames(values []destinationProfile) []string {
	names := make([]string, 0, len(values))
	for _, value := range values {
		names = append(names, value.Name)
	}
	return names
}

func cloneDestinations(values []destinationProfile) []destinationProfile {
	out := make([]destinationProfile, len(values))
	for i, value := range values {
		out[i] = destinationProfile{Name: value.Name, RelayAddrs: append([]string(nil), value.RelayAddrs...), RoomProof: value.RoomProof, WindowsUser: value.WindowsUser}
	}
	return out
}

func (a *clientApp) setDestinations(values []destinationProfile, selectedName string) {
	a.destinations = cloneDestinations(values)
	a.selectedDestination = 0
	for i, value := range a.destinations {
		if strings.EqualFold(value.Name, selectedName) {
			a.selectedDestination = i
			break
		}
	}
	a.changingDestination = true
	if a.destinationList != nil {
		_ = a.destinationList.SetModel(destinationNames(a.destinations))
		_ = a.destinationList.SetCurrentIndex(a.selectedDestination)
	}
	a.changingDestination = false
	a.updateDestinationEditor()
	a.updateDestinationButtons()
}

func (a *clientApp) updateDestinationEditor() {
	if a.destinationEdit == nil {
		return
	}
	if a.selectedDestination >= 0 && a.selectedDestination < len(a.destinations) {
		_ = a.destinationEdit.SetText(a.destinations[a.selectedDestination].Name)
		if a.roomPass != nil {
			_ = a.roomPass.SetText("")
			if a.destinations[a.selectedDestination].RoomProof != "" {
				a.roomPass.SetCueBanner("saved room credential configured")
			} else {
				a.roomPass.SetCueBanner("optional room password")
			}
		}
		if a.clearRoomPassword != nil {
			a.clearRoomPassword.SetChecked(false)
		}
		if a.rdpUser != nil {
			_ = a.rdpUser.SetText(a.destinations[a.selectedDestination].WindowsUser)
		}
		if a.rdpPass != nil {
			_ = a.rdpPass.SetText("")
			a.rdpPass.SetCueBanner("blank uses the saved profile login")
		}
	}
}

func (a *clientApp) updateDestinationButtons() {
	valid := a.selectedDestination >= 0 && a.selectedDestination < len(a.destinations)
	if a.destinationRename != nil {
		a.destinationRename.SetEnabled(valid)
	}
	if a.destinationDelete != nil {
		a.destinationDelete.SetEnabled(valid && len(a.destinations) > 1)
	}
}

func (a *clientApp) commitDestinationProfile() {
	if a.selectedDestination >= 0 && a.selectedDestination < len(a.destinations) {
		a.destinations[a.selectedDestination].RelayAddrs = a.relayURLListValues()
		if a.rdpUser != nil {
			a.destinations[a.selectedDestination].WindowsUser = strings.TrimSpace(a.rdpUser.Text())
		}
	}
}

func (a *clientApp) destinationSelectionChanged() {
	if a.changingDestination || a.destinationList == nil {
		return
	}
	index := a.destinationList.CurrentIndex()
	if index < 0 || index >= len(a.destinations) || index == a.selectedDestination {
		return
	}
	if a.isTunnelRunning() {
		a.changingDestination = true
		_ = a.destinationList.SetCurrentIndex(a.selectedDestination)
		a.changingDestination = false
		return
	}
	a.commitDestinationProfile()
	a.selectedDestination = index
	a.setRelayURLList(a.destinations[index].RelayAddrs, 0)
	a.updateDestinationEditor()
	a.persistDestinationSelection()
}

func (a *clientApp) destinationNameFromEditor() (string, error) {
	name := strings.TrimSpace(a.destinationEdit.Text())
	if name == "" {
		return "", errors.New("destination name is required")
	}
	return name, nil
}

func (a *clientApp) uniqueDestinationName(requested string, ignored int) string {
	base := strings.TrimSpace(requested)
	candidate := base
	for suffix := 2; ; suffix++ {
		found := false
		for i, value := range a.destinations {
			if i != ignored && strings.EqualFold(value.Name, candidate) {
				found = true
				break
			}
		}
		if !found {
			return candidate
		}
		candidate = fmt.Sprintf("%s %d", base, suffix)
	}
}

func (a *clientApp) addDestination() {
	name, err := a.destinationNameFromEditor()
	if err != nil {
		a.showError(err)
		return
	}
	a.commitDestinationProfile()
	a.destinations = append(a.destinations, destinationProfile{
		Name: a.uniqueDestinationName(name, -1), RelayAddrs: []string{defaultRelayURL},
	})
	a.selectedDestination = len(a.destinations) - 1
	a.setDestinations(a.destinations, a.destinations[a.selectedDestination].Name)
	a.setRelayURLList(a.destinations[a.selectedDestination].RelayAddrs, 0)
	a.persistDestinationSelection()
}

func (a *clientApp) renameDestination() {
	if a.selectedDestination < 0 || a.selectedDestination >= len(a.destinations) {
		return
	}
	name, err := a.destinationNameFromEditor()
	if err != nil {
		a.showError(err)
		return
	}
	a.destinations[a.selectedDestination].Name = a.uniqueDestinationName(name, a.selectedDestination)
	selected := a.destinations[a.selectedDestination].Name
	a.setDestinations(a.destinations, selected)
	a.persistDestinationSelection()
}

func (a *clientApp) deleteDestination() {
	if len(a.destinations) <= 1 || a.selectedDestination < 0 || a.selectedDestination >= len(a.destinations) {
		return
	}
	index := a.selectedDestination
	a.destinations = append(a.destinations[:index], a.destinations[index+1:]...)
	if index >= len(a.destinations) {
		index = len(a.destinations) - 1
	}
	selected := a.destinations[index].Name
	a.setDestinations(a.destinations, selected)
	a.setRelayURLList(a.destinations[a.selectedDestination].RelayAddrs, 0)
	a.persistDestinationSelection()
}

func (a *clientApp) persistDestinationSelection() {
	if len(a.destinations) == 0 || a.selectedDestination < 0 || a.selectedDestination >= len(a.destinations) {
		return
	}
	a.commitDestinationProfile()
	cfg := a.currentConfig()
	cfg.Destinations = cloneDestinations(a.destinations)
	cfg.SelectedDestination = a.destinations[a.selectedDestination].Name
	cfg.RDPUser = a.destinations[a.selectedDestination].WindowsUser
	cfg.setRelayAddresses(a.destinations[a.selectedDestination].RelayAddrs)
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	if err := saveSettingsConfig(cfg); err != nil {
		a.showError(err)
		return
	}
	a.restartHomePresence()
	a.refreshRelayStatusAsync()
}

func (a *clientApp) setRelayURLList(values []string, selectIndex int) {
	a.relayURLs = uniqueRelayURLs(values)
	if len(a.relayURLs) == 0 {
		selectIndex = -1
	} else if selectIndex < 0 {
		selectIndex = 0
	} else if selectIndex >= len(a.relayURLs) {
		selectIndex = len(a.relayURLs) - 1
	}
	if a.relayList != nil {
		_ = a.relayList.SetModel(append([]string(nil), a.relayURLs...))
		_ = a.relayList.SetCurrentIndex(selectIndex)
	}
	a.setRelayEditorFromIndex(selectIndex)
	a.updateRelayButtons()
}

func (a *clientApp) setRelayEditorFromIndex(index int) {
	if a.relayEdit == nil {
		return
	}
	if index >= 0 && index < len(a.relayURLs) {
		_ = a.relayEdit.SetText(a.relayURLs[index])
		return
	}
	_ = a.relayEdit.SetText("")
}

func (a *clientApp) relaySelectionChanged() {
	index := -1
	if a.relayList != nil {
		index = a.relayList.CurrentIndex()
	}
	a.setRelayEditorFromIndex(index)
	a.updateRelayButtons()
}

func (a *clientApp) updateRelayButtons() {
	if a.relayList == nil {
		return
	}
	index := a.relayList.CurrentIndex()
	hasSelection := index >= 0 && index < len(a.relayURLs)
	if a.relayUpdate != nil {
		a.relayUpdate.SetEnabled(hasSelection)
	}
	if a.relayDelete != nil {
		a.relayDelete.SetEnabled(hasSelection)
	}
	if a.relayUp != nil {
		a.relayUp.SetEnabled(hasSelection && index > 0)
	}
	if a.relayDown != nil {
		a.relayDown.SetEnabled(hasSelection && index < len(a.relayURLs)-1)
	}
}

func (a *clientApp) relayURLFromEditor() (string, error) {
	if a.relayEdit == nil {
		return "", errors.New("relay URL editor is not available")
	}
	return normalizeRelayURL(a.relayEdit.Text())
}

func (a *clientApp) addRelayURL() {
	value, err := a.relayURLFromEditor()
	if err != nil {
		a.showError(err)
		return
	}
	values := a.relayURLListValues()
	for i, existing := range values {
		if strings.EqualFold(existing, value) {
			a.setRelayURLList(values, i)
			return
		}
	}
	values = append(values, value)
	a.setRelayURLList(values, len(values)-1)
}

func (a *clientApp) updateRelayURL() {
	index := -1
	if a.relayList != nil {
		index = a.relayList.CurrentIndex()
	}
	if index < 0 || index >= len(a.relayURLs) {
		a.addRelayURL()
		return
	}
	value, err := a.relayURLFromEditor()
	if err != nil {
		a.showError(err)
		return
	}
	values := a.relayURLListValues()
	values[index] = value
	values = uniqueRelayURLs(values)
	nextIndex := index
	for i, existing := range values {
		if strings.EqualFold(existing, value) {
			nextIndex = i
			break
		}
	}
	a.setRelayURLList(values, nextIndex)
}

func (a *clientApp) deleteRelayURL() {
	if a.relayList == nil {
		return
	}
	index := a.relayList.CurrentIndex()
	if index < 0 || index >= len(a.relayURLs) {
		return
	}
	values := a.relayURLListValues()
	values = append(values[:index], values[index+1:]...)
	a.setRelayURLList(values, index)
}

func (a *clientApp) moveRelayURL(delta int) {
	if a.relayList == nil {
		return
	}
	index := a.relayList.CurrentIndex()
	a.moveRelayURLTo(index, index+delta)
}

func (a *clientApp) moveRelayURLTo(from, to int) {
	if from < 0 || from >= len(a.relayURLs) || to < 0 || to >= len(a.relayURLs) || from == to {
		return
	}
	values := a.relayURLListValues()
	value := values[from]
	values = append(values[:from], values[from+1:]...)
	if to >= len(values) {
		values = append(values, value)
	} else {
		values = append(values[:to], append([]string{value}, values[to:]...)...)
	}
	a.setRelayURLList(values, to)
}

func (a *clientApp) relayListMouseDown(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton {
		return
	}
	a.relayDragIndex = a.relayListIndexAt(x, y)
	a.relayDragStartY = y
	a.relayDragging = false
	if a.relayDragIndex >= 0 {
		_ = a.relayList.SetCurrentIndex(a.relayDragIndex)
	}
}

func (a *clientApp) relayListMouseMove(_, y int, button walk.MouseButton) {
	if button&walk.LeftButton == 0 || a.relayDragIndex < 0 {
		return
	}
	if absInt(y-a.relayDragStartY) > 4 {
		a.relayDragging = true
	}
}

func (a *clientApp) relayListMouseUp(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton {
		return
	}
	from := a.relayDragIndex
	dragging := a.relayDragging
	a.relayDragIndex = -1
	a.relayDragging = false
	if !dragging || from < 0 {
		return
	}
	to := a.relayListIndexAt(x, y)
	if to < 0 {
		if y < 0 {
			to = 0
		} else {
			to = len(a.relayURLs) - 1
		}
	}
	a.moveRelayURLTo(from, to)
}

func (a *clientApp) relayListIndexAt(x, y int) int {
	if a.relayList == nil || len(a.relayURLs) == 0 {
		return -1
	}
	lParam := uintptr(uint32(uint16(x)) | uint32(uint16(y))<<16)
	result := uint32(a.relayList.SendMessage(win.LB_ITEMFROMPOINT, 0, lParam))
	if win.HIWORD(result) != 0 {
		return -1
	}
	index := int(win.LOWORD(result))
	if index < 0 || index >= len(a.relayURLs) {
		return -1
	}
	return index
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (a *clientApp) setupNotifyIcon() error {
	ni, err := walk.NewNotifyIcon(a.mw)
	if err != nil {
		return err
	}
	a.ni = ni
	if err := ni.SetIcon(appIcon()); err != nil {
		return err
	}
	if err := ni.SetToolTip("DeskFerry Home"); err != nil {
		return err
	}
	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showWindow()
		}
	})

	a.trayOpen = trayAction("Open Control Panel", a.showWindow)
	a.trayConnect = trayAction("Connect", a.connectFromUI)
	a.trayStop = trayAction("Stop Tunnel", a.stopTunnel)
	a.trayRDP = trayAction("Open Remote Desktop", a.openRemoteDesktop)
	quit := trayAction("Quit", func() {
		a.exiting = true
		_ = a.mw.Close()
	})

	actions := ni.ContextMenu().Actions()
	for _, action := range []*walk.Action{
		a.trayOpen,
		walk.NewSeparatorAction(),
		a.trayConnect,
		a.trayStop,
		a.trayRDP,
		walk.NewSeparatorAction(),
		quit,
	} {
		if err := actions.Add(action); err != nil {
			return err
		}
	}
	return ni.SetVisible(true)
}

func trayAction(text string, action func()) *walk.Action {
	item := walk.NewAction()
	_ = item.SetText(text)
	item.Triggered().Attach(action)
	return item
}

func appIcon() *walk.Icon {
	icon, err := walk.NewIconFromResourceId(appIconResourceID)
	if err == nil {
		return icon
	}
	return walk.IconApplication()
}

func (a *clientApp) showWindow() {
	if a.mw == nil {
		return
	}
	a.mw.Show()
	_ = a.mw.SetFocus()
}

func (a *clientApp) connectFromUI() {
	if a.isTunnelRunning() {
		a.stopTunnel()
		return
	}
	if err := a.saveFromUI(false); err != nil {
		a.showError(err)
		return
	}
	if err := a.startTunnel(true); err != nil {
		a.showError(err)
		a.appendLog("Connect failed: %v", err)
		return
	}
}

func (a *clientApp) saveFromUI(showMessage bool) error {
	cfg, err := a.configFromUI()
	if err != nil {
		if showMessage {
			a.showError(err)
		}
		return err
	}
	wasRunning := a.isTunnelRunning()
	if wasRunning {
		a.stopTunnel()
	}
	a.setConfig(cfg)
	if err := saveSettingsConfig(cfg); err != nil {
		if showMessage {
			a.showError(err)
		}
		return err
	}
	a.restartHomePresence()
	a.refreshRelayStatusAsync()
	if wasRunning {
		if err := a.startTunnel(false); err != nil {
			if showMessage {
				a.showError(err)
			}
			return err
		}
	}
	if showMessage {
		a.appendLog("Saved settings.")
	}
	return nil
}

func (a *clientApp) configFromUI() (config, error) {
	relayURLs := a.relayURLListValues()
	a.commitDestinationProfile()
	cfg := config{
		RelayAddrs:      relayURLs,
		ListenAddr:      strings.TrimSpace(a.listenAddr.Text()),
		WinRMListenAddr: strings.TrimSpace(a.winrmListen.Text()),
		Proxy:           strings.TrimSpace(a.proxy.Text()),
		RDPUser:         strings.TrimSpace(a.rdpUser.Text()),
		Destinations:    cloneDestinations(a.destinations),
	}
	if a.selectedDestination >= 0 && a.selectedDestination < len(a.destinations) {
		cfg.SelectedDestination = a.destinations[a.selectedDestination].Name
	}
	if len(relayURLs) > 0 {
		cfg.RelayAddr = relayURLs[0]
	}
	cfg.applyDefaults()
	if normalized, err := normalizeRelayURLs(cfg.RelayAddr, cfg.RelayAddrs); err != nil {
		return config{}, err
	} else {
		cfg.setRelayAddresses(normalized)
	}
	if a.selectedDestination >= 0 && a.selectedDestination < len(cfg.Destinations) {
		cfg.Destinations[a.selectedDestination].WindowsUser = cfg.RDPUser
		proof := cfg.Destinations[a.selectedDestination].RoomProof
		if a.clearRoomPassword.Checked() {
			proof = ""
		} else if password := a.roomPass.Text(); password != "" {
			proof = tunnel.RoomPasswordProof(cfg.primaryRelayAddress(), "", password)
		}
		cfg.Destinations[a.selectedDestination].RoomProof = proof
		cfg.RoomProof = proof
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return config{}, fmt.Errorf("local RDP address must be host:port: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (a *clientApp) setConfig(cfg config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	a.onUI(func() {
		a.setDestinations(cfg.Destinations, cfg.SelectedDestination)
		a.setRelayURLList(cfg.relayAddresses(), 0)
		_ = a.listenAddr.SetText(cfg.ListenAddr)
		_ = a.proxy.SetText(cfg.Proxy)
		_ = a.rdpUser.SetText(cfg.RDPUser)
		_ = a.winrmListen.SetText(cfg.WinRMListenAddr)
		_ = a.roomPass.SetText("")
		a.clearRoomPassword.SetChecked(false)
	})
}

func (a *clientApp) currentConfig() config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *clientApp) startTunnel(openRDP bool) error {
	cfg := a.currentConfig()
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := listenLocalRDP(cfg)
	if err != nil {
		cancel()
		return localListenError(cfg.ListenAddr, err)
	}
	var winrmListener net.Listener
	if cfg.roomProof() != "" {
		winrmListener, err = net.Listen("tcp", cfg.WinRMListenAddr)
		if err != nil {
			cancel()
			_ = listener.Close()
			return fmt.Errorf("listen for local WinRM on %s: %w", cfg.WinRMListenAddr, err)
		}
	}

	a.mu.Lock()
	if a.listener != nil {
		a.mu.Unlock()
		cancel()
		_ = listener.Close()
		if winrmListener != nil {
			_ = winrmListener.Close()
		}
		if openRDP {
			if err := launchMSTSC(cfg); err != nil {
				a.appendLog("Could not open Remote Desktop: %v", err)
			}
		}
		return nil
	}
	a.cancel = cancel
	a.listener = listener
	a.winrmListener = winrmListener
	a.activeLocal = 0
	a.activeWinRM = 0
	a.mu.Unlock()

	a.appendLog("Local RDP listener started on %s.", listener.Addr())
	if winrmListener != nil {
		a.appendLog("Local WinRM listener started on %s.", winrmListener.Addr())
	}
	a.refreshLocalState()
	go func() {
		err := serveListener(ctx, cfg, listener, a.localConnStarted, a.localConnDone, a.appendLog)
		if err != nil && ctx.Err() == nil {
			a.appendLog("Listener stopped: %v", err)
		}
		a.mu.Lock()
		if a.listener == listener {
			a.listener = nil
			a.cancel = nil
			a.activeLocal = 0
		}
		a.mu.Unlock()
		a.refreshLocalState()
	}()
	if winrmListener != nil {
		go func() {
			err := serveWinRMListener(ctx, cfg, winrmListener, a.appendLog)
			if err != nil && ctx.Err() == nil {
				a.appendLog("WinRM listener stopped: %v", err)
			}
		}()
	}

	if openRDP {
		if err := launchMSTSC(cfg); err != nil {
			a.appendLog("Could not open Remote Desktop: %v", err)
		}
	}
	return nil
}

func (a *clientApp) stopTunnel() {
	a.mu.Lock()
	cancel := a.cancel
	listener := a.listener
	winrmListener := a.winrmListener
	a.cancel = nil
	a.listener = nil
	a.winrmListener = nil
	a.activeLocal = 0
	a.activeWinRM = 0
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
		a.appendLog("Local RDP listener stopped.")
	}
	if winrmListener != nil {
		_ = winrmListener.Close()
		a.appendLog("Local WinRM listener stopped.")
	}
	a.refreshLocalState()
}

func (a *clientApp) isTunnelRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listener != nil
}

func (a *clientApp) localConnStarted(remote string) {
	a.mu.Lock()
	a.activeLocal++
	a.mu.Unlock()
	a.appendLog("RDP connection from %s.", remote)
	a.refreshLocalState()
}

func (a *clientApp) localConnDone(remote string) {
	a.mu.Lock()
	if a.activeLocal > 0 {
		a.activeLocal--
	}
	a.mu.Unlock()
	a.appendLog("RDP connection closed from %s.", remote)
	a.refreshLocalState()
}

func (a *clientApp) refreshLocalState() {
	a.mu.Lock()
	running := a.listener != nil
	active := a.activeLocal
	cfg := a.cfg
	a.mu.Unlock()

	a.onUI(func() {
		if a.destinationList != nil {
			a.destinationList.SetEnabled(!running)
		}
		if a.destinationAdd != nil {
			a.destinationAdd.SetEnabled(!running)
		}
		if a.destinationRename != nil {
			a.destinationRename.SetEnabled(!running)
		}
		if a.destinationDelete != nil {
			a.destinationDelete.SetEnabled(!running && len(a.destinations) > 1)
		}
		if running {
			_ = a.tunnelStatus.SetText("Running")
			_ = a.connectButton.SetText("Stop Tunnel")
			_ = a.rdpStatus.SetText(fmt.Sprintf("%s (%d active)", cfg.ListenAddr, active))
			_ = a.trayConnect.SetEnabled(false)
			_ = a.trayStop.SetEnabled(true)
			_ = a.trayRDP.SetEnabled(true)
			_ = a.ni.SetToolTip("DeskFerry Home: running")
		} else {
			_ = a.tunnelStatus.SetText("Stopped")
			_ = a.connectButton.SetText("Connect")
			_ = a.rdpStatus.SetText(cfg.ListenAddr)
			_ = a.trayConnect.SetEnabled(true)
			_ = a.trayStop.SetEnabled(false)
			_ = a.trayRDP.SetEnabled(false)
			_ = a.ni.SetToolTip("DeskFerry Home: stopped")
		}
	})
}

func (a *clientApp) openRemoteDesktop() {
	cfg := a.currentConfig()
	if !a.isTunnelRunning() {
		if err := a.saveFromUI(false); err != nil {
			a.showError(err)
			return
		}
		if err := a.startTunnel(false); err != nil {
			a.showError(err)
			return
		}
		cfg = a.currentConfig()
	}
	if err := launchMSTSC(cfg); err != nil {
		a.showError(err)
	}
}

func (a *clientApp) copyRDPAddress() {
	cfg := a.currentConfig()
	if err := walk.Clipboard().SetText(mstscTarget(cfg.ListenAddr)); err != nil {
		a.showError(err)
		return
	}
	a.appendLog("Copied RDP address: %s", mstscTarget(cfg.ListenAddr))
}

func (a *clientApp) executeWinRM() {
	if !a.isTunnelRunning() {
		if err := a.saveFromUI(false); err != nil {
			a.showError(err)
			return
		}
	}
	cfg := a.currentConfig()
	if cfg.roomProof() == "" {
		a.showError(errors.New("a room password is required before WinRM can be used"))
		return
	}
	user := strings.TrimSpace(a.rdpUser.Text())
	password := a.rdpPass.Text()
	command := strings.TrimSpace(a.winrmCommand.Text())
	if password == "" {
		savedUser, savedPassword, savedErr := readWindowsCredential(cfg)
		if savedErr != nil {
			a.showError(errors.New("enter a Windows password or save a Windows login for this destination"))
			return
		}
		user, password = savedUser, savedPassword
		_ = a.rdpUser.SetText(user)
	}
	if user == "" || password == "" || command == "" {
		a.showError(errors.New("Windows username, password, and PowerShell command are required"))
		return
	}
	_, port, err := net.SplitHostPort(cfg.WinRMListenAddr)
	if err != nil {
		a.showError(err)
		return
	}
	if !a.isTunnelRunning() {
		if err := a.startTunnel(false); err != nil {
			a.showError(err)
			return
		}
	}
	payload, err := json.Marshal(map[string]string{"user": user, "password": password, "command": command, "port": port})
	if err != nil {
		a.showError(err)
		return
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	_ = a.rdpPass.SetText("")
	a.executeWinRMButton.SetEnabled(false)
	_ = a.winrmOutput.SetText("Running...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		script := `$ErrorActionPreference = 'Stop'
$payload = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + encoded + `')) | ConvertFrom-Json
$secure = ConvertTo-SecureString ([string]$payload.password) -AsPlainText -Force
$credential = [Management.Automation.PSCredential]::new([string]$payload.user, $secure)
Invoke-Command -ComputerName localhost -Port ([int]$payload.port) -Authentication Negotiate -Credential $credential -ScriptBlock ([ScriptBlock]::Create([string]$payload.command)) | Out-String -Width 240
`
		cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")
		cmd.Stdin = strings.NewReader(script)
		output, runErr := cmd.CombinedOutput()
		text := strings.TrimSpace(string(output))
		if runErr != nil {
			if text != "" {
				text += "\r\n\r\n"
			}
			text += "WinRM command failed: " + runErr.Error()
		}
		if text == "" {
			text = "Command completed without output."
		}
		a.onUI(func() {
			_ = a.winrmOutput.SetText(text)
			a.executeWinRMButton.SetEnabled(true)
		})
		a.appendLog("WinRM command completed success=%t.", runErr == nil)
	}()
}

func (a *clientApp) saveWindowsCredentials() {
	if err := a.saveFromUI(false); err != nil {
		a.showError(err)
		return
	}
	cfg := a.currentConfig()
	user := strings.TrimSpace(a.rdpUser.Text())
	pass := a.rdpPass.Text()
	if user == "" {
		a.showError(errors.New("Windows username is required"))
		return
	}
	if pass == "" {
		a.showError(errors.New("Windows password is required"))
		return
	}
	if err := saveWindowsCredential(cfg, user, pass); err != nil {
		a.showError(err)
		return
	}
	if _, err := writeMSTSCRDPFile(cfg); err != nil {
		a.appendLog("Saved Windows login, but could not update the Remote Desktop profile: %v", err)
	}
	_ = a.rdpPass.SetText("")
	a.appendLog("Saved shared RDP and WinRM login in Windows Credential Manager for destination %s.", cfg.SelectedDestination)
}

func (a *clientApp) forgetWindowsCredentials() {
	cfg, err := a.configFromUI()
	if err != nil {
		a.showError(err)
		return
	}
	if err := deleteWindowsCredential(cfg); err != nil {
		a.showError(err)
		return
	}
	a.appendLog("Removed the shared RDP and WinRM login for destination %s.", cfg.SelectedDestination)
}

func (a *clientApp) openDashboard() {
	cfg := a.currentConfig()
	if err := shellOpen(dashboardURL(cfg.primaryRelayAddress())); err != nil {
		a.showError(err)
	}
}

func (a *clientApp) restartHomePresence() {
	a.mu.Lock()
	if a.statusCancel != nil {
		a.statusCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.statusCancel = cancel
	cfg := a.cfg
	a.mu.Unlock()

	go a.homePresenceLoop(ctx, cfg)
}

func (a *clientApp) homePresenceLoop(ctx context.Context, cfg config) {
	for {
		a.setHomePresence("Connecting")
		conn, relayAddr, err := dialWebSocketFallback(ctx, cfg, tunnel.RoleHomeAgent)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.setHomePresence("Offline")
			a.appendLog("Home status connection failed: %v", err)
		} else {
			a.setHomePresence("Online")
			a.appendLog("Home status connected to %s.", relayAddr)
			_, _, err = conn.Read(ctx)
			tunnel.CloseWebSocket(conn)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				a.setHomePresence("Reconnecting")
				a.appendLog("Home status disconnected: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *clientApp) setHomePresence(text string) {
	a.onUI(func() {
		_ = a.homeStatus.SetText(text)
	})
}

func (a *clientApp) refreshRelayStatusAsync() {
	cfg := a.currentConfig()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		summary, err := queryRelaySummary(ctx, cfg)
		a.onUI(func() {
			if err != nil {
				_ = a.workStatus.SetText("Check relay")
				_ = a.details.SetText("Relay status: " + err.Error())
				return
			}
			if summary.WorkOnline {
				_ = a.workStatus.SetText("Connected")
			} else {
				_ = a.workStatus.SetText("Waiting")
			}
			if summary.HomeOnline {
				_ = a.homeStatus.SetText("Online")
			}
			_ = a.details.SetText(formatRelayDetails(summary, cfg))
		})
	}()
}

func (a *clientApp) startStatusPoller() {
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if a.mw == nil || a.exiting {
				return
			}
			a.refreshRelayStatusAsync()
		}
	}()
}

func (a *clientApp) appendLog(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Print(line)
	if a.logView == nil || a.mw == nil {
		return
	}
	a.onUI(func() {
		a.logView.AppendText(time.Now().Format("15:04:05") + "  " + line + "\r\n")
	})
}

func (a *clientApp) showError(err error) {
	if err == nil {
		return
	}
	walk.MsgBox(a.mw, appTitle(), err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func (a *clientApp) onUI(f func()) {
	if a.mw == nil {
		return
	}
	a.mw.Synchronize(f)
}

func (a *clientApp) shutdown() {
	a.exiting = true
	a.stopTunnel()
	a.mu.Lock()
	cancel := a.statusCancel
	a.statusCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.ni != nil {
		_ = a.ni.SetVisible(false)
		_ = a.ni.Dispose()
	}
}

func loadConfig(relayURL, listenAddr, proxyFlag string) (config, error) {
	cfg := defaultConfig()
	if saved, ok, err := loadSavedConfig(); err != nil {
		return config{}, err
	} else if ok {
		cfg.merge(saved)
	}
	if strings.TrimSpace(relayURL) != "" {
		cfg.RelayAddr = strings.TrimSpace(relayURL)
		cfg.RelayAddrs = nil
	}
	if strings.TrimSpace(listenAddr) != "" {
		cfg.ListenAddr = strings.TrimSpace(listenAddr)
	}
	if strings.TrimSpace(proxyFlag) != "" {
		cfg.Proxy = strings.TrimSpace(proxyFlag)
	}
	cfg.applyDefaults()
	normalized, err := normalizeRelayURLs(cfg.RelayAddr, cfg.RelayAddrs)
	if err != nil {
		return config{}, err
	}
	cfg.setRelayAddresses(normalized)
	if err := cfg.ensureDestinations(); err != nil {
		return config{}, err
	}
	return cfg, cfg.validate()
}

func defaultConfig() config {
	return config{
		ListenAddr:          defaultListenAddr,
		WinRMListenAddr:     defaultWinRMListenAddr,
		RelayAddr:           defaultRelayURL,
		RelayAddrs:          []string{defaultRelayURL},
		Proxy:               "env",
		Destinations:        []destinationProfile{{Name: "Work", RelayAddrs: []string{defaultRelayURL}}},
		SelectedDestination: "Work",
	}
}

func loadSavedConfig() (config, bool, error) {
	path, err := settingsPath()
	if err != nil {
		return config{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{}, false, nil
	}
	if err != nil {
		return config{}, false, fmt.Errorf("read saved settings: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, false, fmt.Errorf("decode saved settings: %w", err)
	}
	return cfg, true, nil
}

func saveSettingsConfig(cfg config) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(config{
		ListenAddr:          cfg.ListenAddr,
		RelayAddr:           cfg.primaryRelayAddress(),
		RelayAddrs:          cfg.relayAddresses(),
		Proxy:               cfg.Proxy,
		RDPUser:             cfg.RDPUser,
		WinRMListenAddr:     cfg.WinRMListenAddr,
		RoomProof:           cfg.roomProof(),
		Destinations:        cloneDestinations(cfg.Destinations),
		SelectedDestination: cfg.SelectedDestination,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func settingsPath() (string, error) {
	base := os.Getenv("APPDATA")
	if base == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = dir
	}
	return filepath.Join(base, "DeskFerry", "home-client.json"), nil
}

func (c *config) merge(other config) {
	if strings.TrimSpace(other.ListenAddr) != "" {
		c.ListenAddr = other.ListenAddr
	}
	if strings.TrimSpace(other.RelayAddr) != "" {
		c.RelayAddr = other.RelayAddr
	}
	if len(other.RelayAddrs) > 0 {
		c.RelayAddrs = append([]string(nil), other.RelayAddrs...)
	}
	if strings.TrimSpace(other.Proxy) != "" {
		c.Proxy = other.Proxy
	}
	if strings.TrimSpace(other.RDPUser) != "" {
		c.RDPUser = other.RDPUser
	}
	if strings.TrimSpace(other.WinRMListenAddr) != "" {
		c.WinRMListenAddr = other.WinRMListenAddr
	}
	if strings.TrimSpace(other.WinRMUser) != "" {
		c.WinRMUser = other.WinRMUser
	}
	if strings.TrimSpace(other.RoomProof) != "" {
		c.RoomProof = other.RoomProof
	}
	if len(other.Destinations) > 0 {
		c.Destinations = cloneDestinations(other.Destinations)
	}
	if strings.TrimSpace(other.SelectedDestination) != "" {
		c.SelectedDestination = other.SelectedDestination
	}
}

func (c *config) ensureDestinations() error {
	if c.RDPUser == "" && c.WinRMUser != "" {
		c.RDPUser = c.WinRMUser
	}
	current := c.relayAddresses()
	if len(c.Destinations) == 0 {
		c.Destinations = []destinationProfile{{Name: "Work", RelayAddrs: current, RoomProof: c.RoomProof, WindowsUser: c.RDPUser}}
		c.SelectedDestination = "Work"
		return nil
	}
	normalized := make([]destinationProfile, 0, len(c.Destinations))
	seen := map[string]bool{}
	for _, destination := range c.Destinations {
		name := strings.TrimSpace(destination.Name)
		if name == "" {
			name = "Work"
		}
		base := name
		for suffix := 2; seen[strings.ToLower(name)]; suffix++ {
			name = fmt.Sprintf("%s %d", base, suffix)
		}
		seen[strings.ToLower(name)] = true
		relays, err := normalizeRelayURLs("", destination.RelayAddrs)
		if err != nil {
			return fmt.Errorf("destination %q: %w", name, err)
		}
		room := ""
		for _, relayAddr := range relays {
			currentRoom := strings.ToLower(tunnel.RelayRoomToken(relayAddr, ""))
			if room == "" {
				room = currentRoom
			} else if room != currentRoom {
				return fmt.Errorf("destination %q relay URLs must use the same room name", name)
			}
		}
		normalized = append(normalized, destinationProfile{Name: name, RelayAddrs: relays, RoomProof: destination.RoomProof, WindowsUser: destination.WindowsUser})
	}
	selected := 0
	for i, destination := range normalized {
		if strings.EqualFold(destination.Name, c.SelectedDestination) {
			selected = i
			break
		}
	}
	normalized[selected].RelayAddrs = append([]string(nil), current...)
	if c.RoomProof != "" && normalized[selected].RoomProof == "" {
		normalized[selected].RoomProof = c.RoomProof
	}
	if c.RDPUser != "" && normalized[selected].WindowsUser == "" {
		normalized[selected].WindowsUser = c.RDPUser
	}
	c.Destinations = normalized
	c.SelectedDestination = normalized[selected].Name
	c.RDPUser = normalized[selected].WindowsUser
	return nil
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
	if c.WinRMListenAddr == "" {
		c.WinRMListenAddr = defaultWinRMListenAddr
	}
}

func (c config) validate() error {
	relayAddrs := c.relayAddresses()
	if len(relayAddrs) == 0 {
		return fmt.Errorf("relay URL is required")
	}
	for _, relayAddr := range relayAddrs {
		if !tunnel.IsWebSocketRelay(relayAddr) {
			return fmt.Errorf("relay URL %q must start with https:// or http://", relayAddr)
		}
		if _, err := url.ParseRequestURI(relayAddr); err != nil {
			return fmt.Errorf("relay URL %q is invalid: %w", relayAddr, err)
		}
	}
	if _, _, err := net.SplitHostPort(c.WinRMListenAddr); err != nil {
		return fmt.Errorf("local WinRM address must be host:port: %w", err)
	}
	return nil
}

func run(ctx context.Context, cfg config, openMSTSC bool) error {
	listener, err := listenLocalRDP(cfg)
	if err != nil {
		return localListenError(cfg.ListenAddr, err)
	}
	defer listener.Close()
	log.Printf("client listening on %s; mstsc should target this address", listener.Addr())
	if openMSTSC {
		if err := launchMSTSC(cfg); err != nil {
			log.Printf("open Remote Desktop: %v", err)
		}
	}
	return serveListener(ctx, cfg, listener, func(string) {}, func(string) {}, func(format string, args ...any) {
		log.Printf(format, args...)
	})
}

func serveListener(ctx context.Context, cfg config, listener net.Listener, started func(string), done func(string), logf func(string, ...any)) error {
	var localSessions tunnel.LatestConnGroup
	defer localSessions.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		remote := conn.RemoteAddr().String()
		started(remote)
		connCtx, release, replaced := localSessions.Begin(ctx, conn)
		if replaced {
			logf("RDP connection remote=%s superseded previous local retry", remote)
		}
		go func() {
			defer release()
			handleLocalConn(connCtx, cfg, conn, remote, done, logf)
		}()
	}
}

func handleLocalConn(ctx context.Context, cfg config, localConn net.Conn, remote string, done func(string), logf func(string, ...any)) {
	defer done(remote)
	started := time.Now()
	relayConn, relayAddr, err := dialRelay(ctx, cfg)
	if err != nil {
		logf("RDP session remote=%s relay dial failed after %s: %v", remote, time.Since(started).Round(time.Millisecond), err)
		_ = localConn.Close()
		return
	}
	logf("RDP session remote=%s connected relay=%s local=%s relay_stream=%s dial_duration=%s", remote, relayAddr, localConn.LocalAddr(), relayConn.RemoteAddr(), time.Since(started).Round(time.Millisecond))
	result := tunnel.PipeWithResult(localConn, relayConn)
	logf("RDP session remote=%s relay=%s ended duration=%s end_initiator=%s local_to_relay_bytes=%d local_to_relay_error=%v local_to_relay_half_close_error=%v relay_to_local_bytes=%d relay_to_local_error=%v relay_to_local_half_close_error=%v local_close_error=%v relay_close_error=%v", remote, relayAddr, result.Duration.Round(time.Millisecond), result.EndInitiator("local_rdp", "relay"), result.AToB.Bytes, result.AToB.CopyErr, result.AToB.CloseWriteErr, result.BToA.Bytes, result.BToA.CopyErr, result.BToA.CloseWriteErr, result.ACloseErr, result.BCloseErr)
}

func serveWinRMListener(ctx context.Context, cfg config, listener net.Listener, logf func(string, ...any)) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleLocalServiceConn(ctx, cfg, conn, tunnel.ServiceWinRM, logf)
	}
}

func handleLocalServiceConn(ctx context.Context, cfg config, localConn net.Conn, service string, logf func(string, ...any)) {
	remote := localConn.RemoteAddr().String()
	started := time.Now()
	relayConn, relayAddr, err := dialRelayService(ctx, cfg, service)
	if err != nil {
		logf("%s session remote=%s relay dial failed after %s: %v", strings.ToUpper(service), remote, time.Since(started).Round(time.Millisecond), err)
		_ = localConn.Close()
		return
	}
	logf("%s session remote=%s connected relay=%s", strings.ToUpper(service), remote, relayAddr)
	result := tunnel.PipeWithResult(localConn, relayConn)
	logf("%s session remote=%s relay=%s ended duration=%s local_to_relay_bytes=%d relay_to_local_bytes=%d local_error=%v relay_error=%v", strings.ToUpper(service), remote, relayAddr, result.Duration.Round(time.Millisecond), result.AToB.Bytes, result.BToA.Bytes, result.AToB.CopyErr, result.BToA.CopyErr)
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
			attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			headers := http.Header{}
			headers.Set(tunnel.HeaderResumable, "1")
			if proof := cfg.roomProof(); proof != "" {
				headers.Set(tunnel.HeaderRoomProof, proof)
			}
			tunnel.AddServiceHeader(headers, service)
			ws, err := tunnel.DialWebSocketWithHeaders(attemptCtx, relayAddr, cfg.Proxy, tunnel.RoleClient, "", headers)
			sessionID := ""
			if err == nil {
				sessionID, err = tunnel.AwaitWebSocketStartSession(attemptCtx, ws)
			}
			if err == nil {
				cancel()
				log.Printf("relay client paired relay=%s via=%s duration=%s", relayAddr, tunnel.ProxySpecForLog(cfg.Proxy), time.Since(attemptStarted).Round(time.Millisecond))
				if sessionID != "" {
					return tunnel.NewResumableWebSocketConn(ctx, ws, tunnel.ResumableWebSocketOptions{RelayAddr: relayAddr, Proxy: cfg.Proxy, SessionID: sessionID, Side: "client", RoomProof: cfg.roomProof(), Service: service}), relayAddr, nil
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
		"Local RDP address: " + mstscTarget(cfg.ListenAddr),
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
	if !summary.CheckedAt.IsZero() {
		lines = append(lines, "Checked: "+summary.CheckedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return strings.Join(lines, "\r\n")
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
		return "", fmt.Errorf("relay URL must include a host")
	}
	switch parsed.Scheme {
	case "https", "http":
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	default:
		return "", fmt.Errorf("relay URL must start with https:// or http://")
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
	return strings.Join(uniqueRelayURLs(values), "\n")
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

func (c config) fallbackRelayAddresses() []string {
	relays := c.relayAddresses()
	if len(relays) <= 1 {
		return nil
	}
	return relays[1:]
}

func (c config) fallbackRelayText() string {
	return joinRelayURLs(c.fallbackRelayAddresses())
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

func (c config) roomProof() string {
	for _, destination := range c.Destinations {
		if strings.EqualFold(destination.Name, c.SelectedDestination) {
			return destination.RoomProof
		}
	}
	return c.RoomProof
}

func dialWebSocketFallback(ctx context.Context, cfg config, role string) (*websocket.Conn, string, error) {
	var errs []string
	for _, relayAddr := range cfg.relayAddresses() {
		headers := http.Header{}
		if proof := cfg.roomProof(); proof != "" {
			headers.Set(tunnel.HeaderRoomProof, proof)
		}
		conn, err := tunnel.DialWebSocketWithHeaders(ctx, relayAddr, cfg.Proxy, role, "", headers)
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
		return "", "", fmt.Errorf("relay URL must include a host")
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

func dashboardURL(relayAddr string) string {
	normalized, err := normalizeRelayURL(relayAddr)
	if err != nil {
		return defaultRelayURL
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return normalized
	}
	if parsed.Scheme == "ws" {
		parsed.Scheme = "http"
	} else if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	}
	return parsed.String()
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

func listenLocalRDP(cfg config) (net.Listener, error) {
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err == nil {
		return listener, nil
	}
	return nil, err
}

func localListenError(listenAddr string, err error) error {
	return fmt.Errorf("listen %s: %w", listenAddr, err)
}

func launchMSTSC(cfg config) error {
	if err := activateRDPProfileCredential(cfg); err != nil {
		log.Printf("activate saved Windows login for RDP: %v", err)
	}
	if profile, err := writeMSTSCRDPFile(cfg); err == nil {
		return exec.Command("mstsc.exe", profile).Start()
	}
	return exec.Command("mstsc.exe", "/v:"+mstscTarget(cfg.ListenAddr)).Start()
}

func writeMSTSCRDPFile(cfg config) (string, error) {
	path, err := mstscProfilePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("create RDP profile directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(mstscProfileContent(cfg)), 0600); err != nil {
		return "", fmt.Errorf("write RDP profile: %w", err)
	}
	return path, nil
}

func mstscProfilePath() (string, error) {
	settings, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(settings), "home-client.rdp"), nil
}

func mstscProfileContent(cfg config) string {
	target := sanitizeRDPValue(mstscTarget(cfg.ListenAddr))
	lines := []string{
		"screen mode id:i:2",
		"use multimon:i:0",
		"session bpp:i:32",
		"full address:s:" + target,
		"prompt for credentials:i:0",
		"promptcredentialonce:i:1",
		"authentication level:i:2",
		"enablecredsspsupport:i:1",
		"negotiate security layer:i:1",
		"redirectclipboard:i:1",
		"redirectprinters:i:0",
	}
	if user := strings.TrimSpace(cfg.RDPUser); user != "" {
		lines = append(lines, "username:s:"+sanitizeRDPValue(user))
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func sanitizeRDPValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func mstscTarget(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = "127.0.0.1"
		port = "3390"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return net.JoinHostPort(host, port)
}

func saveRDPCredentialTargets(listenAddr, user, pass string) error {
	for _, target := range rdpCredentialTargets(listenAddr) {
		out, err := exec.Command("cmdkey.exe", "/generic:"+target, "/user:"+user, "/pass:"+pass).CombinedOutput()
		if err != nil {
			return fmt.Errorf("save RDP credentials with cmdkey for %s: %w: %s", target, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func profileCredentialTarget(cfg config) string {
	room := strings.ToLower(strings.TrimSpace(tunnel.RelayRoomToken(cfg.primaryRelayAddress(), "")))
	if room == "" {
		room = "default"
	}
	return "DeskFerry/room/" + room
}

func saveWindowsCredential(cfg config, user, pass string) error {
	if err := wincred.Write(profileCredentialTarget(cfg), wincred.TypeGeneric, user, pass); err != nil {
		return err
	}
	if err := saveRDPCredentialTargets(cfg.ListenAddr, user, pass); err != nil {
		return err
	}
	return nil
}

func readWindowsCredential(cfg config) (user, pass string, err error) {
	user, pass, err = wincred.Read(profileCredentialTarget(cfg), wincred.TypeGeneric)
	if err == nil && user != "" && pass != "" {
		return user, pass, nil
	}
	for _, target := range rdpCredentialTargets(cfg.ListenAddr) {
		for _, credentialType := range []uint32{wincred.TypeDomainPassword, wincred.TypeGeneric} {
			user, pass, err = wincred.Read(target, credentialType)
			if err == nil && user != "" && pass != "" {
				return user, pass, nil
			}
		}
	}
	return "", "", errors.New("no saved Windows login for this destination")
}

func activateRDPProfileCredential(cfg config) error {
	user, pass, err := wincred.Read(profileCredentialTarget(cfg), wincred.TypeGeneric)
	if err != nil {
		return nil
	}
	if user == "" || pass == "" {
		return nil
	}
	return saveRDPCredentialTargets(cfg.ListenAddr, user, pass)
}

func deleteWindowsCredential(cfg config) error {
	profileErr := wincred.Delete(profileCredentialTarget(cfg), wincred.TypeGeneric)
	rdpErr := deleteRDPCredentialTargets(cfg.ListenAddr)
	if profileErr != nil {
		return profileErr
	}
	return rdpErr
}

func deleteRDPCredentialTargets(listenAddr string) error {
	var failures []string
	for _, target := range rdpCredentialTargets(listenAddr) {
		out, err := exec.Command("cmdkey.exe", "/delete:"+target).CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", target, strings.TrimSpace(string(out))))
		}
	}
	if len(failures) == len(rdpCredentialTargets(listenAddr)) {
		return fmt.Errorf("cmdkey did not remove any matching RDP credentials: %s", strings.Join(failures, "; "))
	}
	return nil
}

func rdpCredentialTargets(listenAddr string) []string {
	target := mstscTarget(listenAddr)
	targets := []string{"TERMSRV/" + target}
	if host, port, err := net.SplitHostPort(target); err == nil {
		host = strings.Trim(host, "[]")
		if host != "" && host != target {
			targets = append(targets, "TERMSRV/"+host)
		}
		if isLoopbackHost(host) {
			targets = append(targets, "TERMSRV/localhost")
			if port != "" {
				targets = append(targets, "TERMSRV/"+net.JoinHostPort("localhost", port))
			}
		}
	}
	return uniqueStrings(targets)
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func shellOpen(path string) error {
	return shellExecute("open", path, "", "")
}

func shellExecute(verb, file, params, dir string) error {
	verbPtr, _ := windows.UTF16PtrFromString(verb)
	filePtr, _ := windows.UTF16PtrFromString(file)
	paramsPtr, _ := windows.UTF16PtrFromString(params)
	dirPtr, _ := windows.UTF16PtrFromString(dir)
	return windows.ShellExecute(0, verbPtr, filePtr, paramsPtr, dirPtr, windows.SW_SHOWNORMAL)
}

func windowsMessageBox(title, text string, style uint32) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	_, _ = windows.MessageBox(0, textPtr, titlePtr, style)
}
