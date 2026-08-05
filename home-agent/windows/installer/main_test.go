//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deskferry/internal/homenetwork"
)

func TestRemoveManagedHostsBlock(t *testing.T) {
	input := "127.0.0.1 localhost\r\n" + hostsBeginMarker + "\r\n198.18.0.2 old-name\r\n" + hostsEndMarker + "\r\n10.0.0.1 intranet\r\n"
	got := removeManagedHostsBlock(input)
	if strings.Contains(got, "old-name") || !strings.Contains(got, "localhost") || !strings.Contains(got, "intranet") {
		t.Fatalf("unexpected cleaned hosts file: %q", got)
	}
}

func TestNetworkConfigUsesRoomScopedProof(t *testing.T) {
	opts := setupOptions{
		InstallDir:   `C:\Program Files\DeskFerry Home`,
		RelayAddrs:   []string{"https://relay.example/relay/office"},
		RoomPassword: "secret",
		Alias:        "deskferry-work",
	}
	cfg := networkConfig(opts)
	if cfg.RoomProof == "" || strings.Contains(cfg.RoomProof, opts.RoomPassword) {
		t.Fatalf("unexpected room proof %q", cfg.RoomProof)
	}
}

func TestNetworkConfigPreservesExistingProof(t *testing.T) {
	opts := setupOptions{
		InstallDir: `C:\Program Files\DeskFerry Home`,
		RelayAddrs: []string{"https://relay.example/relay/office"},
		RoomProof:  "existing-proof",
		Alias:      "deskferry-work",
	}
	if got := networkConfig(opts).RoomProof; got != opts.RoomProof {
		t.Fatalf("RoomProof = %q", got)
	}
}

func TestParseCLIInstall(t *testing.T) {
	sourceDir := t.TempDir()
	for _, name := range []string{"DeskFerryHomeSetup.exe", "DeskFerryHome.exe", "DeskFerryHomeNetwork.exe", "tun2socks.exe", "wintun.dll", "LICENSE-Wintun.txt", "LICENSE-tun2socks.txt"} {
		if err := os.WriteFile(filepath.Join(sourceDir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	action, opts, err := parseCLIArgs([]string{
		"-cli-action", "configure",
		"-install-dir", filepath.Join(t.TempDir(), "Home"),
		"-source-dir", sourceDir,
		"-destination", "Room h",
		"-relay-url", "https://relay.example/relay/h",
		"-relay-url", "http://relay-backup.example/relay/h",
		"-proxy", "direct",
		"-alias", "deskferry-work",
		"-enable-network=true",
		"-room-password-stdin",
	}, strings.NewReader("\uFEFF\uFEFFsecret\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if action != "configure" || opts.Destination != "Room h" || opts.RoomPassword != "secret" || len(opts.RelayAddrs) != 2 || opts.Proxy != "direct" || !opts.EnableNetwork {
		t.Fatalf("action=%q options=%#v", action, opts)
	}
}

func TestParseCLIRejectsInstallOptionsForStatus(t *testing.T) {
	_, _, err := parseCLIArgs([]string{"-cli-action", "status", "-enable-network=true"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "only valid with install or configure") {
		t.Fatalf("error = %v", err)
	}
}

func TestExistingSetupOptionsLoadsInstalledNetworkConfig(t *testing.T) {
	programData := t.TempDir()
	appData := t.TempDir()
	t.Setenv("ProgramData", programData)
	t.Setenv("APPDATA", appData)
	installDir := filepath.Join(t.TempDir(), "DeskFerry Home")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "DeskFerryHome.exe"), []byte("home"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(installMetadataPath()), 0755); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]string{"install_dir": installDir})
	if err := os.WriteFile(installMetadataPath(), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := (homenetwork.Config{
		RelayAddrs: []string{"https://relay.example/relay/h"},
		Proxy:      "direct",
		RoomProof:  "saved-proof",
		Alias:      "deskferry-work",
	}).WithDefaults(installDir)
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	opts := existingSetupOptions(t.TempDir())
	if opts.InstallDir != installDir || !opts.EnableNetwork || opts.RoomProof != "saved-proof" || opts.Proxy != "direct" {
		t.Fatalf("options = %#v", opts)
	}
}

func TestConfigureHomeClientUpdatesSelectedDestination(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)
	settings := homeClientSettings{
		ListenAddr:          "127.0.0.1:3390",
		Proxy:               "env",
		WinRMListenAddr:     "127.0.0.1:3391",
		WinRMUser:           `DOMAIN\legacy`,
		SelectedDestination: "Room a",
		Destinations: []homeClientDestination{
			{Name: "Room a", RelayAddrs: []string{"https://relay.example/relay/a"}, RoomProof: "old-proof", WindowsUser: `DOMAIN\user`},
			{Name: "Other", RelayAddrs: []string{"https://relay.example/relay/other"}, RoomProof: "other-proof"},
		},
	}
	data, _ := json.Marshal(settings)
	if err := os.MkdirAll(filepath.Dir(homeClientSettingsPath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeClientSettingsPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	opts := setupOptions{
		Destination: "Room b",
		RelayAddrs: []string{
			"https://relay.example/relay/b",
			"http://relay-backup.example/relay/b",
		},
		Proxy: "direct",
	}
	if err := configureHomeClient(opts, "room-b-proof"); err != nil {
		t.Fatal(err)
	}
	var got homeClientSettings
	written, err := os.ReadFile(homeClientSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatal(err)
	}
	if got.SelectedDestination != "Room b" || got.RoomProof != "room-b-proof" || got.Proxy != "direct" || got.WinRMUser != settings.WinRMUser {
		t.Fatalf("settings = %#v", got)
	}
	if len(got.Destinations) != 3 || got.Destinations[2].RoomProof != "room-b-proof" || got.Destinations[0].WindowsUser != `DOMAIN\user` {
		t.Fatalf("destinations = %#v", got.Destinations)
	}
}
