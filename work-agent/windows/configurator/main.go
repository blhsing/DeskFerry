//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
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
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"deskferry/internal/winsecret"
)

const (
	serviceName        = "DeskFerryAgent"
	serviceDisplayName = "DeskFerry Agent"
	installedAgentName = "agent.exe"
	defaultRelayURL    = "https://test-officialwebsite.azurewebsites.net/relay/"
	defaultSMBAlias    = "deskferry-work"
)

type app struct {
	mw              *walk.MainWindow
	installDir      *walk.LineEdit
	agentPath       *walk.LineEdit
	relayList       *walk.ListBox
	relayEdit       *walk.LineEdit
	relayAdd        *walk.PushButton
	relayUpdate     *walk.PushButton
	relayDelete     *walk.PushButton
	relayUp         *walk.PushButton
	relayDown       *walk.PushButton
	roomPassword    *walk.LineEdit
	clearPassword   *walk.CheckBox
	winrmAddr       *walk.LineEdit
	smbAddr         *walk.LineEdit
	smbAlias        *walk.LineEdit
	status          *walk.Label
	log             *walk.TextEdit
	relayURLs       []string
	relayDragIndex  int
	relayDragStartY int
	relayDragging   bool
}

type actionOptions struct {
	InstallDir        string
	AgentPath         string
	RelayURL          string
	RoomPassword      string
	RoomPasswordBlob  string
	ClearRoomPassword bool
	WinRMAddr         string
	SMBAddr           string
	SMBAlias          string
}

type serviceInfo struct {
	Installed bool
	State     uint32
	ProcessID uint32
}

