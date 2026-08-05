//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
