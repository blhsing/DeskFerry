//go:build windows

package homewindows

import (
	"testing"

	"deskferry/internal/tunnel"
)

func TestTCPForwardListenersFollowSelectedProfileUnlessOverridden(t *testing.T) {
	cfg := config{
		RelayAddrs:          []string{"https://relay.example/relay/workdesk"},
		SelectedDestination: "Work",
		Destinations: []destinationProfile{
			{Name: "Home lab", RelayAddrs: []string{"https://relay.example/relay/lab"}, TCPForwards: "2222=22"},
			{Name: "Work", RelayAddrs: []string{"https://relay.example/relay/workdesk"}, TCPForwards: " 15432=5432 ;13389"},
		},
	}
	if err := cfg.ensureDestinations(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Destinations[1].TCPForwards; got != "15432=5432;13389=13389" {
		t.Fatalf("normalized forwards = %q", got)
	}
	forwards, err := cfg.tcpForwardListeners()
	if err != nil || len(forwards) != 2 || forwards[0].Service() != "tcp-5432" {
		t.Fatalf("selected forwards = %+v, %v", forwards, err)
	}
	cfg.TCPForwards = "8080=80"
	if forwards, _ := cfg.tcpForwardListeners(); len(forwards) != 1 || forwards[0].LocalPort != 8080 {
		t.Fatalf("override forwards = %+v", forwards)
	}
	if cloned := cloneDestinations(cfg.Destinations); cloned[0].TCPForwards != "2222=22" {
		t.Fatalf("clone lost forwards: %+v", cloned[0])
	}
	cfg.Destinations[0].TCPForwards = "0=1"
	if err := cfg.ensureDestinations(); err == nil {
		t.Fatal("invalid profile forward was accepted")
	}
}

func TestTCPForwardsRequireServiceEchoAndLoopback(t *testing.T) {
	if !requiresServiceEcho(tunnel.TCPForwardService(5432)) || !requiresServiceEcho(tunnel.ServiceScreen) || requiresServiceEcho(tunnel.ServiceRDP) {
		t.Fatal("service echo classification is wrong")
	}
	if got := tcpForwardAddress(15432); got != "127.0.0.1:15432" {
		t.Fatalf("listener address = %q", got)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	cfg := config{Destinations: []destinationProfile{{Name: "Work", TCPForwards: "15432=5432"}}, SelectedDestination: "Work"}
	if opened := openTCPForwards(cfg, logf); len(opened) != 0 || len(logged) != 1 {
		t.Fatalf("forwards opened without a room password: %d %v", len(opened), logged)
	}
}