func main() {
	if hasArg(os.Args[1:], "-ui-smoke-test") {
		if err := (&app{}).run(true); err != nil {
			os.Exit(1)
		}
		return
	}
	if hasArg(os.Args[1:], "-cli-action") || hasArg(os.Args[1:], "-cli-help") {
		if err := runCLI(os.Args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if hasElevatedAction(os.Args[1:]) {
		if !isElevated() {
			if err := relaunchCurrentArgsElevated(os.Args[1:]); err != nil {
				windowsMessageBox(appTitle(), err.Error(), windows.MB_OK|windows.MB_ICONERROR)
				os.Exit(1)
			}
			return
		}
		runElevatedAction(os.Args[1:])
		return
	}
	if err := (&app{}).run(false); err != nil {
		windowsMessageBox(appTitle(), err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}
}

func (a *app) run(smokeTest bool) error {
	installDir := defaultInstallDir()
	agentPath := defaultAgentPath()
	a.relayDragIndex = -1

	window := MainWindow{
		AssignTo: &a.mw,
		Title:    appTitle(),
		MinSize:  Size{Width: 760, Height: 520},
		Size:     Size{Width: 860, Height: 620},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 8},
		Visible:  !smokeTest,
		Children: []Widget{
			GroupBox{
				Title:  "Files",
				Layout: Grid{Columns: 3, Spacing: 6},
				Children: []Widget{
					Label{Text: "Install directory"},
					LineEdit{AssignTo: &a.installDir, Text: installDir, ColumnSpan: 1},
					PushButton{Text: "Browse", OnClicked: a.browseInstallDir},

					Label{Text: "Agent executable"},
					LineEdit{AssignTo: &a.agentPath, Text: agentPath, ColumnSpan: 1},
					PushButton{Text: "Browse", OnClicked: a.browseAgentPath},

					Label{Text: "Relay URLs"},
					Composite{
						ColumnSpan: 2,
						Layout:     VBox{Spacing: 6},
						Children: []Widget{
							ListBox{
								AssignTo:              &a.relayList,
								Model:                 []string{defaultRelayURL},
								MinSize:               Size{Height: 74},
								OnCurrentIndexChanged: a.relaySelectionChanged,
								OnMouseDown:           a.relayListMouseDown,
								OnMouseMove:           a.relayListMouseMove,
								OnMouseUp:             a.relayListMouseUp,
							},
							Composite{
								Layout: Grid{Columns: 3, Spacing: 6},
								Children: []Widget{
									Label{Text: "Selected URL"},
									LineEdit{AssignTo: &a.relayEdit, CueBanner: defaultRelayURL, ColumnSpan: 2},
								},
							},
							Composite{
								Layout: Flow{Spacing: 6},
								Children: []Widget{
									PushButton{AssignTo: &a.relayAdd, Text: "Add", MinSize: Size{Width: 72, Height: 30}, OnClicked: a.addRelayURL},
									PushButton{AssignTo: &a.relayUpdate, Text: "Update", MinSize: Size{Width: 82, Height: 30}, OnClicked: a.updateRelayURL},
									PushButton{AssignTo: &a.relayDelete, Text: "Delete", MinSize: Size{Width: 78, Height: 30}, OnClicked: a.deleteRelayURL},
									PushButton{AssignTo: &a.relayUp, Text: "Up", MinSize: Size{Width: 64, Height: 30}, OnClicked: func() { a.moveRelayURL(-1) }},
									PushButton{AssignTo: &a.relayDown, Text: "Down", MinSize: Size{Width: 64, Height: 30}, OnClicked: func() { a.moveRelayURL(1) }},
								},
							},
						},
					},
					Label{Text: "Room password"},
					LineEdit{AssignTo: &a.roomPassword, PasswordMode: true, CueBanner: "blank keeps the current password", ColumnSpan: 2},
					Label{Text: "Password options"},
					CheckBox{AssignTo: &a.clearPassword, Text: "Clear room password (also disables WinRM and SMB)", ColumnSpan: 2},
					Label{Text: "WinRM target"},
					LineEdit{AssignTo: &a.winrmAddr, Text: "127.0.0.1:5985", CueBanner: "blank disables WinRM", ColumnSpan: 2},
					Label{Text: "SMB target"},
					LineEdit{AssignTo: &a.smbAddr, Text: "127.0.0.1:445", CueBanner: "blank disables SMB", ColumnSpan: 2},
					Label{Text: "SMB server alias"},
					LineEdit{AssignTo: &a.smbAlias, Text: defaultSMBAlias, CueBanner: defaultSMBAlias, ColumnSpan: 2},
				},
			},
			GroupBox{
				Title:  "Service",
				Layout: VBox{Spacing: 6},
				Children: []Widget{
					Label{AssignTo: &a.status, Text: "Status: checking..."},
					Composite{
						Layout: Flow{Spacing: 6},
						Children: []Widget{
							PushButton{Text: "Install / Update", OnClicked: func() { a.runAction("install") }},
							PushButton{Text: "Start", OnClicked: func() { a.runAction("start") }},
							PushButton{Text: "Stop", OnClicked: func() { a.runAction("stop") }},
							PushButton{Text: "Restart", OnClicked: func() { a.runAction("restart") }},
							PushButton{Text: "Uninstall", OnClicked: func() { a.runAction("uninstall") }},
							PushButton{Text: "Self-test", OnClicked: a.runSelfTest},
							PushButton{Text: "Open Folder", OnClicked: a.openInstallFolder},
							PushButton{Text: "Refresh", OnClicked: a.refreshStatus},
						},
					},
				},
			},
			TextEdit{AssignTo: &a.log, ReadOnly: true, VScroll: true, MinSize: Size{Height: 220}},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	a.setRelayURLList([]string{defaultRelayURL}, 0)
	if smokeTest {
		time.AfterFunc(250*time.Millisecond, func() {
			a.mw.Synchronize(func() {
				_ = a.mw.Close()
			})
		})
		a.mw.Run()
		return nil
	}
	a.refreshStatus()
	a.appendLog("Ready.")
	a.mw.Run()
	return nil
}

func appTitle() string {
	return "DeskFerry Agent Configurator"
}

func (a *app) browseInstallDir() {
	dlg := new(walk.FileDialog)
	dlg.Title = "Select install directory"
	dlg.InitialDirPath = a.installDir.Text()
	if ok, err := dlg.ShowBrowseFolder(a.mw); err != nil {
		a.showError(err)
	} else if ok {
		a.installDir.SetText(dlg.FilePath)
	}
}

func (a *app) browseAgentPath() {
	dlg := new(walk.FileDialog)
	dlg.Title = "Select agent.exe"
	dlg.FilePath = a.agentPath.Text()
	dlg.Filter = "Executable files (*.exe)|*.exe|All files (*.*)|*.*"
	if ok, err := dlg.ShowOpen(a.mw); err != nil {
		a.showError(err)
	} else if ok {
		a.agentPath.SetText(dlg.FilePath)
	}
}

func (a *app) relayURLListValues() []string {
	return append([]string(nil), a.relayURLs...)
}

func (a *app) setRelayURLList(values []string, selectIndex int) {
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

func (a *app) setRelayEditorFromIndex(index int) {
	if a.relayEdit == nil {
		return
	}
	if index >= 0 && index < len(a.relayURLs) {
		_ = a.relayEdit.SetText(a.relayURLs[index])
		return
	}
	_ = a.relayEdit.SetText("")
}

func (a *app) relaySelectionChanged() {
	index := -1
	if a.relayList != nil {
		index = a.relayList.CurrentIndex()
	}
	a.setRelayEditorFromIndex(index)
	a.updateRelayButtons()
}

func (a *app) updateRelayButtons() {
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

func (a *app) relayURLFromEditor() (string, error) {
	if a.relayEdit == nil {
		return "", errors.New("relay URL editor is not available")
	}
	value := strings.TrimSpace(a.relayEdit.Text())
	if value == "" {
		return "", errors.New("relay URL is required")
	}
	return value, nil
}

func (a *app) addRelayURL() {
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

func (a *app) updateRelayURL() {
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

func (a *app) deleteRelayURL() {
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

func (a *app) moveRelayURL(delta int) {
	if a.relayList == nil {
		return
	}
	index := a.relayList.CurrentIndex()
	a.moveRelayURLTo(index, index+delta)
}

func (a *app) moveRelayURLTo(from, to int) {
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

func (a *app) relayListMouseDown(x, y int, button walk.MouseButton) {
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

func (a *app) relayListMouseMove(_, y int, button walk.MouseButton) {
	if button&walk.LeftButton == 0 || a.relayDragIndex < 0 {
		return
	}
	if absInt(y-a.relayDragStartY) > 4 {
		a.relayDragging = true
	}
}

func (a *app) relayListMouseUp(x, y int, button walk.MouseButton) {
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

func (a *app) relayListIndexAt(x, y int) int {
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

func (a *app) runAction(action string) {
	opts := a.options()
	if action == "install" {
		if err := validateInstallInputs(opts); err != nil {
			a.showError(err)
			return
		}
	}
	go func() {
		if !isElevated() {
			if err := relaunchElevatedAction(action, opts, false); err != nil {
				a.appendLog("Elevation failed: %v", err)
				return
			}
			a.appendLog("Elevation requested for %s.", action)
			return
		}
		message, err := performAction(action, opts)
		if err != nil {
			a.appendLog("%s failed: %v", action, err)
		} else {
			a.appendLog("%s", message)
		}
		a.refreshStatus()
	}()
}

func (a *app) runSelfTest() {
	opts := a.options()
	installedExePath := installedPath(opts.InstallDir)
	exePath := installedExePath
	usingInstalledExe := fileExists(installedExePath)
	if !usingInstalledExe {
		exePath = opts.AgentPath
	}
	if exePath == "" || opts.RelayURL == "" {
		a.showError(errors.New("select an agent executable and enter at least one relay URL"))
		return
	}
	go func() {
		a.appendLog("Running self-test with relay URL(s): %s", strings.Join(splitRelayURLs(opts.RelayURL), ", "))
		a.appendLog("Agent executable: %s", exePath)
		if usingInstalledExe && !sameFileOrContent(installedExePath, opts.AgentPath) {
			a.appendLog("Installed agent.exe differs from the selected agent executable. Click Install / Update to copy the selected executable before testing it.")
		}
		args := []string{"-self-test", "-relay-url", opts.RelayURL}
		passwordFile := filepath.Join(opts.InstallDir, "room-password.dpapi")
		if fileExists(passwordFile) {
			args = append(args, "-room-password-file", passwordFile)
		}
		if opts.WinRMAddr != "" {
			args = append(args, "-winrm", opts.WinRMAddr)
		}
		if opts.SMBAddr != "" {
			args = append(args, "-smb", opts.SMBAddr)
		}
		cmd := exec.Command(exePath, args...)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		err := cmd.Run()
		text := strings.TrimSpace(output.String())
		if text != "" {
			a.appendLog("%s", text)
		}
		if err != nil {
			a.appendLog("Self-test failed: %v", err)
			return
		}
		a.appendLog("Self-test OK.")
	}()
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func sameFileOrContent(left, right string) bool {
	if sameFileOrPath(left, right) {
		return true
	}
	leftInfo, leftErr := os.Stat(strings.TrimSpace(left))
	rightInfo, rightErr := os.Stat(strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil || leftInfo.Size() != rightInfo.Size() {
		return false
	}
	leftHash, leftErr := fileHash(left)
	rightHash, rightErr := fileHash(right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}

func sameFileOrPath(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo) {
		return true
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return strings.EqualFold(left, right)
	}
	return strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
}

func fileHash(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

func (a *app) openInstallFolder() {
	dir := strings.TrimSpace(a.installDir.Text())
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0755)
	if err := shellOpen(dir); err != nil {
		a.showError(err)
	}
}

func (a *app) refreshStatus() {
	info, err := queryServiceInfo()
	text := "Status: "
	if err != nil {
		text += err.Error()
	} else if !info.Installed {
		text += "not installed"
	} else {
		text += serviceStateText(info.State)
		if info.ProcessID != 0 {
			text += fmt.Sprintf(" (pid %d)", info.ProcessID)
		}
	}
	if a.status != nil {
		a.mw.Synchronize(func() {
			a.status.SetText(text)
		})
	}
}

func (a *app) options() actionOptions {
	return actionOptions{
		InstallDir:        strings.TrimSpace(a.installDir.Text()),
		AgentPath:         strings.TrimSpace(a.agentPath.Text()),
		RelayURL:          joinRelayURLs(a.relayURLListValues()),
		RoomPassword:      a.roomPassword.Text(),
		ClearRoomPassword: a.clearPassword.Checked(),
		WinRMAddr:         strings.TrimSpace(a.winrmAddr.Text()),
		SMBAddr:           strings.TrimSpace(a.smbAddr.Text()),
		SMBAlias:          strings.TrimSpace(a.smbAlias.Text()),
	}
}

func (a *app) appendLog(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if a.log == nil {
		return
	}
	a.mw.Synchronize(func() {
		a.log.AppendText(time.Now().Format("15:04:05") + "  " + line + "\r\n")
	})
}

func (a *app) showError(err error) {
	if err == nil {
		return
	}
	walk.MsgBox(a.mw, appTitle(), err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func hasElevatedAction(args []string) bool {
	return hasArg(args, "-elevated-action")
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func runElevatedAction(args []string) {
	fs := flag.NewFlagSet("elevated-action", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	action := fs.String("elevated-action", "", "service action")
	installDir := fs.String("install-dir", "", "install directory")
	agentPath := fs.String("agent", "", "agent executable")
	relayURL := fs.String("relay-url", "", "relay URL")
	roomPasswordBlob := fs.String("room-password-blob", "", "DPAPI room password blob")
	clearRoomPassword := fs.Bool("clear-room-password", false, "clear room password")
	winrmAddr := fs.String("winrm", "", "WinRM target")
	smbAddr := fs.String("smb", "", "SMB target")
	smbAlias := fs.String("smb-alias", defaultSMBAlias, "SMB server alias")
	noDialog := fs.Bool("no-dialog", false, "write the result to the console instead of displaying a dialog")
	if err := fs.Parse(args); err != nil {
		reportElevatedResult(*noDialog, "", err)
		os.Exit(2)
	}
	message, err := performAction(*action, actionOptions{
		InstallDir:        *installDir,
		AgentPath:         *agentPath,
		RelayURL:          *relayURL,
		RoomPasswordBlob:  *roomPasswordBlob,
		ClearRoomPassword: *clearRoomPassword,
		WinRMAddr:         *winrmAddr,
		SMBAddr:           *smbAddr,
		SMBAlias:          *smbAlias,
	})
	if err != nil {
		reportElevatedResult(*noDialog, "", err)
		os.Exit(1)
	}
	reportElevatedResult(*noDialog, message, nil)
}

func reportElevatedResult(noDialog bool, message string, err error) {
	if noDialog {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stdout, message)
		}
		return
	}
	if err != nil {
		windowsMessageBox(appTitle(), err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		return
	}
	windowsMessageBox(appTitle(), message, windows.MB_OK|windows.MB_ICONINFORMATION)
}

type relayURLFlags []string

func (values *relayURLFlags) String() string {
	return joinRelayURLs(*values)
}

func (values *relayURLFlags) Set(value string) error {
	urls := splitRelayURLs(value)
	if len(urls) == 0 {
		return errors.New("relay URL cannot be empty")
	}
	*values = append(*values, urls...)
	return nil
}

func parseCLIArgs(args []string, stdin io.Reader) (string, actionOptions, error) {
	var opts actionOptions
	var relayURLs relayURLFlags
	fs := flag.NewFlagSet("deskferry-agent-configurator", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	action := fs.String("cli-action", "", "install, start, stop, restart, uninstall, or status")
	fs.StringVar(&opts.InstallDir, "install-dir", defaultInstallDir(), "agent installation directory")
	fs.StringVar(&opts.AgentPath, "agent", defaultAgentPath(), "work agent executable")
	fs.Var(&relayURLs, "relay-url", "relay room URL; repeat to configure multiple relays in priority order")
	passwordStdin := fs.Bool("room-password-stdin", false, "read the room password from standard input")
	fs.StringVar(&opts.RoomPasswordBlob, "room-password-blob", "", "machine-scope DPAPI password blob to consume")
	fs.BoolVar(&opts.ClearRoomPassword, "clear-room-password", false, "remove the stored room password")
	fs.StringVar(&opts.WinRMAddr, "winrm", "", "WinRM target in host:port form")
	fs.StringVar(&opts.SMBAddr, "smb", "", "SMB target in host:port form")
	fs.StringVar(&opts.SMBAlias, "smb-alias", defaultSMBAlias, "SMB server alias")
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
	case "install", "start", "stop", "restart", "uninstall", "status":
	default:
		return "", opts, fmt.Errorf("unknown CLI action %q", *action)
	}
	if *action != "install" {
		installOnly := map[string]bool{
			"agent": true, "relay-url": true, "room-password-stdin": true,
			"room-password-blob": true, "clear-room-password": true,
			"winrm": true, "smb": true, "smb-alias": true,
		}
		if *action != "uninstall" {
			installOnly["install-dir"] = true
		}
		var invalid string
		fs.Visit(func(option *flag.Flag) {
			if invalid == "" && installOnly[option.Name] {
				invalid = option.Name
			}
		})
		if invalid != "" {
			return "", opts, fmt.Errorf("-%s is only valid with -cli-action install", invalid)
		}
	}
	if *passwordStdin && opts.RoomPasswordBlob != "" {
		return "", opts, errors.New("-room-password-stdin and -room-password-blob cannot be used together")
	}
	if opts.ClearRoomPassword && (*passwordStdin || opts.RoomPasswordBlob != "") {
		return "", opts, errors.New("-clear-room-password cannot be combined with a password input")
	}
	if *passwordStdin {
		value, err := io.ReadAll(stdin)
		if err != nil {
			return "", opts, fmt.Errorf("read room password: %w", err)
		}
		opts.RoomPassword = strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
		if opts.RoomPassword == "" {
			return "", opts, errors.New("room password from standard input is empty")
		}
	}
	opts.RelayURL = joinRelayURLs(relayURLs)
	if *action == "install" {
		if err := validateInstallInputs(opts); err != nil {
			return "", opts, err
		}
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
		info, err := queryServiceInfo()
		if err != nil {
			return err
		}
		if !info.Installed {
			fmt.Fprintln(stdout, "DeskFerry Agent is not installed.")
			return nil
		}
		fmt.Fprintf(stdout, "DeskFerry Agent is installed: state=%s pid=%d\n", serviceStateText(info.State), info.ProcessID)
		return nil
	}
	if !isElevated() {
		if err := relaunchElevatedAction(action, opts, true); err != nil {
			return fmt.Errorf("request elevation: %w", err)
		}
		fmt.Fprintf(stdout, "Elevation requested for %s.\n", action)
		return nil
	}
	message, err := performAction(action, opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, message)
	return nil
}

func printCLIUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: deskferry-agent-configurator-windows-amd64.exe -cli-action ACTION [options]")
	fmt.Fprintln(output, "Actions: install, start, stop, restart, uninstall, status")
	fmt.Fprintln(output, "Install options:")
	fmt.Fprintln(output, "  -install-dir PATH          Agent installation directory")
	fmt.Fprintln(output, "  -agent PATH                Work agent executable")
	fmt.Fprintln(output, "  -relay-url URL             Relay room URL; repeat for multiple relays")
	fmt.Fprintln(output, "  -room-password-stdin       Read a new room password from standard input")
	fmt.Fprintln(output, "  -room-password-blob PATH   Consume a machine-scope DPAPI password blob")
	fmt.Fprintln(output, "  -clear-room-password       Remove the stored room password")
	fmt.Fprintln(output, "  -winrm HOST:PORT           WinRM target")
	fmt.Fprintln(output, "  -smb HOST:PORT             SMB target")
	fmt.Fprintln(output, "  -smb-alias NAME            SMB server alias (default deskferry-work)")
	fmt.Fprintln(output, "Omitting all password flags preserves the installed credential.")
}

func performAction(action string, opts actionOptions) (string, error) {
	switch action {
	case "install":
		return installOrUpdate(opts)
	case "start":
		return startService()
	case "stop":
		return stopService()
	case "restart":
		if _, err := stopService(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return "", err
		}
		return startService()
	case "uninstall":
		return uninstallService(opts.InstallDir)
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

func installOrUpdate(opts actionOptions) (string, error) {
	if err := validateInstallInputs(opts); err != nil {
		return "", err
	}
	installDir, err := filepath.Abs(opts.InstallDir)
	if err != nil {
		return "", err
	}
	agentDest := installedPath(installDir)
	passwordDest := filepath.Join(installDir, "room-password.dpapi")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", fmt.Errorf("create install directory: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return "", fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	existed := false
	if s, err := m.OpenService(serviceName); err == nil {
		existed = true
		if err := stopMgrService(s); err != nil {
			s.Close()
			return "", fmt.Errorf("stop existing service: %w", err)
		}
		s.Close()
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "", fmt.Errorf("open existing service: %w", err)
	}

	if err := copyFile(opts.AgentPath, agentDest); err != nil {
		return "", fmt.Errorf("copy agent executable: %w", err)
	}
	if opts.RoomPasswordBlob != "" {
		defer os.Remove(opts.RoomPasswordBlob)
		if err := copyFile(opts.RoomPasswordBlob, passwordDest); err != nil {
			return "", fmt.Errorf("install room password: %w", err)
		}
	} else if opts.ClearRoomPassword {
		_ = os.Remove(passwordDest)
	}
	if (opts.WinRMAddr != "" || opts.SMBAddr != "") && !fileExists(passwordDest) {
		return "", errors.New("WinRM and SMB require a room password")
	}
	args := []string{"-service", "-relay-url", opts.RelayURL}
	if fileExists(passwordDest) {
		args = append(args, "-room-password-file", passwordDest)
	}
	if opts.WinRMAddr != "" {
		args = append(args, "-winrm", opts.WinRMAddr)
	}
	if opts.SMBAddr != "" {
		args = append(args, "-smb", opts.SMBAddr)
	}
	if err := configureSMBAlias(installDir, opts.SMBAlias, opts.SMBAddr != ""); err != nil {
		return "", err
	}

	config := serviceConfig(agentDest, args)
	s, err := m.CreateService(serviceName, agentDest, config, args...)
	if err != nil {
		if !errors.Is(err, windows.ERROR_SERVICE_EXISTS) {
			return "", fmt.Errorf("create service: %w", err)
		}
		s, err = m.OpenService(serviceName)
		if err != nil {
			return "", fmt.Errorf("open service: %w", err)
		}
		if err := s.UpdateConfig(config); err != nil {
			s.Close()
			return "", fmt.Errorf("update service: %w", err)
		}
		existed = true
	}
	defer s.Close()

	_ = eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)

	if err := startMgrService(s); err != nil {
		return "", err
	}
	note := ""
	if opts.SMBAddr != "" {
		note = fmt.Sprintf(" SMB accepts the alias %s; restart Windows once if the Server service was already running.", opts.SMBAlias)
	}
	if existed {
		return fmt.Sprintf("Updated and started %s in %s.%s", serviceDisplayName, installDir, note), nil
	}
	return fmt.Sprintf("Installed and started %s in %s.%s", serviceDisplayName, installDir, note), nil
}

func startService() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return "", err
	}
	defer s.Close()
	if err := startMgrService(s); err != nil {
		return "", err
	}
	return fmt.Sprintf("Started %s.", serviceDisplayName), nil
}

func stopService() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return "", err
	}
	defer s.Close()
	if err := stopMgrService(s); err != nil {
		return "", err
	}
	return fmt.Sprintf("Stopped %s.", serviceDisplayName), nil
}

func uninstallService(installDir string) (string, error) {
	if strings.TrimSpace(installDir) == "" {
		installDir = defaultInstallDir()
	}
	if err := configureSMBAlias(installDir, "", false); err != nil {
		return "", err
	}
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Sprintf("%s is not installed.", serviceDisplayName), nil
		}
		return "", err
	}
	defer s.Close()
	_ = stopMgrService(s)
	if err := s.Delete(); err != nil {
		return "", err
	}
	_ = eventlog.Remove(serviceName)
	return fmt.Sprintf("Uninstalled %s. Installed files were left in place.", serviceDisplayName), nil
}

func validateInstallInputs(opts actionOptions) error {
	if opts.InstallDir == "" {
		return errors.New("install directory is required")
	}
	if opts.AgentPath == "" {
		return errors.New("agent executable is required")
	}
	if opts.RelayURL == "" {
		return errors.New("at least one relay URL is required")
	}
	if opts.WinRMAddr != "" {
		if _, _, err := net.SplitHostPort(opts.WinRMAddr); err != nil {
			return fmt.Errorf("WinRM target must be host:port: %w", err)
		}
	}
	if opts.SMBAddr != "" {
		if _, _, err := net.SplitHostPort(opts.SMBAddr); err != nil {
			return fmt.Errorf("SMB target must be host:port: %w", err)
		}
		if !validSMBAlias(opts.SMBAlias) {
			return errors.New("SMB server alias must be a single DNS label containing only letters, numbers, and hyphens")
		}
	}
	if _, err := os.Stat(opts.AgentPath); err != nil {
		return fmt.Errorf("agent executable is not readable: %w", err)
	}
	return nil
}

func serviceConfig(exePath string, args []string) mgr.Config {
	return mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: serviceBinaryPath(exePath, args),
		DisplayName:    serviceDisplayName,
		Description:    "Work-side RDP, WinRM, and SMB backend for DeskFerry.",
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

func startMgrService(s *mgr.Service) error {
	status, err := s.Query()
	if err == nil && status.State == svc.Running {
		return nil
	}
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForState(s, svc.Running, 20*time.Second)
}

func stopMgrService(s *mgr.Service) error {
	status, err := s.Query()
	if err == nil && status.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop service: %w", err)
	}
	return waitForState(s, svc.Stopped, 20*time.Second)
}

func waitForState(s *mgr.Service, want svc.State, timeout time.Duration) error {
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
			return fmt.Errorf("timed out waiting for service state %s, current state is %s", serviceStateText(uint32(want)), serviceStateText(uint32(status.State)))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func queryServiceInfo() (serviceInfo, error) {
	var info serviceInfo
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return info, fmt.Errorf("service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return info, err
	}
	handle, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return info, nil
		}
		return info, fmt.Errorf("service: %w", err)
	}
	defer windows.CloseServiceHandle(handle)

	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	err = windows.QueryServiceStatusEx(
		handle,
		windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)),
		uint32(unsafe.Sizeof(status)),
		&needed,
	)
	if err != nil {
		return info, err
	}
	info.Installed = true
	info.State = status.CurrentState
	info.ProcessID = status.ProcessId
	return info, nil
}

func relaunchElevatedAction(action string, opts actionOptions, noDialog bool) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"-elevated-action", action}
	if noDialog {
		args = append(args, "-no-dialog")
	}
	if opts.InstallDir != "" {
		args = append(args, "-install-dir", opts.InstallDir)
	}
	if opts.AgentPath != "" {
		args = append(args, "-agent", opts.AgentPath)
	}
	if opts.RelayURL != "" {
		args = append(args, "-relay-url", opts.RelayURL)
	}
	if opts.WinRMAddr != "" {
		args = append(args, "-winrm", opts.WinRMAddr)
	}
	if opts.SMBAddr != "" {
		args = append(args, "-smb", opts.SMBAddr)
	}
	if opts.SMBAlias != "" {
		args = append(args, "-smb-alias", opts.SMBAlias)
	}
	temporaryPasswordBlob := ""
	if opts.ClearRoomPassword {
		args = append(args, "-clear-room-password")
	} else if opts.RoomPassword != "" {
		blob, err := winsecret.ProtectMachine(opts.RoomPassword)
		if err != nil {
			return err
		}
		file, err := os.CreateTemp("", "deskferry-room-password-*.dpapi")
		if err != nil {
			return err
		}
		path := file.Name()
		if _, err := file.Write(blob); err != nil {
			file.Close()
			os.Remove(path)
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(path)
			return err
		}
		args = append(args, "-room-password-blob", path)
		temporaryPasswordBlob = path
	} else if opts.RoomPasswordBlob != "" {
		args = append(args, "-room-password-blob", opts.RoomPasswordBlob)
	}
	if err := shellExecute("runas", exePath, joinWindowsArgs(args), ""); err != nil {
		if temporaryPasswordBlob != "" {
			_ = os.Remove(temporaryPasswordBlob)
		}
		return err
	}
	return nil
}

func relaunchCurrentArgsElevated(args []string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	return shellExecute("runas", exePath, joinWindowsArgs(args), "")
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

func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func defaultInstallDir() string {
	if _, err := os.Stat(`D:\`); err == nil {
		return filepath.Join(`D:\`, "DeskFerry", "Agent")
	}
	if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
		return filepath.Join(programFiles, "DeskFerry", "Agent")
	}
	if systemDrive := os.Getenv("SystemDrive"); systemDrive != "" {
		return filepath.Join(systemDrive+`\`, "DeskFerry", "Agent")
	}
	return filepath.Join(`C:\`, "DeskFerry", "Agent")
}

func defaultAgentPath() string {
	exePath, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exePath)
	for _, name := range []string{"agent.exe", "deskferry-agent-windows-amd64.exe"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(dir, "deskferry-agent-windows-amd64.exe")
}

func installedPath(installDir string) string {
	return filepath.Join(installDir, installedAgentName)
}

func copyFile(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if strings.EqualFold(srcAbs, dstAbs) {
		return nil
	}
	in, err := os.Open(srcAbs)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstAbs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
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

func configureSMBAlias(installDir, alias string, enable bool) error {
	recordPath := filepath.Join(installDir, "smb-alias.txt")
	oldAlias := ""
	if data, err := os.ReadFile(recordPath); err == nil {
		oldAlias = strings.TrimSpace(string(data))
	}
	if !enable && oldAlias == "" {
		return nil
	}
	key, _, err := registry.CreateKey(
		registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\LanmanServer\Parameters`,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if err != nil {
		return fmt.Errorf("open SMB Server configuration: %w", err)
	}
	defer key.Close()

	aliases := readSMBOptionalNames(key)
	filtered := aliases[:0]
	for _, existing := range aliases {
		if oldAlias != "" && strings.EqualFold(existing, oldAlias) {
			continue
		}
		if alias != "" && strings.EqualFold(existing, alias) {
			continue
		}
		filtered = append(filtered, existing)
	}
	aliases = filtered
	if enable {
		alias = strings.TrimSpace(alias)
		aliases = append(aliases, alias)
		if err := key.SetDWordValue("DisableStrictNameChecking", 1); err != nil {
			return fmt.Errorf("allow the DeskFerry SMB alias: %w", err)
		}
	}
	if len(aliases) == 0 {
		if err := key.DeleteValue("OptionalNames"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("remove DeskFerry SMB alias: %w", err)
		}
	} else if err := key.SetStringsValue("OptionalNames", aliases); err != nil {
		return fmt.Errorf("save SMB server aliases: %w", err)
	}
	if enable {
		if err := os.WriteFile(recordPath, []byte(alias+"\n"), 0600); err != nil {
			return fmt.Errorf("save managed SMB alias: %w", err)
		}
	} else {
		_ = os.Remove(recordPath)
	}
	return nil
}

func readSMBOptionalNames(key registry.Key) []string {
	values, _, err := key.GetStringsValue("OptionalNames")
	if err == nil {
		return uniqueSMBAliases(values)
	}
	value, _, err := key.GetStringValue("OptionalNames")
	if err != nil {
		return nil
	}
	return uniqueSMBAliases(strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\r' || r == '\n'
	}))
}

func uniqueSMBAliases(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
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

func validSMBAlias(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func serviceStateText(state uint32) string {
	switch state {
	case windows.SERVICE_STOPPED:
		return "stopped"
	case windows.SERVICE_START_PENDING:
		return "start pending"
	case windows.SERVICE_STOP_PENDING:
		return "stop pending"
	case windows.SERVICE_RUNNING:
		return "running"
	case windows.SERVICE_CONTINUE_PENDING:
		return "continue pending"
	case windows.SERVICE_PAUSE_PENDING:
		return "pause pending"
	case windows.SERVICE_PAUSED:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func joinWindowsArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, syscall.EscapeArg(arg))
	}
	return strings.Join(quoted, " ")
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

func windowsMessageBox(title, text string, style uint32) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	_, _ = windows.MessageBox(0, textPtr, titlePtr, style)
}
