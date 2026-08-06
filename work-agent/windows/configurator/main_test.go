//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
)

func TestValidSMBAlias(t *testing.T) {
	for _, value := range []string{"deskferry-work", "WORK10", "work-2"} {
		if !validSMBAlias(value) {
			t.Errorf("validSMBAlias(%q) = false", value)
		}
	}
	for _, value := range []string{"", "-work", "work-", `work\host`, "work host"} {
		if validSMBAlias(value) {
			t.Errorf("validSMBAlias(%q) = true", value)
		}
	}
}

func TestUniqueSMBAliasesPreservesUnrelatedValues(t *testing.T) {
	got := uniqueSMBAliases([]string{"files", "DeskFerry-Work", "FILES", "archive"})
	if len(got) != 3 || got[0] != "files" || got[1] != "DeskFerry-Work" || got[2] != "archive" {
		t.Fatalf("aliases = %#v", got)
	}
}

func TestParseCLIInstall(t *testing.T) {
	exePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	action, opts, err := parseCLIArgs([]string{
		"-cli-action", "install",
		"-install-dir", `C:\DeskFerry\Agent`,
		"-agent", exePath,
		"-relay-url", "https://relay.example/relay/h",
		"-relay-url", "http://relay-backup.example/relay/h",
		"-room-password-stdin",
		"-winrm", "127.0.0.1:5985",
		"-smb", "127.0.0.1:445",
		"-smb-alias", "deskferry-work",
	}, strings.NewReader("\uFEFF\uFEFFsecret\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if action != "install" {
		t.Fatalf("action = %q", action)
	}
	if opts.RelayURL != "https://relay.example/relay/h;http://relay-backup.example/relay/h" {
		t.Fatalf("RelayURL = %q", opts.RelayURL)
	}
	if opts.RoomPassword != "secret" {
		t.Fatalf("RoomPassword = %q", opts.RoomPassword)
	}
	if opts.WinRMAddr != "127.0.0.1:5985" || opts.SMBAddr != "127.0.0.1:445" || opts.SMBAlias != "deskferry-work" {
		t.Fatalf("service options = %#v", opts)
	}
}

func TestParseCLIInstallComposesRelayBasesWithRoom(t *testing.T) {
	exePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, opts, err := parseCLIArgs([]string{
		"-cli-action", "install",
		"-agent", exePath,
		"-room", "office",
		"-relay-base-url", "https://primary.example/relay/",
		"-relay-base-url", "http://fallback.example/relay",
	}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://primary.example/relay/office;http://fallback.example/relay/office"
	if opts.RelayURL != want {
		t.Fatalf("RelayURL = %q, want %q", opts.RelayURL, want)
	}
}

func TestParseCLIRejectsPasswordConflicts(t *testing.T) {
	_, _, err := parseCLIArgs([]string{
		"-cli-action", "install",
		"-room-password-stdin",
		"-room-password-blob", `C:\temp\password.dpapi`,
	}, strings.NewReader("secret\n"))
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseCLIStatusNeedsNoInstallOptions(t *testing.T) {
	action, _, err := parseCLIArgs([]string{"-cli-action", "status"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if action != "status" {
		t.Fatalf("action = %q", action)
	}
}

func TestParseCLIRejectsInstallOptionsForServiceActions(t *testing.T) {
	_, _, err := parseCLIArgs([]string{
		"-cli-action", "restart",
		"-room-password-stdin",
	}, strings.NewReader("secret\n"))
	if err == nil || !strings.Contains(err.Error(), "only valid with -cli-action install") {
		t.Fatalf("error = %v", err)
	}
}
