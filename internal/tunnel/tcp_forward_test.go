package tunnel

import (
	"testing"
	"time"
)

func TestParseTCPForwards(t *testing.T) {
	forwards, err := ParseTCPForwards(" 15432=db01:5432; 13389=[fe80::1]:3389,\n2222=host.example:22 ")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTCPForwards(forwards); got != "15432=db01:5432;13389=[fe80::1]:3389;2222=host.example:22" {
		t.Fatalf("formatted forwards = %q", got)
	}
	if forwards[0].Service() != "tcp-15432" {
		t.Fatalf("service = %q", forwards[0].Service())
	}
	if empty, err := ParseTCPForwards(""); err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, %v", empty, err)
	}
	for _, invalid := range []string{"15432", "0=db:1", "70000=db:1", "1=db", "1=:22", "1=db:0", "1=db:1;1=other:2", "x=db:1"} {
		if _, err := ParseTCPForwards(invalid); err == nil {
			t.Fatalf("ParseTCPForwards(%q) succeeded", invalid)
		}
	}
}

func TestTCPForwardServiceNames(t *testing.T) {
	for service, want := range map[string]int{"tcp-1": 1, "TCP-65535": 65535, "tcp-15432": 15432} {
		if got, ok := ParseTCPForwardService(service); !ok || got != want {
			t.Fatalf("ParseTCPForwardService(%q) = %d, %t", service, got, ok)
		}
	}
	for _, service := range []string{"tcp-", "tcp-0", "tcp-01", "tcp-65536", "tcp-1a", "tcp--1", "udp-1", "rdp"} {
		if _, ok := ParseTCPForwardService(service); ok {
			t.Fatalf("ParseTCPForwardService(%q) accepted", service)
		}
	}
	if !IsKnownService("tcp-22") || !IsKnownService(ServiceSMB) || IsKnownService("ftp") {
		t.Fatal("IsKnownService classification is wrong")
	}
}

func TestParseTCPForwardListeners(t *testing.T) {
	listeners, err := ParseTCPForwardListeners("15432=5432; 2222\n13389=3389")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTCPForwardListeners(listeners); got != "15432=5432;2222=2222;13389=3389" {
		t.Fatalf("formatted listeners = %q", got)
	}
	if listeners[0].Service() != "tcp-5432" {
		t.Fatalf("service = %q", listeners[0].Service())
	}
	for _, invalid := range []string{"0", "1=0", "1=x", "1;1=2", "70000"} {
		if _, err := ParseTCPForwardListeners(invalid); err == nil {
			t.Fatalf("ParseTCPForwardListeners(%q) succeeded", invalid)
		}
	}
}

func TestValidateSessionOfferAcceptsTCPForward(t *testing.T) {
	now := time.Now().UTC()
	offer := ControlMessage{Type: MessageSessionOffer, SessionID: "0123456789abcdef0123456789abcdef", Room: "workdesk", Service: "tcp-15432", CreatedAt: now, ExpiresAt: now.Add(8 * time.Second), ProtocolVersion: ProtocolVersion2}
	if err := ValidateSessionOffer(offer, "workdesk", "agent-1", now); err != nil {
		t.Fatal(err)
	}
	offer.Service = "tcp-0"
	if err := ValidateSessionOffer(offer, "workdesk", "agent-1", now); err == nil {
		t.Fatal("invalid TCP forward service was accepted")
	}
}
