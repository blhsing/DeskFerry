//go:build windows

package windowssetup

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"deskferry/internal/homenetwork"
	"deskferry/internal/tunnel"
	"deskferry/internal/winsecret"
)

const (
	productName           = "DeskFerry"
	networkServiceName    = "DeskFerryHomeNetwork"
	networkServiceDisplay = "DeskFerry Home Network"
	defaultRelayURL       = "https://test-officialwebsite.azurewebsites.net/relay/workdesk"
	defaultAzureRelayBase = "https://test-officialwebsite.azurewebsites.net/relay"
	defaultOCIRelayBase   = "http://217.142.228.117/relay"
	defaultRoomName       = "workdesk"
	productVersion        = "0.12.0"
	setupInstallMutexName = `Global\DeskFerryWindowsComponentsInstall`
	hostsBeginMarker      = "# BEGIN DeskFerry Home managed alias"
	hostsEndMarker        = "# END DeskFerry Home managed alias"
)

var installedFiles = []string{
	"DeskFerry.exe",
	"tun2socks.exe",
	"wintun.dll",
	"LICENSE-Wintun.txt",
	"LICENSE-tun2socks.txt",
}

type setupOptions struct {
	InstallDir    string   `json:"install_dir"`
	SourceDir     string   `json:"source_dir"`
	Destination   string   `json:"destination,omitempty"`
	RelayAddrs    []string `json:"relay_addrs"`
	RelayBases    []string `json:"relay_bases,omitempty"`
	Room          string   `json:"room,omitempty"`
	Proxy         string   `json:"proxy"`
	RoomPassword  string   `json:"room_password"`
	RoomProof     string   `json:"room_proof,omitempty"`
	Alias         string   `json:"alias"`
	EnableNetwork bool     `json:"enable_network"`
}

type setupApp struct {
	mw              *walk.MainWindow
	installDir      *walk.LineEdit
	enableNetwork   *walk.CheckBox
	relayList       *walk.ListBox
	relayEdit       *walk.LineEdit
	relayAdd        *walk.PushButton
	relayUpdate     *walk.PushButton
	relayDelete     *walk.PushButton
	relayUp         *walk.PushButton
	relayDown       *walk.PushButton
	roomName        *walk.LineEdit
	roomPassword    *walk.LineEdit
	proxy           *walk.LineEdit
	alias           *walk.LineEdit
	uncPreview      *walk.Label
	status          *walk.Label
	installButton   *walk.PushButton
	uninstallButton *walk.PushButton
	relayURLs       []string
	relayDragIndex  int
	relayDragStartY int
	relayDragging   bool
	roomProof       string
	roomProofRoom   string
	destination     string
}

