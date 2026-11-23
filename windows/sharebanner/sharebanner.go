//go:build windows

// Package sharebanner shows the always-on-top banner on the Work PC while its
// screen is shared through DeskFerry. The banner tells the person at the PC
// who is viewing the screen, whether that is a person or an AI agent, whether
// the remote side can use the mouse and keyboard, and lets them stop it.
//
// The banner is one custom-painted layered window that follows
// docs/design-system.md through the tokens in windows/uikit. It never takes
// the keyboard focus and has no taskbar button.
package sharebanner

import (
	"context"
	"runtime"

	"deskferry/internal/screenview"
)

// Actions are the banner's buttons.
type Actions struct {
	// StopControl ends remote control for the session; viewing continues.
	StopControl func()
	// StopSharing ends the screen session.
	StopSharing func()
}

// Run shows the banner and updates it from states until ctx is done or a
// state with Ended arrives. It blocks; call it from its own goroutine. When
// the banner window cannot be created (for example on a secure desktop), Run
// still waits for ctx or Ended before it returns.
func Run(ctx context.Context, states <-chan screenview.SessionState, actions Actions) {
	// A window belongs to the OS thread that created it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Start from the newest state that is already waiting, so the banner
	// opens with the right text and never opens for a session that is over.
	initial, ended := latestQueued(ctx, states)
	if ended {
		return
	}
	b, err := newBanner(actions, initial)
	if err != nil {
		waitForEnd(ctx, states)
		return
	}
	stop := make(chan struct{})
	finished := make(chan bool, 1)
	go func() { finished <- forward(ctx, states, stop, b.offer, b.requestClose) }()
	b.messageLoop()
	close(stop)
	if ended := <-finished; !ended {
		// The window went away before the session ended.
		waitForEnd(ctx, states)
	}
}

// forward hands every state to offer and calls end once when ctx is done, a
// state with Ended arrives, or states is closed; it then returns true. It
// returns false when stop is closed first.
func forward(ctx context.Context, states <-chan screenview.SessionState, stop <-chan struct{}, offer func(screenview.SessionState), end func()) bool {
	for {
		select {
		case <-stop:
			return false
		case <-ctx.Done():
			end()
			return true
		case state, ok := <-states:
			if !ok || state.Ended {
				end()
				return true
			}
			offer(state)
		}
	}
}

// latestQueued returns the newest state that can be read without waiting.
// ended reports that ctx is done, a state with Ended was queued, or states is
// closed.
func latestQueued(ctx context.Context, states <-chan screenview.SessionState) (latest screenview.SessionState, ended bool) {
	for {
		select {
		case <-ctx.Done():
			return latest, true
		case state, ok := <-states:
			if !ok || state.Ended {
				return latest, true
			}
			latest = state
		default:
			return latest, false
		}
	}
}

// waitForEnd blocks until ctx is done, a state with Ended arrives, or states
// is closed.
func waitForEnd(ctx context.Context, states <-chan screenview.SessionState) {
	forward(ctx, states, nil, func(screenview.SessionState) {}, func() {})
}
