//go:build windows

package sharebanner

import (
	"context"
	"testing"
	"time"

	"deskferry/internal/screenview"
)

func TestForwardDeliversStatesUntilEnded(t *testing.T) {
	states := make(chan screenview.SessionState, 4)
	states <- screenview.SessionState{ClientName: "one", Live: true}
	states <- screenview.SessionState{ClientName: "two", Live: true, ControlActive: true}
	states <- screenview.SessionState{Ended: true}
	states <- screenview.SessionState{ClientName: "after the end"}

	var offered []string
	ends := 0
	ended := forward(context.Background(), states, nil,
		func(state screenview.SessionState) { offered = append(offered, state.ClientName) },
		func() { ends++ })
	if !ended || ends != 1 {
		t.Fatalf("forward ended=%v ends=%d, want true and 1", ended, ends)
	}
	if len(offered) != 2 || offered[0] != "one" || offered[1] != "two" {
		t.Fatalf("offered = %v, want [one two]", offered)
	}
}

func TestForwardEndsOnCancelCloseAndStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ends := 0
	if !forward(ctx, make(chan screenview.SessionState), nil, func(screenview.SessionState) {}, func() { ends++ }) || ends != 1 {
		t.Fatalf("a cancelled context must end the banner once, ends=%d", ends)
	}

	closed := make(chan screenview.SessionState)
	close(closed)
	ends = 0
	if !forward(context.Background(), closed, nil, func(screenview.SessionState) {}, func() { ends++ }) || ends != 1 {
		t.Fatalf("a closed channel must end the banner once, ends=%d", ends)
	}

	stop := make(chan struct{})
	close(stop)
	ends = 0
	if forward(context.Background(), make(chan screenview.SessionState), stop, func(screenview.SessionState) {}, func() { ends++ }) || ends != 0 {
		t.Fatalf("stop must return without ending, ends=%d", ends)
	}
}

func TestLatestQueued(t *testing.T) {
	states := make(chan screenview.SessionState, 4)
	if state, ended := latestQueued(context.Background(), states); ended || state != (screenview.SessionState{}) {
		t.Fatalf("empty queue = %+v ended=%v, want the zero state and false", state, ended)
	}
	states <- screenview.SessionState{ClientName: "one"}
	states <- screenview.SessionState{ClientName: "two", ControlActive: true}
	if state, ended := latestQueued(context.Background(), states); ended || state.ClientName != "two" || !state.ControlActive {
		t.Fatalf("queued states = %+v ended=%v, want the newest and false", state, ended)
	}
	states <- screenview.SessionState{ClientName: "three"}
	states <- screenview.SessionState{Ended: true}
	if _, ended := latestQueued(context.Background(), states); !ended {
		t.Fatal("a queued Ended state must report ended")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ended := latestQueued(ctx, make(chan screenview.SessionState)); !ended {
		t.Fatal("a cancelled context must report ended")
	}
}

// runReturns runs the banner (it appears briefly when a desktop is available
// and waits silently otherwise) and reports whether Run returned.
func runReturns(t *testing.T, ctx context.Context, states chan screenview.SessionState, trigger func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, states, Actions{})
	}()
	select {
	case <-done:
		t.Fatal("Run returned before the session ended")
	case <-time.After(150 * time.Millisecond):
	}
	trigger()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the session ended")
	}
}

func TestRunReturnsWhenEnded(t *testing.T) {
	states := make(chan screenview.SessionState, 8)
	states <- screenview.SessionState{ClientName: "HOME-PC", ClientKind: "viewer", Live: true}
	states <- screenview.SessionState{ClientName: "HOME-PC", ClientKind: "agent", Live: true, ControlActive: true}
	runReturns(t, context.Background(), states, func() { states <- screenview.SessionState{Ended: true} })
}

func TestRunReturnsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runReturns(t, ctx, make(chan screenview.SessionState), cancel)
}
