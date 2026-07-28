//go:build windows

package main

import (
	"reflect"
	"testing"
)

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
