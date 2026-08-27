//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
)

func TestRelayStatusClientReusesBoundedConnection(t *testing.T) {
	var newConnections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay/status" || r.URL.Query().Get("room") != "test-room" {
			t.Fatalf("unexpected relay status request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"DeskFerry.Relay","time":"2026-08-22T00:00:00Z","rooms":[{"id":"test-room","waiting_agents":1}]}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	cfg := config{RelayAddr: server.URL + "/relay/test-room", Proxy: "direct"}
	client := httpClient(cfg)
	defer client.CloseIdleConnections()
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		summary, err := queryRelaySummaryWithClient(ctx, cfg, client)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !summary.WorkOnline || summary.Room != "test-room" {
			t.Fatalf("unexpected relay summary: %#v", summary)
		}
	}
	if got := newConnections.Load(); got != 1 {
		t.Fatalf("status polling opened %d connections, want one reused connection", got)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.IdleConnTimeout <= 0 || transport.MaxConnsPerHost <= 0 {
		t.Fatalf("status client transport is not bounded: %#v", client.Transport)
	}
}

func TestEncodePowerShellCommandUsesUTF16LE(t *testing.T) {
	encoded := encodePowerShellCommand("Write-Output 'ready'")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("encoded command has odd UTF-16 byte count: %d", len(raw))
	}
	var decoded strings.Builder
	for index := 0; index < len(raw); index += 2 {
		decoded.WriteRune(rune(binary.LittleEndian.Uint16(raw[index:])))
	}
	if got := decoded.String(); got != "Write-Output 'ready'" {
		t.Fatalf("decoded command = %q", got)
	}
}

func TestWinRMSessionKeyChangesWithCredentialAndDestination(t *testing.T) {
	base := winRMSessionKey("Room b", `DOMAIN\\user`, "3391", "password-one")
	if base == winRMSessionKey("Room b", `DOMAIN\\user`, "3391", "password-two") {
		t.Fatal("session key did not change with password")
	}
	if base == winRMSessionKey("Room h", `DOMAIN\\user`, "3391", "password-one") {
		t.Fatal("session key did not change with destination")
	}
	if strings.Contains(base, "password-one") {
		t.Fatal("session key contains the plaintext password")
	}
}

func TestReadCLIWinRMCommand(t *testing.T) {
	command, err := readCLIWinRMCommand("", "-", strings.NewReader("Get-Date\n"))
	if err != nil || command != "Get-Date\n" {
		t.Fatalf("command = %q, err=%v", command, err)
	}
	if _, err := readCLIWinRMCommand("Get-Date", "-", bytes.NewReader(nil)); err == nil {
		t.Fatal("expected mutually exclusive command inputs to fail")
	}
}

func TestSelectDestinationConfig(t *testing.T) {
	cfg := config{
		ListenAddr:      defaultListenAddr,
		WinRMListenAddr: defaultWinRMListenAddr,
		Proxy:           "direct",
		Destinations: []destinationProfile{
			{Name: "Office", RelayAddrs: []string{"https://one.example/relay/office"}, RoomProof: "office-proof", WindowsUser: `OFFICE\\owner`},
			{Name: "Home", RelayAddrs: []string{"https://two.example/relay/home"}, RoomProof: "home-proof", WindowsUser: `HOME\\owner`},
		},
		SelectedDestination: "Office",
	}
	cfg.setRelayAddresses(cfg.Destinations[0].RelayAddrs)
	if err := selectDestinationConfig(&cfg, "home"); err != nil {
		t.Fatal(err)
	}
	if cfg.SelectedDestination != "Home" || cfg.RDPUser != `HOME\\owner` || cfg.RoomProof != "home-proof" || cfg.primaryRelayAddress() != "https://two.example/relay/home" {
		t.Fatalf("selected config = %#v", cfg)
	}
}

