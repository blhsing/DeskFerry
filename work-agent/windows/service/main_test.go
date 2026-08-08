package main

import (
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestOrderedActiveSessionIDsPrefersActiveConsoleThenRemoteSessions(t *testing.T) {
	sessions := []windows.WTS_SESSION_INFO{
		{SessionID: 1, State: windows.WTSDisconnected},
		{SessionID: 4, State: windows.WTSActive},
		{SessionID: 7, State: windows.WTSActive},
	}
	if got, want := orderedActiveSessionIDs(1, sessions), []uint32{4, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disconnected console ordering = %v, want %v", got, want)
	}
	sessions[0].State = windows.WTSActive
	if got, want := orderedActiveSessionIDs(1, sessions), []uint32{1, 4, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active console ordering = %v, want %v", got, want)
	}
}

func TestOrderedDisconnectedSessionIDsPrefersConsoleThenRemoteSessions(t *testing.T) {
	sessions := []windows.WTS_SESSION_INFO{
		{SessionID: 7, State: windows.WTSDisconnected},
		{SessionID: 4, State: windows.WTSActive},
		{SessionID: 1, State: windows.WTSDisconnected},
		{SessionID: 9, State: windows.WTSListen},
	}
	if got, want := orderedDisconnectedSessionIDs(1, sessions), []uint32{1, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disconnected session ordering = %v, want %v", got, want)
	}
	if got, want := orderedDisconnectedSessionIDs(0xffffffff, sessions), []uint32{7, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disconnected ordering without a console = %v, want %v", got, want)
	}
}

func TestSplitRelayURLs(t *testing.T) {
	got := splitRelayURLs(" https://test-officialwebsite.azurewebsites.net/relay/workdesk;\nhttp://217.142.228.117/relay/workdesk, ws://localhost:8000/relay/dev ")
	want := []string{
		"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
		"http://217.142.228.117/relay/workdesk",
		"ws://localhost:8000/relay/dev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitRelayURLs() = %#v, want %#v", got, want)
	}
}

func TestRelayURLFlagAccumulatesValues(t *testing.T) {
	var flag relayURLFlag
	if err := flag.Set("https://one.example/relay/a;https://two.example/relay/a"); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("https://three.example/relay/a"); err != nil {
		t.Fatal(err)
	}
	want := "https://one.example/relay/a;https://two.example/relay/a;https://three.example/relay/a"
	if got := flag.String(); got != want {
		t.Fatalf("relayURLFlag.String() = %q, want %q", got, want)
	}
}

func TestLoadConfigAcceptsMultipleWebSocketRelayURLs(t *testing.T) {
	cfg, err := loadConfig("https://test-officialwebsite.azurewebsites.net/relay/workdesk;http://217.142.228.117/relay/workdesk", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
		"http://217.142.228.117/relay/workdesk",
	}
	if got := cfg.relayAddresses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayAddresses() = %#v, want %#v", got, want)
	}
	if cfg.RelayAddr != want[0] {
		t.Fatalf("RelayAddr = %q, want first relay %q", cfg.RelayAddr, want[0])
	}
}

func TestConfigFileRelayAddrsAreNormalized(t *testing.T) {
	cfg := config{
		RelayAddrs: []string{
			"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
			"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
			"http://217.142.228.117/relay/workdesk",
		},
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://test-officialwebsite.azurewebsites.net/relay/workdesk",
		"http://217.142.228.117/relay/workdesk",
	}
	if got := cfg.relayAddresses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayAddresses() = %#v, want %#v", got, want)
	}
}

func TestLoadOrCreateAgentIDPersists(t *testing.T) {
	t.Setenv("ProgramData", t.TempDir())
	t.Setenv("APPDATA", "")

	first, err := loadOrCreateAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("agent id length = %d, want 32", len(first))
	}

	second, err := loadOrCreateAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second agent id = %q, want %q", second, first)
	}
}

func TestCleanAgentIdentity(t *testing.T) {
	got := cleanAgentIdentity(" unit/agent:slot!* ")
	if got != "unitagentslot" {
		t.Fatalf("cleanAgentIdentity() = %q", got)
	}
}

func TestReconnectDelayBacksOffDialFailures(t *testing.T) {
	minBackoff := time.Second
	maxBackoff := time.Minute

	delay, next := reconnectDelay(minBackoff, minBackoff, maxBackoff, false)
	if delay != time.Second || next != 2*time.Second {
		t.Fatalf("first failure delay, next = %s, %s; want 1s, 2s", delay, next)
	}

	delay, next = reconnectDelay(maxBackoff, minBackoff, maxBackoff, false)
	if delay != time.Minute || next != time.Minute {
		t.Fatalf("capped failure delay, next = %s, %s; want 1m, 1m", delay, next)
	}
}

func TestReconnectDelayResetsAfterConnection(t *testing.T) {
	minBackoff := time.Second
	maxBackoff := time.Minute

	delay, next := reconnectDelay(maxBackoff, minBackoff, maxBackoff, true)
	if delay != time.Second || next != time.Second {
		t.Fatalf("connected delay, next = %s, %s; want 1s, 1s", delay, next)
	}
}