func Main() {
	if hasArg(os.Args[1:], "-cli-action") || hasArg(os.Args[1:], "-cli-help") {
		if err := runCLI(os.Args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	requestFile := flag.String("elevated-request", "", "DPAPI-protected component request")
	networkRequestFile := flag.String("elevated-network-request", "", "DPAPI-protected selected-profile network request")
	uninstall := flag.Bool("elevated-uninstall", false, "uninstall DeskFerry")
	smokeTest := flag.Bool("ui-smoke-test", false, "open and close the Windows Components UI")
	noDialog := flag.Bool("no-dialog", false, "write the result to the console instead of displaying a dialog")
	flag.Parse()
	if *requestFile != "" || *networkRequestFile != "" || *uninstall {
		if !isElevated() {
			if err := relaunchElevated(os.Args[1:]); err != nil {
				reportResult(*noDialog, "", err)
			}
			return
		}
		var message string
		var err error
		if *uninstall {
			message, err = uninstallProduct()
		} else if *networkRequestFile != "" {
			message, err = configureNetworkFromRequest(*networkRequestFile)
		} else {
			message, err = installFromRequest(*requestFile)
		}
		if err != nil {
			reportResult(*noDialog, "", err)
			os.Exit(1)
		}
		reportResult(*noDialog, message, nil)
		return
	}
	app := &setupApp{relayDragIndex: -1}
	if err := app.run(*smokeTest); err != nil {
		windowsMessageBox(productName+" Windows Components", err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}
}

func (a *setupApp) run(smokeTest bool) error {
	window := MainWindow{
		AssignTo: &a.mw,
		Title:    productName + " Windows Components",
		Size:     Size{Width: 840, Height: 620},
		MinSize:  Size{Width: 760, Height: 560},
		Layout:   VBox{Margins: Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 8},
		Visible:  !smokeTest,
		Children: []Widget{
			GroupBox{
				Title:  "Application",
				Layout: Grid{Columns: 3, Spacing: 6},
				Children: []Widget{
					Label{Text: "Install location"},
					LineEdit{AssignTo: &a.installDir, Text: defaultInstallDir()},
					PushButton{Text: "Browse...", OnClicked: a.browseInstallDir},
				},
			},
			GroupBox{
				Title:  "Work file access (optional)",
				Layout: Grid{Columns: 3, Spacing: 6},
				Children: []Widget{
					CheckBox{
						AssignTo:         &a.enableNetwork,
						Text:             `Enable \\deskferry-work\... file access with the DeskFerry virtual network adapter`,
						Checked:          true,
						ColumnSpan:       3,
						OnCheckedChanged: a.updateNetworkControls,
					},
					Label{Text: "Room name"},
					LineEdit{AssignTo: &a.roomName, Text: defaultRoomName, CueBanner: defaultRoomName, ColumnSpan: 2},
					Label{Text: "Relay service base URLs"},
					Composite{
						ColumnSpan: 2,
						Layout:     VBox{Spacing: 5},
						Children: []Widget{
							ListBox{
								AssignTo:              &a.relayList,
								Model:                 []string{defaultAzureRelayBase, defaultOCIRelayBase},
								MinSize:               Size{Height: 72},
								OnCurrentIndexChanged: a.relaySelectionChanged,
								OnMouseDown:           a.relayListMouseDown,
								OnMouseMove:           a.relayListMouseMove,
								OnMouseUp:             a.relayListMouseUp,
							},
							Composite{
								Layout: Grid{Columns: 6, Spacing: 5},
								Children: []Widget{
									LineEdit{AssignTo: &a.relayEdit, CueBanner: defaultAzureRelayBase, ColumnSpan: 6},
									PushButton{AssignTo: &a.relayAdd, Text: "Add", OnClicked: a.addRelayURL},
									PushButton{AssignTo: &a.relayUpdate, Text: "Update", OnClicked: a.updateRelayURL},
									PushButton{AssignTo: &a.relayDelete, Text: "Delete", OnClicked: a.deleteRelayURL},
									PushButton{AssignTo: &a.relayUp, Text: "Up", OnClicked: func() { a.moveRelayURL(-1) }},
									PushButton{AssignTo: &a.relayDown, Text: "Down", OnClicked: func() { a.moveRelayURL(1) }},
									Label{Text: "Drag rows to reorder."},
								},
							},
						},
					},
					Label{Text: "Room password"},
					LineEdit{AssignTo: &a.roomPassword, PasswordMode: true, CueBanner: "same password as the work agent", ColumnSpan: 2},
					Label{Text: "Proxy"},
					LineEdit{AssignTo: &a.proxy, CueBanner: "env, direct, or http(s)://host:port", ColumnSpan: 2},
					Label{Text: "Work computer alias"},
					LineEdit{AssignTo: &a.alias, Text: homenetwork.DefaultAlias, OnTextChanged: a.updateUNCPreview, ColumnSpan: 2},
					Label{Text: "UNC path"},
					Label{AssignTo: &a.uncPreview, Text: `\\deskferry-work\sharename`, ColumnSpan: 2},
				},
			},
			GroupBox{
				Title:  "Install",
				Layout: VBox{Spacing: 7},
				Children: []Widget{
					Label{AssignTo: &a.status, Text: "Checking installation..."},
					Composite{
						Layout: Flow{Spacing: 8},
						Children: []Widget{
							PushButton{AssignTo: &a.installButton, Text: "Install", MinSize: Size{Width: 130, Height: 34}, OnClicked: a.install},
							PushButton{Text: "Open DeskFerry", MinSize: Size{Width: 155, Height: 34}, OnClicked: a.openHome},
							PushButton{AssignTo: &a.uninstallButton, Text: "Remove", MinSize: Size{Width: 100, Height: 34}, OnClicked: a.uninstall},
							PushButton{Text: "Refresh", MinSize: Size{Width: 90, Height: 34}, OnClicked: a.refreshStatus},
							PushButton{Text: "Close", MinSize: Size{Width: 90, Height: 34}, OnClicked: func() { _ = a.mw.Close() }},
						},
					},
				},
			},
			Label{Text: "The adapter routes only the private DeskFerry work address on TCP port 445; other Internet and LAN traffic is unchanged."},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	initial := existingSetupOptions(filepath.Dir(mustExecutable()))
	_ = a.installDir.SetText(initial.InstallDir)
	a.enableNetwork.SetChecked(initial.EnableNetwork)
	_ = a.proxy.SetText(initial.Proxy)
	_ = a.alias.SetText(initial.Alias)
	a.roomProof = initial.RoomProof
	a.roomProofRoom = relayRoom(initial.RelayAddrs)
	a.destination = initial.Destination
	bases, room, _ := tunnel.SplitRelayRoomURLs(initial.RelayAddrs)
	if len(initial.RelayBases) > 0 {
		bases = initial.RelayBases
	}
	if initial.Room != "" {
		room = initial.Room
	}
	if len(bases) == 0 {
		bases = []string{defaultAzureRelayBase, defaultOCIRelayBase}
	}
	if room == "" {
		room = defaultRoomName
	}
	_ = a.roomName.SetText(room)
	a.setRelayURLList(bases, 0)
	a.updateNetworkControls()
	a.refreshStatus()
	if smokeTest {
		time.AfterFunc(250*time.Millisecond, func() { a.mw.Synchronize(func() { _ = a.mw.Close() }) })
	}
	a.mw.Run()
	return nil
}

func (a *setupApp) browseInstallDir() {
	dlg := new(walk.FileDialog)
	dlg.Title = "Choose where to install DeskFerry"
	dlg.InitialDirPath = a.installDir.Text()
	if ok, err := dlg.ShowBrowseFolder(a.mw); err != nil {
		a.showError(err)
	} else if ok {
		a.installDir.SetText(dlg.FilePath)
	}
}

func (a *setupApp) updateNetworkControls() {
	enabled := a.enableNetwork == nil || a.enableNetwork.Checked()
	for _, widget := range []walk.Widget{a.relayList, a.relayEdit, a.relayAdd, a.relayUpdate, a.relayDelete, a.relayUp, a.relayDown, a.roomName, a.roomPassword, a.proxy, a.alias} {
		if widget != nil {
			widget.SetEnabled(enabled)
		}
	}
}

func (a *setupApp) updateUNCPreview() {
	if a.uncPreview == nil || a.alias == nil {
		return
	}
	name := strings.TrimSpace(a.alias.Text())
	if name == "" {
		name = homenetwork.DefaultAlias
	}
	a.uncPreview.SetText(`\\` + name + `\sharename`)
}

func (a *setupApp) options() setupOptions {
	exe, _ := os.Executable()
	opts := setupOptions{
		InstallDir:    strings.TrimSpace(a.installDir.Text()),
		SourceDir:     filepath.Dir(exe),
		Destination:   strings.TrimSpace(a.destination),
		RelayBases:    append([]string(nil), a.relayURLs...),
		Room:          strings.TrimSpace(a.roomName.Text()),
		Proxy:         strings.TrimSpace(a.proxy.Text()),
		RoomPassword:  a.roomPassword.Text(),
		Alias:         strings.TrimSpace(a.alias.Text()),
		EnableNetwork: a.enableNetwork.Checked(),
	}
	for _, base := range opts.RelayBases {
		if value, err := tunnel.RelayRoomURL(base, opts.Room); err == nil {
			opts.RelayAddrs = append(opts.RelayAddrs, value)
		}
	}
	if opts.RoomPassword == "" && relayRoom(opts.RelayAddrs) == a.roomProofRoom {
		opts.RoomProof = a.roomProof
	}
	return opts
}

func (a *setupApp) install() {
	opts := a.options()
	if err := validateRequestOptions(opts); err != nil {
		a.showError(err)
		return
	}
	path, err := writeSetupRequest(opts)
	if err != nil {
		a.showError(err)
		return
	}
	if err := relaunchElevated([]string{"-elevated-request", path}); err != nil {
		_ = os.Remove(path)
		a.showError(err)
	}
}

func (a *setupApp) uninstall() {
	if walk.MsgBox(a.mw, productName+" Windows Components", "Remove DeskFerry and its Windows services? Your per-user saved profiles will be kept.", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	if err := relaunchElevated([]string{"-elevated-uninstall"}); err != nil {
		a.showError(err)
	}
}

func (a *setupApp) openHome() {
	path := filepath.Join(strings.TrimSpace(a.installDir.Text()), "DeskFerry.exe")
	if _, err := os.Stat(path); err != nil {
		a.showError(errors.New("DeskFerry is not installed yet"))
		return
	}
	if err := shellExecute("open", path, "", filepath.Dir(path)); err != nil {
		a.showError(err)
	}
}

func (a *setupApp) refreshStatus() {
	installDir := strings.TrimSpace(a.installDir.Text())
	_, homeErr := os.Stat(filepath.Join(installDir, "DeskFerry.exe"))
	serviceState, serviceErr := queryServiceState()
	text := "DeskFerry: not installed"
	if homeErr == nil {
		text = "DeskFerry: installed"
	}
	if serviceErr == nil && serviceState != 0 {
		text += "; file access: " + serviceStateText(serviceState)
	} else {
		text += "; file access: not installed"
	}
	a.status.SetText(text)
	installed := homeErr == nil
	if a.installButton != nil {
		if installed {
			a.installButton.SetText("Apply components")
		} else {
			a.installButton.SetText("Install")
		}
	}
	if a.uninstallButton != nil {
		a.uninstallButton.SetVisible(installed)
	}
}

func (a *setupApp) showError(err error) {
	walk.MsgBox(a.mw, productName+" Windows Components", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func (a *setupApp) setRelayURLList(values []string, index int) {
	a.relayURLs = uniqueRelayURLs(values)
	if len(a.relayURLs) == 0 {
		index = -1
	} else if index < 0 {
		index = 0
	} else if index >= len(a.relayURLs) {
		index = len(a.relayURLs) - 1
	}
	if a.relayList != nil {
		_ = a.relayList.SetModel(append([]string(nil), a.relayURLs...))
		_ = a.relayList.SetCurrentIndex(index)
	}
	a.setRelayEditor(index)
	a.updateRelayButtons()
}

func (a *setupApp) setRelayEditor(index int) {
	if a.relayEdit == nil {
		return
	}
	text := ""
	if index >= 0 && index < len(a.relayURLs) {
		text = a.relayURLs[index]
	}
	_ = a.relayEdit.SetText(text)
}

func (a *setupApp) relaySelectionChanged() {
	index := -1
	if a.relayList != nil {
		index = a.relayList.CurrentIndex()
	}
	a.setRelayEditor(index)
	a.updateRelayButtons()
}

func (a *setupApp) updateRelayButtons() {
	if a.relayList == nil {
		return
	}
	index := a.relayList.CurrentIndex()
	selected := index >= 0 && index < len(a.relayURLs)
	a.relayUpdate.SetEnabled(selected)
	a.relayDelete.SetEnabled(selected)
	a.relayUp.SetEnabled(selected && index > 0)
	a.relayDown.SetEnabled(selected && index < len(a.relayURLs)-1)
}

func (a *setupApp) relayEditorValue() (string, error) {
	value := strings.TrimSpace(a.relayEdit.Text())
	if value == "" {
		return "", errors.New("relay service base URL is required")
	}
	return tunnel.RelayServiceBaseURL(value)
}

func (a *setupApp) addRelayURL() {
	value, err := a.relayEditorValue()
	if err != nil {
		a.showError(err)
		return
	}
	values := append([]string(nil), a.relayURLs...)
	values = append(values, value)
	a.setRelayURLList(values, len(values)-1)
}

func (a *setupApp) updateRelayURL() {
	index := a.relayList.CurrentIndex()
	if index < 0 || index >= len(a.relayURLs) {
		a.addRelayURL()
		return
	}
	value, err := a.relayEditorValue()
	if err != nil {
		a.showError(err)
		return
	}
	values := append([]string(nil), a.relayURLs...)
	values[index] = value
	a.setRelayURLList(values, index)
}

func (a *setupApp) deleteRelayURL() {
	index := a.relayList.CurrentIndex()
	if index < 0 || index >= len(a.relayURLs) {
		return
	}
	values := append([]string(nil), a.relayURLs...)
	values = append(values[:index], values[index+1:]...)
	a.setRelayURLList(values, index)
}

func (a *setupApp) moveRelayURL(delta int) {
	index := a.relayList.CurrentIndex()
	a.moveRelayURLTo(index, index+delta)
}

func (a *setupApp) moveRelayURLTo(from, to int) {
	if from < 0 || from >= len(a.relayURLs) || to < 0 || to >= len(a.relayURLs) || from == to {
		return
	}
	values := append([]string(nil), a.relayURLs...)
	values[from], values[to] = values[to], values[from]
	a.setRelayURLList(values, to)
}

func (a *setupApp) relayListMouseDown(x, y int, button walk.MouseButton) {
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

func (a *setupApp) relayListMouseMove(_, y int, button walk.MouseButton) {
	if button&walk.LeftButton != 0 && a.relayDragIndex >= 0 && absInt(y-a.relayDragStartY) > 4 {
		a.relayDragging = true
	}
}

func (a *setupApp) relayListMouseUp(x, y int, button walk.MouseButton) {
	from := a.relayDragIndex
	dragging := a.relayDragging
	a.relayDragIndex = -1
	a.relayDragging = false
	if button != walk.LeftButton || !dragging || from < 0 {
		return
	}
	to := a.relayListIndexAt(x, y)
	if to >= 0 {
		a.moveRelayURLTo(from, to)
	}
}

func (a *setupApp) relayListIndexAt(x, y int) int {
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

func installFromRequest(path string) (string, error) {
	var opts setupOptions
	if err := readSetupRequest(path, &opts); err != nil {
		return "", err
	}
	return installProduct(opts)
}

func configureNetworkFromRequest(path string) (string, error) {
	var opts setupOptions
	if err := readSetupRequest(path, &opts); err != nil {
		return "", err
	}
	if !opts.EnableNetwork {
		return "", errors.New("selected-profile SMB configuration requires the virtual network adapter")
	}
	installedDir := installedLocation()
	if strings.TrimSpace(installedDir) == "" {
		return "", errors.New("DeskFerry is not installed")
	}
	opts.InstallDir = installedDir
	for _, name := range []string{"DeskFerry.exe", "tun2socks.exe", "wintun.dll"} {
		if _, err := os.Stat(filepath.Join(installedDir, name)); err != nil {
			return "", fmt.Errorf("installed network component is missing %s", name)
		}
	}
	cfg := networkConfig(opts)
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if err := stopAndDeleteNetworkService(); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(configPath(), append(data, '\n'), 0600); err != nil {
		return "", err
	}
	if err := restrictConfigACL(configPath()); err != nil {
		return "", err
	}
	if err := updateHostsAlias(cfg.Alias, cfg.RemoteAddress); err != nil {
		return "", err
	}
	if err := createAndStartNetworkService(filepath.Join(installedDir, "DeskFerry.exe"), configPath()); err != nil {
		return "", err
	}
	if err := writeInstallMetadata(opts); err != nil {
		return "", err
	}
	return fmt.Sprintf("DeskFerry SMB now uses profile %s at \\\\%s\\sharename.", opts.Destination, cfg.Alias), nil
}

func readSetupRequest(path string, opts *setupOptions) error {
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	plain, err := winsecret.Unprotect(data)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(plain), opts)
}

func writeSetupRequest(opts setupOptions) (string, error) {
	data, err := json.Marshal(opts)
	if err != nil {
		return "", err
	}
	protected, err := winsecret.ProtectMachine(string(data))
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "deskferry-home-setup-*.dpapi")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err = file.Write(protected); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

type relayURLFlags []string

func (values *relayURLFlags) String() string { return strings.Join(*values, ";") }

func (values *relayURLFlags) Set(value string) error {
	items := strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ',' || r == '\r' || r == '\n' })
	if len(items) == 0 {
		return errors.New("relay URL cannot be empty")
	}
	*values = append(*values, items...)
	return nil
}

func parseCLIArgs(args []string, stdin io.Reader) (string, setupOptions, error) {
	exe := mustExecutable()
	initial := existingSetupOptions(filepath.Dir(exe))
	opts := initial
	var relayURLs relayURLFlags
	var relayBases relayURLFlags
	fs := flag.NewFlagSet("DeskFerryWindowsComponents", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	action := fs.String("cli-action", "", "install, configure, uninstall, or status")
	fs.StringVar(&opts.InstallDir, "install-dir", initial.InstallDir, "installation directory")
	fs.StringVar(&opts.SourceDir, "source-dir", initial.SourceDir, "component payload directory")
	fs.StringVar(&opts.Destination, "destination", initial.Destination, "Home app destination name to configure")
	fs.Var(&relayURLs, "relay-url", "relay room URL; repeat for multiple relays")
	fs.Var(&relayBases, "relay-base-url", "relay service base URL; repeat for multiple relays")
	fs.StringVar(&opts.Room, "room", initial.Room, "room name appended to relay service base URLs")
	fs.StringVar(&opts.Proxy, "proxy", initial.Proxy, "env, direct, or an HTTP(S) proxy URL")
	fs.StringVar(&opts.Alias, "alias", initial.Alias, "work computer alias")
	fs.BoolVar(&opts.EnableNetwork, "enable-network", initial.EnableNetwork, "install the virtual network adapter and SMB bridge")
	passwordStdin := fs.Bool("room-password-stdin", false, "read the room password from standard input")
	passwordBlob := fs.String("room-password-blob", "", "read a machine-scope DPAPI room password blob")
	if err := fs.Parse(args); err != nil {
		return "", opts, err
	}
	if fs.NArg() != 0 {
		return "", opts, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *action == "" {
		return "", opts, errors.New("-cli-action is required")
	}
	switch *action {
	case "install", "configure", "uninstall", "status":
	default:
		return "", opts, fmt.Errorf("unknown CLI action %q", *action)
	}
	if *action != "install" && *action != "configure" {
		var invalid string
		fs.Visit(func(option *flag.Flag) {
			if invalid == "" && option.Name != "cli-action" {
				invalid = option.Name
			}
		})
		if invalid != "" {
			return "", opts, fmt.Errorf("-%s is only valid with install or configure", invalid)
		}
		return *action, opts, nil
	}
	if *passwordStdin && *passwordBlob != "" {
		return "", opts, errors.New("-room-password-stdin and -room-password-blob cannot be used together")
	}
	if len(relayURLs) > 0 {
		opts.RelayAddrs = uniqueRelayURLs(relayURLs)
	}
	if len(relayBases) > 0 {
		if len(relayURLs) > 0 {
			return "", opts, errors.New("use either -relay-url or -relay-base-url, not both")
		}
		opts.RelayBases = uniqueRelayURLs(relayBases)
		opts.RelayAddrs = nil
		for _, base := range opts.RelayBases {
			value, err := tunnel.RelayRoomURL(base, opts.Room)
			if err != nil {
				return "", opts, err
			}
			opts.RelayAddrs = append(opts.RelayAddrs, value)
		}
	}
	if relayRoom(opts.RelayAddrs) != relayRoom(initial.RelayAddrs) {
		opts.RoomProof = ""
	}
	if *passwordStdin {
		value, err := io.ReadAll(stdin)
		if err != nil {
			return "", opts, fmt.Errorf("read room password: %w", err)
		}
		password := string(value)
		for strings.HasPrefix(password, "\uFEFF") {
			password = strings.TrimPrefix(password, "\uFEFF")
		}
		opts.RoomPassword = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
		if opts.RoomPassword == "" {
			return "", opts, errors.New("room password from standard input is empty")
		}
	} else if *passwordBlob != "" {
		data, err := os.ReadFile(*passwordBlob)
		if err != nil {
			return "", opts, fmt.Errorf("read room password blob: %w", err)
		}
		opts.RoomPassword, err = winsecret.Unprotect(data)
		if err != nil {
			return "", opts, fmt.Errorf("decrypt room password blob: %w", err)
		}
	}
	if err := validateRequestOptions(opts); err != nil {
		return "", opts, err
	}
	return *action, opts, nil
}

func runCLI(args []string, stdin io.Reader, stdout io.Writer) error {
	if hasArg(args, "-cli-help") {
		printCLIUsage(stdout)
		return nil
	}
	action, opts, err := parseCLIArgs(args, stdin)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCLIUsage(stdout)
			return nil
		}
		return err
	}
	if action == "status" {
		state, err := queryServiceState()
		if err != nil {
			return err
		}
		installed := filepath.Join(installedLocation(), "DeskFerry.exe")
		if _, err := os.Stat(installed); err != nil {
			fmt.Fprintln(stdout, "DeskFerry is not installed; virtual network adapter is not installed.")
			return nil
		}
		if state == 0 {
			fmt.Fprintf(stdout, "DeskFerry is installed at %s; virtual network adapter is not installed.\n", installedLocation())
		} else {
			fmt.Fprintf(stdout, "DeskFerry is installed at %s; virtual network adapter service is %s.\n", installedLocation(), serviceStateText(state))
		}
		return nil
	}
	if !isElevated() {
		args := []string{"-no-dialog"}
		requestPath := ""
		if action == "uninstall" {
			args = append(args, "-elevated-uninstall")
		} else {
			requestPath, err = writeSetupRequest(opts)
			if err != nil {
				return err
			}
			args = append(args, "-elevated-request", requestPath)
		}
		if err := relaunchElevated(args); err != nil {
			if requestPath != "" {
				_ = os.Remove(requestPath)
			}
			return fmt.Errorf("request elevation: %w", err)
		}
		fmt.Fprintf(stdout, "Elevation requested for %s.\n", action)
		return nil
	}
	var message string
	if action == "uninstall" {
		message, err = uninstallProduct()
	} else {
		message, err = installProduct(opts)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, message)
	return nil
}

func printCLIUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: DeskFerry.exe -windows-setup -cli-action ACTION [options]")
	fmt.Fprintln(output, "Actions: install, configure, uninstall, status")
	fmt.Fprintln(output, "  -install-dir PATH          Installation directory")
	fmt.Fprintln(output, "  -source-dir PATH           Setup payload directory")
	fmt.Fprintln(output, "  -destination NAME         Home app destination profile to configure")
	fmt.Fprintln(output, "  -relay-base-url URL       Relay service base URL; repeat for multiple relays")
	fmt.Fprintln(output, "  -room NAME                Room name appended to every relay service base URL")
	fmt.Fprintln(output, "  -relay-url URL             Relay room URL; repeat for multiple relays")
	fmt.Fprintln(output, "  -proxy VALUE               env, direct, or an HTTP(S) proxy URL")
	fmt.Fprintln(output, "  -alias NAME                Work computer alias")
	fmt.Fprintln(output, "  -enable-network=BOOL       Install the virtual adapter and SMB bridge")
	fmt.Fprintln(output, "  -room-password-stdin       Read the room password from standard input")
	fmt.Fprintln(output, "  -room-password-blob PATH   Read a machine-scope DPAPI password blob")
	fmt.Fprintln(output, "Installed settings are defaults; the room proof is preserved when the room is unchanged.")
}

type homeClientSettings struct {
	ListenAddr          string                  `json:"listen_addr,omitempty"`
	RelayAddr           string                  `json:"relay_addr,omitempty"`
	RelayAddrs          []string                `json:"relay_addrs"`
	Proxy               string                  `json:"proxy"`
	RDPUser             string                  `json:"rdp_user,omitempty"`
	WinRMListenAddr     string                  `json:"winrm_listen_addr,omitempty"`
	WinRMUser           string                  `json:"winrm_user,omitempty"`
	RoomProof           string                  `json:"room_proof"`
	Destinations        []homeClientDestination `json:"destinations"`
	SelectedDestination string                  `json:"selected_destination"`
}

type homeClientDestination struct {
	Name            string   `json:"name"`
	RelayAddrs      []string `json:"relay_addrs"`
	RelayBases      []string `json:"relay_bases,omitempty"`
	Room            string   `json:"room,omitempty"`
	Proxy           string   `json:"proxy,omitempty"`
	RoomProof       string   `json:"room_proof"`
	WindowsUser     string   `json:"windows_user,omitempty"`
	PasswordlessRDP bool     `json:"passwordless_rdp,omitempty"`
	SMBAlias        string   `json:"smb_alias,omitempty"`
}

type installMetadata struct {
	InstallDir    string   `json:"install_dir"`
	Destination   string   `json:"destination,omitempty"`
	RelayAddrs    []string `json:"relay_addrs,omitempty"`
	Proxy         string   `json:"proxy,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	EnableNetwork bool     `json:"enable_network"`
}

func existingSetupOptions(sourceDir string) setupOptions {
	opts := setupOptions{
		InstallDir:    defaultInstallDir(),
		SourceDir:     sourceDir,
		RelayAddrs:    []string{defaultRelayURL, defaultOCIRelayBase + "/" + defaultRoomName},
		RelayBases:    []string{defaultAzureRelayBase, defaultOCIRelayBase},
		Room:          defaultRoomName,
		Proxy:         "env",
		Alias:         homenetwork.DefaultAlias,
		EnableNetwork: true,
	}
	installedDir := installedLocation()
	if _, err := os.Stat(filepath.Join(installedDir, "DeskFerry.exe")); err == nil {
		opts.InstallDir = installedDir
		opts.EnableNetwork = false
	}
	metadataLoaded := false
	if metadata, ok := readInstallMetadata(); ok {
		if relays := uniqueRelayURLs(metadata.RelayAddrs); len(relays) > 0 {
			metadataLoaded = true
			opts.RelayAddrs = relays
		}
		opts.Proxy = strings.TrimSpace(metadata.Proxy)
		if strings.TrimSpace(metadata.Alias) != "" {
			opts.Alias = strings.TrimSpace(metadata.Alias)
		}
		opts.EnableNetwork = metadata.EnableNetwork
	}
	settingsPath := homeClientSettingsPath()
	var settings homeClientSettings
	settingsLoaded := false
	if data, err := os.ReadFile(settingsPath); err == nil && json.Unmarshal(data, &settings) == nil {
		settingsLoaded = true
		opts.Destination = strings.TrimSpace(settings.SelectedDestination)
	}
	var cfg homenetwork.Config
	if data, err := os.ReadFile(configPath()); err == nil && json.Unmarshal(data, &cfg) == nil {
		cfg = cfg.WithDefaults(opts.InstallDir)
		opts.RelayAddrs = cfg.RelayAddrs
		opts.Proxy = cfg.Proxy
		opts.RoomProof = cfg.RoomProof
		opts.Alias = cfg.Alias
		opts.EnableNetwork = true
		return finalizeSetupRelayParts(opts)
	}
	if metadataLoaded {
		return finalizeSetupRelayParts(opts)
	}
	if settingsLoaded {
		relays, proof, alias := settings.RelayAddrs, settings.RoomProof, ""
		for _, destination := range settings.Destinations {
			if destination.Name == settings.SelectedDestination {
				relays, proof, alias = destination.RelayAddrs, destination.RoomProof, destination.SMBAlias
				if strings.TrimSpace(destination.Proxy) != "" {
					opts.Proxy = strings.TrimSpace(destination.Proxy)
				}
				break
			}
		}
		if values := uniqueRelayURLs(relays); len(values) > 0 {
			opts.RelayAddrs = values
			opts.RoomProof = strings.TrimSpace(proof)
		}
		if strings.TrimSpace(settings.Proxy) != "" {
			opts.Proxy = strings.TrimSpace(settings.Proxy)
		}
		if strings.TrimSpace(alias) != "" {
			opts.Alias = strings.TrimSpace(alias)
		}
	}
	return finalizeSetupRelayParts(opts)
}

func finalizeSetupRelayParts(opts setupOptions) setupOptions {
	if bases, room, err := tunnel.SplitRelayRoomURLs(opts.RelayAddrs); err == nil && len(bases) > 0 {
		opts.RelayBases, opts.Room = bases, room
	}
	if opts.Room == "" {
		opts.Room = defaultRoomName
	}
	return opts
}

func relayRoom(relays []string) string {
	if len(relays) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(tunnel.RelayRoomToken(relays[0], "")))
}

func mustExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

func setupPackagePath(sourceDir string) string {
	return setupPackagePathForExecutable(sourceDir, mustExecutable())
}

func setupPackagePathForExecutable(sourceDir, executable string) string {
	sourceAbs, sourceErr := filepath.Abs(sourceDir)
	executableAbs, executableErr := filepath.Abs(executable)
	if sourceErr == nil && executableErr == nil && strings.EqualFold(sourceAbs, filepath.Dir(executableAbs)) {
		return executableAbs
	}
	return filepath.Join(sourceDir, "DeskFerry.exe")
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func reportResult(noDialog bool, message string, err error) {
	if noDialog {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stdout, message)
		}
		return
	}
	if err != nil {
		windowsMessageBox(productName+" Windows Components", err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		return
	}
	windowsMessageBox(productName+" Windows Components", message, windows.MB_OK|windows.MB_ICONINFORMATION)
}

func preserveInstalledRoomProof(opts *setupOptions) {
	if opts == nil || opts.RoomPassword != "" || opts.RoomProof != "" || !opts.EnableNetwork {
		return
	}
	var cfg homenetwork.Config
	data, err := os.ReadFile(configPath())
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return
	}
	if relayRoom(opts.RelayAddrs) == relayRoom(cfg.RelayAddrs) {
		opts.RoomProof = strings.TrimSpace(cfg.RoomProof)
	}
}

func validateOptions(opts setupOptions) error {
	if strings.TrimSpace(opts.InstallDir) == "" {
		return errors.New("install location is required")
	}
	setupPath := setupPackagePath(opts.SourceDir)
	if _, err := os.Stat(setupPath); err != nil {
		return errors.New("DeskFerry executable is unavailable")
	}
	required := []string{}
	if opts.EnableNetwork {
		required = append(required, "tun2socks.exe", "wintun.dll", "LICENSE-Wintun.txt", "LICENSE-tun2socks.txt")
	}
	missingSibling := false
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(opts.SourceDir, name)); err != nil {
			missingSibling = true
			break
		}
	}
	if missingSibling {
		if err := validateEmbeddedPayload(setupPath, required); err != nil {
			return err
		}
	}
	if !opts.EnableNetwork {
		return nil
	}
	if strings.TrimSpace(opts.RoomPassword) == "" && strings.TrimSpace(opts.RoomProof) == "" {
		return errors.New("enter the same room password configured on the work agent")
	}
	cfg := networkConfig(opts)
	return cfg.Validate()
}

func validateRequestOptions(opts setupOptions) error {
	if opts.EnableNetwork && opts.RoomPassword == "" && opts.RoomProof == "" {
		if _, err := os.Stat(configPath()); err == nil {
			opts.RoomProof = "preserve-installed-proof-after-elevation"
		}
	}
	return validateOptions(opts)
}

func networkConfig(opts setupOptions) homenetwork.Config {
	proof := strings.TrimSpace(opts.RoomProof)
	if len(opts.RelayAddrs) > 0 && opts.RoomPassword != "" {
		proof = tunnel.RoomPasswordProof(opts.RelayAddrs[0], "", opts.RoomPassword)
	}
	return (homenetwork.Config{
		RelayAddrs:    opts.RelayAddrs,
		Proxy:         opts.Proxy,
		RoomProof:     proof,
		Alias:         opts.Alias,
		Tun2SocksPath: filepath.Join(opts.InstallDir, "tun2socks.exe"),
	}).WithDefaults(opts.InstallDir)
}

func installProduct(opts setupOptions) (string, error) {
	installMutex, err := acquireSetupInstallMutex()
	if err != nil {
		return "", err
	}
	defer func() {
		_ = windows.ReleaseMutex(installMutex)
		_ = windows.CloseHandle(installMutex)
	}()
	legacyInstallDir := installedLocation()
	preserveInstalledRoomProof(&opts)
	if err := validateOptions(opts); err != nil {
		return "", err
	}
	sourceDir, cleanup, err := materializePayload(opts.SourceDir, opts.EnableNetwork)
	if err != nil {
		return "", err
	}
	defer cleanup()
	opts.SourceDir = sourceDir
	installDir, err := filepath.Abs(opts.InstallDir)
	if err != nil {
		return "", err
	}
	opts.InstallDir = installDir
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}
	for _, name := range []string{"DeskFerry.exe"} {
		target := filepath.Join(installDir, name)
		current, _ := os.Executable()
		if currentAbs, currentErr := filepath.Abs(current); currentErr == nil && strings.EqualFold(currentAbs, target) {
			continue
		}
		if err := ensureReplaceable(target); err != nil {
			return "", fmt.Errorf("prepare to install %s: %w (close DeskFerry if it is running)", name, err)
		}
	}
	if err := stopAndDeleteNetworkService(); err != nil {
		return "", err
	}
	for _, name := range []string{"DeskFerry.exe"} {
		target := filepath.Join(installDir, name)
		current, _ := os.Executable()
		if currentAbs, currentErr := filepath.Abs(current); currentErr == nil && strings.EqualFold(currentAbs, target) {
			continue
		}
		if err := copyFile(filepath.Join(opts.SourceDir, name), target); err != nil {
			return "", fmt.Errorf("install %s: %w (close DeskFerry if it is running)", name, err)
		}
	}
	if err := createShortcuts(installDir); err != nil {
		return "", err
	}
	if err := registerUninstaller(installDir); err != nil {
		return "", err
	}
	if !opts.EnableNetwork {
		if err := removeHostsAlias(); err != nil {
			return "", err
		}
		_ = os.Remove(configPath())
		for _, name := range []string{"tun2socks.exe", "wintun.dll", "LICENSE-Wintun.txt", "LICENSE-tun2socks.txt"} {
			_ = os.Remove(filepath.Join(installDir, name))
		}
		if err := writeInstallMetadata(opts); err != nil {
			return "", err
		}
		if err := configureHomeClient(opts, networkConfig(opts).RoomProof); err != nil {
			return "", err
		}
		removeLegacyWindowsBinaries(installDir)
		removeSupersededInstall(legacyInstallDir, installDir)
		return "DeskFerry was installed without the optional virtual network adapter.", nil
	}
	for _, name := range []string{"tun2socks.exe", "wintun.dll", "LICENSE-Wintun.txt", "LICENSE-tun2socks.txt"} {
		if err := copyFile(filepath.Join(opts.SourceDir, name), filepath.Join(installDir, name)); err != nil {
			return "", fmt.Errorf("install %s: %w", name, err)
		}
	}
	cfg := networkConfig(opts)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(configPath(), append(data, '\n'), 0600); err != nil {
		return "", err
	}
	if err := restrictConfigACL(configPath()); err != nil {
		return "", err
	}
	if err := updateHostsAlias(cfg.Alias, cfg.RemoteAddress); err != nil {
		return "", err
	}
	if err := createAndStartNetworkService(filepath.Join(installDir, "DeskFerry.exe"), configPath()); err != nil {
		return "", err
	}
	if err := writeInstallMetadata(opts); err != nil {
		return "", err
	}
	if err := configureHomeClient(opts, cfg.RoomProof); err != nil {
		return "", err
	}
	removeLegacyWindowsBinaries(installDir)
	removeSupersededInstall(legacyInstallDir, installDir)
	return fmt.Sprintf("DeskFerry and file access were installed. Open \\\\%s\\sharename after the work service enables SMB.", cfg.Alias), nil
}

func removeLegacyWindowsBinaries(installDir string) {
	current, _ := os.Executable()
	for _, name := range []string{
		"DeskFerryHome.exe",
		"DeskFerryHomeNetwork.exe",
		"DeskFerryHomeSetup.exe",
		"DeskFerryHomeSetup-debug.exe",
	} {
		path := filepath.Join(installDir, name)
		if strings.EqualFold(path, current) {
			continue
		}
		_ = os.Remove(path)
	}
}

func removeSupersededInstall(oldDir, newDir string) {
	oldAbs, oldErr := filepath.Abs(strings.TrimSpace(oldDir))
	newAbs, newErr := filepath.Abs(strings.TrimSpace(newDir))
	if oldErr != nil || newErr != nil || oldAbs == "." || strings.EqualFold(oldAbs, newAbs) {
		return
	}
	for _, name := range append(installedFiles,
		"DeskFerryHome.exe",
		"DeskFerryHomeNetwork.exe",
		"DeskFerryHomeSetup.exe",
		"DeskFerryHomeSetup-debug.exe",
	) {
		_ = os.Remove(filepath.Join(oldAbs, name))
	}
	_ = os.Remove(oldAbs)
}

func acquireSetupInstallMutex() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(setupInstallMutexName)
	if err != nil {
		return 0, err
	}
	handle, createErr := windows.CreateMutex(nil, false, name)
	if createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		return 0, fmt.Errorf("create setup install mutex: %w", createErr)
	}
	result, err := windows.WaitForSingleObject(handle, uint32((2*time.Minute)/time.Millisecond))
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("wait for another DeskFerry component update: %w", err)
	}
	if result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("another DeskFerry component update did not finish within two minutes")
	}
	return handle, nil
}

func ensureReplaceable(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}

func configureHomeClient(opts setupOptions, proof string) error {
	path := homeClientSettingsPath()
	settings := homeClientSettings{
		ListenAddr:      "127.0.0.1:3390",
		WinRMListenAddr: "127.0.0.1:3391",
		Proxy:           "env",
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("decode Home app settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Home app settings: %w", err)
	}
	relays := uniqueRelayURLs(opts.RelayAddrs)
	if len(relays) == 0 {
		return errors.New("at least one relay room URL is required to configure the Home app")
	}
	destinationName := strings.TrimSpace(opts.Destination)
	if destinationName == "" {
		destinationName = strings.TrimSpace(settings.SelectedDestination)
	}
	if destinationName == "" {
		destinationName = "Work"
	}
	index := -1
	for i := range settings.Destinations {
		if strings.EqualFold(strings.TrimSpace(settings.Destinations[i].Name), destinationName) {
			index = i
			break
		}
	}
	if index < 0 {
		settings.Destinations = append(settings.Destinations, homeClientDestination{Name: destinationName})
		index = len(settings.Destinations) - 1
	}
	existing := settings.Destinations[index]
	if relayRoom(existing.RelayAddrs) == relayRoom(relays) {
		existingProof := strings.TrimSpace(existing.RoomProof)
		if opts.RoomPassword == "" && existingProof != "" {
			// An in-place install loads the network component's proof from
			// machine metadata. Do not let it overwrite the Home user's
			// independently saved proof unless a new password was supplied.
			proof = existingProof
		} else if proof == "" {
			proof = existingProof
		}
	}
	settings.Destinations[index].Name = destinationName
	settings.Destinations[index].RelayAddrs = relays
	settings.Destinations[index].RelayBases = append([]string(nil), opts.RelayBases...)
	settings.Destinations[index].Room = strings.TrimSpace(opts.Room)
	settings.Destinations[index].Proxy = strings.TrimSpace(opts.Proxy)
	if len(settings.Destinations[index].RelayBases) == 0 || settings.Destinations[index].Room == "" {
		if bases, room, err := tunnel.SplitRelayRoomURLs(relays); err == nil {
			settings.Destinations[index].RelayBases = bases
			settings.Destinations[index].Room = room
		}
	}
	settings.Destinations[index].RoomProof = strings.TrimSpace(proof)
	settings.Destinations[index].SMBAlias = strings.TrimSpace(opts.Alias)
	settings.SelectedDestination = destinationName
	settings.RelayAddr = relays[0]
	settings.RelayAddrs = relays
	settings.RoomProof = strings.TrimSpace(proof)
	settings.Proxy = strings.TrimSpace(opts.Proxy)
	if settings.Proxy == "" {
		settings.Proxy = "env"
	}
	if strings.TrimSpace(settings.ListenAddr) == "" {
		settings.ListenAddr = "127.0.0.1:3390"
	}
	if strings.TrimSpace(settings.WinRMListenAddr) == "" {
		settings.WinRMListenAddr = "127.0.0.1:3391"
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("save Home app settings: %w", err)
	}
	return nil
}

func uninstallProduct() (string, error) {
	if err := stopAndDeleteNamedService("DeskFerryAgent"); err != nil {
		return "", err
	}
	if err := stopAndDeleteNetworkService(); err != nil {
		return "", err
	}
	if err := removeHostsAlias(); err != nil {
		return "", err
	}
	_ = os.Remove(configPath())
	installDir := installedLocation()
	_ = os.RemoveAll(shortcutDir())
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Uninstall\DeskFerry`)
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Uninstall\DeskFerryHome`)
	_ = os.Remove(installMetadataPath())
	current, _ := os.Executable()
	for _, name := range installedFiles {
		path := filepath.Join(installDir, name)
		if strings.EqualFold(path, current) {
			pathPtr, _ := windows.UTF16PtrFromString(path)
			_ = windows.MoveFileEx(pathPtr, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
			continue
		}
		_ = os.Remove(path)
	}
	removeLegacyWindowsBinaries(installDir)
	_ = os.Remove(installDir)
	return "DeskFerry and its Windows services were removed. Saved per-user profiles were kept.", nil
}

func stopAndDeleteNamedService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if status, queryErr := s.Query(); queryErr == nil && status.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		if err := waitForServiceState(s, svc.Stopped, 30*time.Second); err != nil {
			return err
		}
	}
	return s.Delete()
}

func validateEmbeddedPayload(setupPath string, required []string) error {
	archive, err := zip.OpenReader(setupPath)
	if err != nil {
		return fmt.Errorf("DeskFerry executable is missing its embedded payload: %w", err)
	}
	defer archive.Close()
	found := make(map[string]bool)
	for _, file := range archive.File {
		found[filepath.Base(file.Name)] = true
	}
	for _, name := range required {
		if !found[name] {
			return fmt.Errorf("DeskFerry executable is missing %s", name)
		}
	}
	return nil
}

func materializePayload(sourceDir string, networkEnabled bool) (string, func(), error) {
	required := []string{}
	if networkEnabled {
		required = append(required, "tun2socks.exe", "wintun.dll")
	}
	setupPath := setupPackagePath(sourceDir)
	tempDir, err := os.MkdirTemp("", "deskferry-home-payload-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	if err := copyFile(setupPath, filepath.Join(tempDir, "DeskFerry.exe")); err != nil {
		cleanup()
		return "", func() {}, err
	}
	allSiblings := true
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(sourceDir, name)); err != nil {
			allSiblings = false
			break
		}
	}
	if allSiblings {
		for _, name := range append(required, "LICENSE-Wintun.txt", "LICENSE-tun2socks.txt") {
			if _, err := os.Stat(filepath.Join(sourceDir, name)); err == nil {
				if err := copyFile(filepath.Join(sourceDir, name), filepath.Join(tempDir, name)); err != nil {
					cleanup()
					return "", func() {}, err
				}
			}
		}
		return tempDir, cleanup, nil
	}
	archive, err := zip.OpenReader(setupPath)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("open embedded component payload: %w", err)
	}
	defer archive.Close()
	allowed := make(map[string]bool)
	for _, name := range installedFiles {
		allowed[name] = true
	}
	for _, file := range archive.File {
		name := filepath.Base(file.Name)
		if !allowed[name] || file.FileInfo().IsDir() {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		destination, err := os.OpenFile(filepath.Join(tempDir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			_, err = io.Copy(destination, reader)
		}
		closeErr := reader.Close()
		if destination != nil {
			if destinationErr := destination.Close(); err == nil {
				err = destinationErr
			}
		}
		if err == nil {
			err = closeErr
		}
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("extract %s: %w", name, err)
		}
	}
	return tempDir, cleanup, nil
}

func writeInstallMetadata(opts setupOptions) error {
	data, err := json.Marshal(installMetadata{
		InstallDir:    opts.InstallDir,
		Destination:   strings.TrimSpace(opts.Destination),
		RelayAddrs:    uniqueRelayURLs(opts.RelayAddrs),
		Proxy:         strings.TrimSpace(opts.Proxy),
		Alias:         strings.TrimSpace(opts.Alias),
		EnableNetwork: opts.EnableNetwork,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(installMetadataPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(installMetadataPath(), data, 0644)
}

func readInstallMetadata() (installMetadata, bool) {
	var metadata installMetadata
	data, err := os.ReadFile(installMetadataPath())
	if err != nil || json.Unmarshal(data, &metadata) != nil {
		return metadata, false
	}
	return metadata, true
}

func installedLocation() string {
	if metadata, ok := readInstallMetadata(); ok && strings.TrimSpace(metadata.InstallDir) != "" {
		return metadata.InstallDir
	}
	return defaultInstallDir()
}

func registerUninstaller(installDir string) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Uninstall\DeskFerry`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("register uninstaller: %w", err)
	}
	defer key.Close()
	setupPath := filepath.Join(installDir, "DeskFerry.exe")
	values := map[string]string{
		"DisplayName":     productName,
		"DisplayVersion":  productVersion,
		"Publisher":       "DeskFerry",
		"InstallLocation": installDir,
		"DisplayIcon":     setupPath + ",0",
		"UninstallString": syscall.EscapeArg(setupPath) + " -windows-setup -elevated-uninstall",
	}
	for name, value := range values {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	if err := key.SetDWordValue("NoModify", 1); err != nil {
		return err
	}
	return key.SetDWordValue("NoRepair", 1)
}

func createAndStartNetworkService(exePath, cfgPath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	args := []string{"-home-network-service", "-service", "-config", cfgPath}
	config := mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: serviceBinaryPath(exePath, args),
		DisplayName:    networkServiceDisplay,
		Description:    "Restricted DeskFerry SMB virtual network adapter for the Home computer.",
	}
	s, err := m.CreateService(networkServiceName, exePath, config, args...)
	if err != nil {
		return fmt.Errorf("create %s service: %w", networkServiceDisplay, err)
	}
	defer s.Close()
	_ = eventlog.InstallAsEventCreate(networkServiceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 30 * time.Second}}, 86400)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := s.Start(); err != nil {
		return fmt.Errorf("start %s: %w", networkServiceDisplay, err)
	}
	return waitForServiceState(s, svc.Running, 30*time.Second)
}

func stopAndDeleteNetworkService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(networkServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close()
	status, _ := s.Query()
	if status.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		if err := waitForServiceState(s, svc.Stopped, 30*time.Second); err != nil {
			return err
		}
	}
	if err := s.Delete(); err != nil {
		return err
	}
	_ = eventlog.Remove(networkServiceName)
	return nil
}

func waitForServiceState(s *mgr.Service, wanted svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == wanted {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to become %s", networkServiceDisplay, serviceStateText(uint32(wanted)))
}

func queryServiceState() (uint32, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(networkServiceName)
	if err != nil {
		return 0, err
	}
	handle, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(handle)
	var status windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(handle, &status); err != nil {
		return 0, err
	}
	return status.CurrentState, nil
}

func updateHostsAlias(alias, address string) error {
	path := hostsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cleaned := removeManagedHostsBlock(string(data))
	if !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\r\n"
	}
	cleaned += hostsBeginMarker + "\r\n" + address + " " + alias + "\r\n" + hostsEndMarker + "\r\n"
	return os.WriteFile(path, []byte(cleaned), 0644)
}

func removeHostsAlias() error {
	path := hostsPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cleaned := removeManagedHostsBlock(string(data))
	return os.WriteFile(path, []byte(cleaned), 0644)
}

func removeManagedHostsBlock(text string) string {
	for {
		start := strings.Index(text, hostsBeginMarker)
		if start < 0 {
			return text
		}
		endOffset := strings.Index(text[start:], hostsEndMarker)
		if endOffset < 0 {
			return text[:start]
		}
		end := start + endOffset + len(hostsEndMarker)
		for end < len(text) && (text[end] == '\r' || text[end] == '\n') {
			end++
		}
		text = text[:start] + text[end:]
	}
}

func restrictConfigACL(path string) error {
	out, err := hiddenCommand("icacls.exe", path, "/inheritance:r", "/grant:r", "SYSTEM:(F)", "Administrators:(F)").CombinedOutput()
	if err != nil {
		return fmt.Errorf("protect network configuration: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func createShortcuts(installDir string) error {
	dir := shortcutDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := createShortcut(filepath.Join(dir, "DeskFerry.lnk"), filepath.Join(installDir, "DeskFerry.exe"), "", installDir, "DeskFerry"); err != nil {
		return err
	}
	return createShortcut(filepath.Join(dir, "Remove DeskFerry.lnk"), filepath.Join(installDir, "DeskFerry.exe"), "-windows-setup -elevated-uninstall", installDir, "Remove DeskFerry")
}

func createShortcut(shortcut, target, args, workDir, description string) error {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	script := "$w=New-Object -ComObject WScript.Shell;$s=$w.CreateShortcut(" + quote(shortcut) + ");$s.TargetPath=" + quote(target) + ";$s.Arguments=" + quote(args) + ";$s.WorkingDirectory=" + quote(workDir) + ";$s.Description=" + quote(description) + ";$s.IconLocation=" + quote(target+",0") + ";$s.Save()"
	out, err := hiddenCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create Start menu shortcut: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func configPath() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "DeskFerry", "home-network.json")
}

func homeClientSettingsPath() string {
	base := strings.TrimSpace(os.Getenv("APPDATA"))
	if base == "" {
		base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(base, "DeskFerry", "home-client.json")
}

func installMetadataPath() string {
	return filepath.Join(filepath.Dir(configPath()), "home-install.json")
}

func hostsPath() string {
	root := strings.TrimSpace(os.Getenv("SystemRoot"))
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}

func shortcutDir() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", productName)
}

func defaultInstallDir() string {
	base := strings.TrimSpace(os.Getenv("ProgramFiles"))
	if base == "" {
		base = `C:\Program Files`
	}
	return filepath.Join(base, productName)
}

func copyFile(source, destination string) error {
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	destinationAbs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if strings.EqualFold(sourceAbs, destinationAbs) {
		return nil
	}
	in, err := os.Open(sourceAbs)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destinationAbs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
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

func serviceBinaryPath(exePath string, args []string) string {
	parts := []string{syscall.EscapeArg(exePath)}
	for _, arg := range args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

func relaunchElevated(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args = append([]string{"-windows-setup"}, args...)
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, syscall.EscapeArg(arg))
	}
	return shellExecute("runas", exe, strings.Join(quoted, " "), filepath.Dir(exe))
}

func shellExecute(verb, file, params, dir string) error {
	verbPtr, _ := windows.UTF16PtrFromString(verb)
	filePtr, _ := windows.UTF16PtrFromString(file)
	paramsPtr, _ := windows.UTF16PtrFromString(params)
	dirPtr, _ := windows.UTF16PtrFromString(dir)
	return windows.ShellExecute(0, verbPtr, filePtr, paramsPtr, dirPtr, windows.SW_SHOWNORMAL)
}

func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func uniqueRelayURLs(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value != "" && !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result
}

func serviceStateText(state uint32) string {
	switch svc.State(state) {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	default:
		return fmt.Sprintf("state %d", state)
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func windowsMessageBox(title, text string, style uint32) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	windows.MessageBox(0, textPtr, titlePtr, style)
}

// Keep net linked in GUI builds and make the intended endpoint explicit to PE
// analysis tools: this installer never opens a listener itself.
var _ = net.IPv4len
