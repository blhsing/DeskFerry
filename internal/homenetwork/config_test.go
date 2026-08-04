package homenetwork

import "testing"

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := (Config{
		RelayAddrs: []string{"https://relay.example/relay/office"},
		RoomProof:  "proof",
	}).WithDefaults(`C:\Program Files\DeskFerry Home`)
	if cfg.Alias != DefaultAlias || cfg.RemoteAddress != DefaultRemoteAddress {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsDifferentRooms(t *testing.T) {
	cfg := (Config{
		RelayAddrs: []string{"https://relay.example/relay/a", "https://relay.example/relay/b"},
		RoomProof:  "proof",
	}).WithDefaults(`C:\DeskFerry`)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected different relay rooms to be rejected")
	}
}

func TestConfigRequiresProof(t *testing.T) {
	cfg := (Config{RelayAddrs: []string{"https://relay.example/relay/office"}}).WithDefaults(`C:\DeskFerry`)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an empty room proof to be rejected")
	}
}
