package tunnel

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestPipeWithResultReportsBothDirections(t *testing.T) {
	a, peerA := tcpPair(t)
	b, peerB := tcpPair(t)
	defer peerA.Close()
	defer peerB.Close()

	done := make(chan PipeResult, 1)
	go func() { done <- PipeWithResult(a, b) }()

	assertTransferred(t, peerA, peerB, []byte("from-a"))
	assertTransferred(t, peerB, peerA, []byte("from-b"))
	if err := peerA.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := peerB.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-done:
		if result.AToB.Bytes != int64(len("from-a")) || result.BToA.Bytes != int64(len("from-b")) {
			t.Fatalf("unexpected byte counts: %+v", result)
		}
		if result.AToB.CopyErr != nil || result.BToA.CopyErr != nil {
			t.Fatalf("unexpected copy errors: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PipeWithResult did not finish")
	}
}

func TestPipeEndInitiator(t *testing.T) {
	tests := []struct {
		name   string
		result PipeResult
		want   string
	}{
		{name: "local", result: PipeResult{AToB: CopyResult{CopyErr: errors.New("local closed")}}, want: "local_rdp"},
		{name: "relay", result: PipeResult{BToA: CopyResult{CopyErr: errors.New("relay closed")}}, want: "relay"},
		{name: "both", result: PipeResult{AToB: CopyResult{CopyErr: errors.New("a")}, BToA: CopyResult{CopyErr: errors.New("b")}}, want: "both"},
		{name: "clean", result: PipeResult{}, want: "clean_or_unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.EndInitiator("local_rdp", "relay"); got != test.want {
				t.Fatalf("EndInitiator = %q, want %q", got, test.want)
			}
		})
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return <-accepted, peer
}

func assertTransferred(t *testing.T, source, destination net.Conn, payload []byte) {
	t.Helper()
	if _, err := source.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(destination, received); err != nil {
		t.Fatal(err)
	}
	if string(received) != string(payload) {
		t.Fatalf("received %q, want %q", received, payload)
	}
}