func TestReadHomeInstallMetadata(t *testing.T) {
	programData := t.TempDir()
	installDir := t.TempDir()
	t.Setenv("ProgramData", programData)
	if err := os.MkdirAll(filepath.Join(programData, "DeskFerry"), 0755); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(homeInstallMetadata{InstallDir: installDir, Destination: "Office", Alias: "office-files", EnableNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programData, "DeskFerry", "home-install.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := readHomeInstallMetadata()
	if !ok || got.InstallDir != installDir || got.Destination != "Office" || got.Alias != "office-files" {
		t.Fatalf("installed metadata = %#v, ok=%t", got, ok)
	}
}

func TestWinRMSessionWorkerProtocol(t *testing.T) {
	manager := newWinRMSessionManager(time.Minute)
	defer manager.Close()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := manager.ensureWorkerLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.stdin.Write([]byte("{\"id\":7,\"action\":\"ping\"}\n")); err != nil {
		t.Fatal(err)
	}
	line, err := manager.stdout.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response winRMResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &response); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if response.ID != 7 || !response.OK || response.Output != "ready" {
		t.Fatalf("unexpected worker response: %#v", response)
	}
}

func TestDecodeWinRMResponseText(t *testing.T) {
	response := winRMResponse{
		OutputBase64: base64.StdEncoding.EncodeToString([]byte("HKLM:\\SOFTWARE\r\nline two")),
		ErrorBase64:  base64.StdEncoding.EncodeToString([]byte("remote error: path C:\\Windows")),
	}
	if err := decodeWinRMResponseText(&response); err != nil {
		t.Fatal(err)
	}
	if response.Output != "HKLM:\\SOFTWARE\r\nline two" || response.Error != "remote error: path C:\\Windows" {
		t.Fatalf("decoded response = %#v", response)
	}
	response.OutputBase64 = "not-base64"
	if err := decodeWinRMResponseText(&response); err == nil {
		t.Fatal("invalid response base64 was accepted")
	}
}

func TestWinRMSessionWorkerAcceptsBlankPassword(t *testing.T) {
	manager := newWinRMSessionManager(time.Minute)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := manager.Execute(ctx, "blank-password-test", "Owner", "", "$env:COMPUTERNAME", "1")
	if err == nil {
		t.Fatal("WinRM connection to closed test port unexpectedly succeeded")
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "converttosecurestring") || strings.Contains(message, "empty string") {
		t.Fatalf("blank password was rejected before WinRM authentication: %v", err)
	}
}

func BenchmarkLiveWinRMSession(b *testing.B) {
	if os.Getenv("DESKFERRY_LIVE_WINRM_BENCHMARK") != "1" {
		b.Skip("set DESKFERRY_LIVE_WINRM_BENCHMARK=1 to benchmark the configured live WinRM tunnel")
	}
	cfg, err := loadConfig("", "", "")
	if err != nil {
		b.Fatal(err)
	}
	user, password, err := readWindowsCredential(cfg)
	if err != nil {
		b.Fatal(err)
	}
	_, port, err := net.SplitHostPort(cfg.WinRMListenAddr)
	if err != nil {
		b.Fatal(err)
	}
	manager := newWinRMSessionManager(time.Minute)
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	response, err := manager.Execute(ctx, cfg.SelectedDestination, user, password, "[Environment]::MachineName", port)
	if err != nil {
		b.Fatal(err)
	}
	if response.Reused {
		b.Fatal("cold command unexpectedly reused a session")
	}
	coldMilliseconds := float64(time.Since(started).Microseconds()) / 1000
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		response, err = manager.Execute(ctx, cfg.SelectedDestination, user, password, "[Environment]::MachineName", port)
		if err != nil {
			b.Fatal(err)
		}
		if !response.Reused {
			b.Fatal("warm command did not reuse the session")
		}
	}
	b.ReportMetric(coldMilliseconds, "cold-ms")
}

func TestAcquireNamedInstanceMutexRejectsSecondInstance(t *testing.T) {
	name := fmt.Sprintf(`Global\DeskFerryHomeAgentTest-%d`, os.Getpid())
	first, alreadyRunning, err := acquireNamedInstanceMutex(name)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyRunning {
		t.Fatal("first mutex acquisition reported an existing instance")
	}
	defer windows.CloseHandle(first)

	second, alreadyRunning, err := acquireNamedInstanceMutex(name)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		windows.CloseHandle(second)
		t.Fatal("second mutex acquisition returned an owned handle")
	}
	if !alreadyRunning {
		t.Fatal("second mutex acquisition did not report the existing instance")
	}
}

func TestHiddenCommandSuppressesConsoleWindow(t *testing.T) {
	cmd := hiddenCommand("cmdkey.exe", "/list")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("hidden command process attributes = %#v", cmd.SysProcAttr)
	}
}

