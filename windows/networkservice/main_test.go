//go:build windows

package homenetworkservice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"deskferry/internal/homenetwork"
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

func TestHandleSOCKSRefusesUDPAssociateQuietly(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- handleSOCKS(context.Background(), server, homenetwork.Config{Alias: "deskferry-work", RemoteAddress: "198.18.0.2"})
	}()
	if _, err := client.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil || reply[1] != 0 {
		t.Fatalf("method reply=%v err=%v", reply, err)
	}
	if _, err := client.Write([]byte{5, 3, 0, 1}); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 10)
	if _, err := io.ReadFull(client, result); err != nil || result[1] != 7 {
		t.Fatalf("UDP ASSOCIATE reply=%v err=%v", result, err)
	}
	if err := <-done; !errors.Is(err, errSOCKSUDPAssociate) {
		t.Fatalf("handleSOCKS error = %v, want errSOCKSUDPAssociate", err)
	}
}

func TestAdapterMetricArgsUseLowestPriority(t *testing.T) {
	got := strings.Join(adapterMetricArgs("ipv4", "DeskFerry"), " ")
	if got != "interface ipv4 set interface interface=DeskFerry metric=9999" {
		t.Fatalf("metric args = %q", got)
	}
}
