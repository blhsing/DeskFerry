//go:build windows

package homenetworkservice

import (
	"bytes"
	"testing"
)

func TestReadSOCKSIPv4Address(t *testing.T) {
	got, err := readSOCKSAddress(bytes.NewReader([]byte{198, 18, 0, 2}), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "198.18.0.2" {
		t.Fatalf("got %q", got)
	}
}

func TestReadSOCKSDomainAddress(t *testing.T) {
	got, err := readSOCKSAddress(bytes.NewReader(append([]byte{14}, []byte("deskferry-work")...)), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != "deskferry-work" {
		t.Fatalf("got %q", got)
	}
}

func TestTunDeviceSpecUsesTunDriver(t *testing.T) {
	if got := tunDeviceSpec("DeskFerry"); got != "tun://DeskFerry" {
		t.Fatalf("device spec = %q", got)
	}
}
