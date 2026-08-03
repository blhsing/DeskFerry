package tunnel

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestLatestConnGroupSupersedesOlderConnection(t *testing.T) {
	var group LatestConnGroup
	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	firstCtx, releaseFirst, replaced := group.Begin(context.Background(), first)
	if replaced {
		t.Fatal("first connection unexpectedly replaced another connection")
	}

	second, secondPeer := net.Pipe()
	defer secondPeer.Close()
	secondCtx, releaseSecond, replaced := group.Begin(context.Background(), second)
	defer releaseSecond()
	if !replaced {
		t.Fatal("second connection did not replace the first")
	}
	select {
	case <-firstCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("first connection context was not canceled")
	}

	// Releasing an already superseded connection must not clear the current one.
	releaseFirst()
	third, thirdPeer := net.Pipe()
	defer thirdPeer.Close()
	thirdCtx, releaseThird, replaced := group.Begin(context.Background(), third)
	defer releaseThird()
	if !replaced {
		t.Fatal("third connection did not replace the current second connection")
	}
	select {
	case <-secondCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("second connection context was not canceled")
	}

	group.Close()
	select {
	case <-thirdCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("current connection context was not canceled by Close")
	}
}
