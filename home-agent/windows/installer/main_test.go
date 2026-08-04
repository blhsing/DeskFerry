//go:build windows

package main

import (
	"strings"
	"testing"
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
