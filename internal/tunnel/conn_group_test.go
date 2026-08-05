package tunnel

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestConnGroupKeepsConnectionsIndependentUntilClose(t *testing.T) {
	var group ConnGroup
	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	firstCtx, releaseFirst := group.Begin(context.Background(), first)
	defer releaseFirst()

	second, secondPeer := net.Pipe()
	defer secondPeer.Close()
	secondCtx, releaseSecond := group.Begin(context.Background(), second)
	defer releaseSecond()
	select {
	case <-firstCtx.Done():
		t.Fatal("second connection canceled the first")
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-firstCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("released connection context was not canceled")
	}
	select {
	case <-secondCtx.Done():
		t.Fatal("releasing the first connection canceled the second")
	case <-time.After(50 * time.Millisecond):
	}

	group.Close()
	select {
	case <-secondCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("active connection context was not canceled by Close")
	}
}

func TestConnGroupRejectsConnectionAfterClose(t *testing.T) {
	var group ConnGroup
	group.Close()
	conn, peer := net.Pipe()
	defer peer.Close()
	ctx, release := group.Begin(context.Background(), conn)
	defer release()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("connection registered after Close was not canceled")
	}
}