func TestQualifyLocalWindowsUser(t *testing.T) {
	got, err := qualifyLocalWindowsUser(" DESKTOP-G2EEM1V\r\n", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if got != `DESKTOP-G2EEM1V\Owner` {
		t.Fatalf("qualified user = %q", got)
	}
	for _, alreadyQualified := range []string{`DOMAIN\Owner`, `owner@example.com`} {
		got, err = qualifyLocalWindowsUser("ignored", alreadyQualified)
		if err != nil || got != alreadyQualified {
			t.Fatalf("qualified user %q = %q, %v", alreadyQualified, got, err)
		}
	}
	if _, err := qualifyLocalWindowsUser("bad host", "Owner"); err == nil {
		t.Fatal("invalid computer name was accepted")
	}
}

func TestMSTSCProfileContentPasswordlessRDP(t *testing.T) {
	cfg := config{
		ListenAddr:          "127.0.0.1:3390",
		RDPUser:             `DESKTOP-G2EEM1V\Owner`,
		SelectedDestination: "Work",
		Destinations: []destinationProfile{{
			Name: "Work", PasswordlessRDP: true,
		}},
	}
	content := mstscProfileContent(cfg)
	for _, expected := range []string{
		"autoreconnection enabled:i:1",
		"connection type:i:6",
		"networkautodetect:i:0",
		"bandwidthautodetect:i:0",
		"authentication level:i:0",
		"enablecredsspsupport:i:0",
		`username:s:DESKTOP-G2EEM1V\Owner`,
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("passwordless RDP profile missing %q:\n%s", expected, content)
		}
	}
	cfg.Destinations[0].PasswordlessRDP = false
	content = mstscProfileContent(cfg)
	for _, expected := range []string{"authentication level:i:2", "enablecredsspsupport:i:1"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("NLA RDP profile missing %q:\n%s", expected, content)
		}
	}
}

