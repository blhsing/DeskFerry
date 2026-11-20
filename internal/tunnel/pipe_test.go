package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
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
		{name: "clean local EOF first", result: PipeResult{FirstCompleted: "a"}, want: "local_rdp"},
		{name: "clean relay EOF first", result: PipeResult{FirstCompleted: "b"}, want: "relay"},
		{name: "relay EOF before secondary local error", result: PipeResult{FirstCompleted: "b", AToB: CopyResult{CopyErr: errors.New("local reset")}}, want: "relay"},
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

type failingReadConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *failingReadConn) Read([]byte) (int, error) {
	return 0, errors.New("relay session could not resume")
}
func (c *failingReadConn) Write(p []byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *failingReadConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

// A broken stream must not leave the local TCP connection half-open until its
// peer, here one that never sends or closes, finally hangs up.
func TestPipeClosesLocalConnectionWhenStreamFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	local, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	stream := &failingReadConn{closed: make(chan struct{})}
	done := make(chan PipeResult, 1)
	go func() { done <- PipeWithResult(stream, local) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe kept the local connection open after the stream failed")
	}
}
