package workservice

import (
	"sync/atomic"
	"testing"
	"time"
)

type fakeResumable struct {
	interrupted bool
	aborted     atomic.Bool
	done        chan struct{}
}

func (f *fakeResumable) Interrupted() bool { return f.interrupted }
func (f *fakeResumable) Abort(error) {
	if f.aborted.CompareAndSwap(false, true) {
		close(f.done)
	}
}

func TestNewRDPSessionEndsOnlyInterruptedSessions(t *testing.T) {
	interrupted := &fakeResumable{interrupted: true, done: make(chan struct{})}
	healthy := &fakeResumable{done: make(chan struct{})}
	interruptedDone := trackRDPSession("interrupted", interrupted)
	healthyDone := trackRDPSession("healthy", healthy)
	go func() { <-interrupted.done; close(interruptedDone) }()
	defer close(healthyDone)

	started := time.Now()
	abortInterruptedRDPSessions("new")
	if !interrupted.aborted.Load() {
		t.Fatal("interrupted RDP session was not ended")
	}
	if healthy.aborted.Load() {
		t.Fatal("connected RDP session was ended")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("waited %s although the old session closed at once", elapsed)
	}
}