func TestEnsureDestinationsMigratesLegacyRelayList(t *testing.T) {
	cfg := config{
		RelayAddr: "https://primary.example/relay/office",
		RelayAddrs: []string{
			"https://primary.example/relay/office",
			"http://fallback.example/relay/office",
		},
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if cfg.SelectedDestination != "Work" || len(cfg.Destinations) != 1 {
		t.Fatalf("unexpected migrated destinations: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Destinations[0].RelayAddrs, cfg.RelayAddrs) {
		t.Fatalf("migrated relay URLs = %#v, want %#v", cfg.Destinations[0].RelayAddrs, cfg.RelayAddrs)
	}
	if got, want := cfg.Destinations[0].RelayBases, []string{"https://primary.example/relay", "http://fallback.example/relay"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated relay bases = %#v, want %#v", got, want)
	}
	if cfg.Destinations[0].Room != "office" {
		t.Fatalf("migrated room = %q, want office", cfg.Destinations[0].Room)
	}
}

func TestEnsureDestinationsKeepsNonSelectedProfiles(t *testing.T) {
	cfg := config{
		RelayAddr:           "https://azure.example/relay/home",
		RelayAddrs:          []string{"https://azure.example/relay/home"},
		SelectedDestination: "Home",
		Destinations: []destinationProfile{
			{Name: "Office", RelayAddrs: []string{"https://azure.example/relay/office"}},
			{Name: "Home", RelayAddrs: []string{"https://old.example/relay/home"}},
		},
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Destinations[0].RelayAddrs[0]; got != "https://azure.example/relay/office" {
		t.Fatalf("non-selected profile changed to %q", got)
	}
	if got := cfg.Destinations[1].RelayAddrs[0]; got != cfg.RelayAddrs[0] {
		t.Fatalf("selected profile relay = %q, want %q", got, cfg.RelayAddrs[0])
	}
}

func TestEnsureDestinationsMigratesAndKeepsSharedWindowsUsersPerProfile(t *testing.T) {
	cfg := config{
		RelayAddr:           "https://azure.example/relay/home",
		RelayAddrs:          []string{"https://azure.example/relay/home"},
		RDPUser:             `HOME\owner`,
		SelectedDestination: "Home",
		Destinations: []destinationProfile{
			{Name: "Office", RelayAddrs: []string{"https://azure.example/relay/office"}, WindowsUser: `DOMAIN\worker`},
			{Name: "Home", RelayAddrs: []string{"https://old.example/relay/home"}},
		},
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Destinations[0].WindowsUser; got != `DOMAIN\worker` {
		t.Fatalf("office Windows user = %q", got)
	}
	if got := cfg.Destinations[1].WindowsUser; got != `HOME\owner` {
		t.Fatalf("migrated home Windows user = %q", got)
	}
	if got := cfg.RDPUser; got != `HOME\owner` {
		t.Fatalf("selected shared Windows user = %q", got)
	}
}

func TestEnsureDestinationsKeepsSMBAliasPerProfile(t *testing.T) {
	cfg := config{
		RelayAddr:           "https://azure.example/relay/home",
		SelectedDestination: "Home",
		Destinations: []destinationProfile{
			{Name: "Office", RelayAddrs: []string{"https://azure.example/relay/office"}, SMBAlias: "office-files"},
			{Name: "Home", RelayAddrs: []string{"https://azure.example/relay/home"}, SMBAlias: "home-files"},
		},
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if cfg.Destinations[0].SMBAlias != "office-files" || cfg.Destinations[1].SMBAlias != "home-files" {
		t.Fatalf("SMB aliases changed: %#v", cfg.Destinations)
	}
	if got := selectedSMBAlias(cfg); got != "home-files" {
		t.Fatalf("selected SMB alias = %q", got)
	}
}

func TestSlicesEqualFold(t *testing.T) {
	if !slicesEqualFold([]string{" HTTPS://Relay/Room "}, []string{"https://relay/room"}) {
		t.Fatal("equivalent relay lists did not compare equal")
	}
	if slicesEqualFold([]string{"a", "b"}, []string{"b", "a"}) {
		t.Fatal("ordered relay lists compared equal after reordering")
	}
}

func TestSelectedSMBProfileSyncSkipsMatchingActiveProfile(t *testing.T) {
	programData := t.TempDir()
	t.Setenv("ProgramData", programData)
	if err := os.MkdirAll(filepath.Join(programData, "DeskFerry"), 0755); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(homeInstallMetadata{
		InstallDir: `C:\Program Files\DeskFerry Home`, Destination: "Office",
		RelayAddrs: []string{"https://relay.example/relay/office"}, Proxy: "direct",
		Alias: "office-files", EnableNetwork: true,
	})
	if err := os.WriteFile(filepath.Join(programData, "DeskFerry", "home-install.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		Proxy: "direct", SelectedDestination: "Office",
		Destinations: []destinationProfile{{
			Name: "Office", RelayAddrs: []string{"https://relay.example/relay/office"},
			RoomProof: "proof", SMBAlias: "office-files",
		}},
	}
	requested, err := requestSelectedSMBProfileSync(cfg, false)
	if err != nil || requested {
		t.Fatalf("requested=%t error=%v", requested, err)
	}
}

func TestEnsureDestinationsMigratesLegacyWinRMUser(t *testing.T) {
	cfg := config{
		RelayAddr: "https://azure.example/relay/work",
		WinRMUser: `DOMAIN\legacy`,
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Destinations[0].WindowsUser; got != `DOMAIN\legacy` {
		t.Fatalf("migrated WinRM user = %q", got)
	}
}

func TestSMBCredentialTargetFromMetadata(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "invalid", data: `{`, want: ""},
		{name: "network disabled", data: `{"alias":"deskferry-work","enable_network":false}`, want: ""},
		{name: "default alias", data: `{"enable_network":true}`, want: "deskferry-work"},
		{name: "custom alias", data: `{"alias":" work-files ","enable_network":true}`, want: "work-files"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := smbCredentialTargetFromMetadata([]byte(test.data)); got != test.want {
				t.Fatalf("target = %q, want %q", got, test.want)
			}
		})
	}
}

func TestScreenViewerStartupBoundsMaximizesLargeRemoteScreen(t *testing.T) {
	work := walk.Rectangle{X: 100, Y: 50, Width: 1920, Height: 1040}
	bounds, maximize := screenViewerStartupBounds(
		3840, 1080,
		walk.Rectangle{Width: 1920, Height: 1040},
		walk.Rectangle{Width: 1900, Height: 950},
		work,
		walk.Size{Width: 720, Height: 500},
	)
	if !maximize {
		t.Fatal("large remote screen should maximize the viewer")
	}
	if bounds != work {
		t.Fatalf("bounds = %#v, want work area %#v", bounds, work)
	}
}

func TestScreenViewerStartupBoundsFitsAndCentersSmallRemoteScreen(t *testing.T) {
	work := walk.Rectangle{X: 100, Y: 50, Width: 1920, Height: 1040}
	bounds, maximize := screenViewerStartupBounds(
		1280, 720,
		walk.Rectangle{Width: 1920, Height: 1040},
		walk.Rectangle{Width: 1900, Height: 950},
		work,
		walk.Size{Width: 720, Height: 500},
	)
	if maximize {
		t.Fatal("small remote screen should use a fitted viewer window")
	}
	want := walk.Rectangle{X: 410, Y: 165, Width: 1300, Height: 810}
	if bounds != want {
		t.Fatalf("bounds = %#v, want %#v", bounds, want)
	}
}
