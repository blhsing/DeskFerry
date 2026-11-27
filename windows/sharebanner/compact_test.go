//go:build windows

package sharebanner

import (
	"testing"

	"deskferry/internal/screenview"
)

func TestCompactLayoutSizes(t *testing.T) {
	viewing := &banner{dpi: 96, compact: true, view: describe(screenview.SessionState{ClientKind: "viewer", Live: true})}
	l := viewing.computeCompactLayout()
	if l.width != 104 || l.height != 32 {
		t.Fatalf("viewing: %dx%d; want 104x32", l.width, l.height)
	}
	if l.buttons[buttonStopControl].Right != 0 {
		t.Fatal("Stop control is shown without remote control")
	}
	control := &banner{dpi: 96, compact: true, view: describe(screenview.SessionState{ClientKind: "agent", ControlActive: true, Live: true})}
	l = control.computeCompactLayout()
	if l.width != 132 {
		t.Fatalf("control: width %d; want 132", l.width)
	}
	for _, index := range []int{buttonStopControl, buttonStopSharing, buttonToggle} {
		rect := l.buttons[index]
		if rect.Right-rect.Left != 24 || rect.Bottom-rect.Top != 24 || rect.Top != 4 {
			t.Fatalf("button %d is %+v; want 24x24 at 4 from the top", index, rect)
		}
	}
	// At 150% everything scales.
	scaled := &banner{dpi: 144, compact: true, view: viewing.view}
	if l := scaled.computeCompactLayout(); l.height != 48 || l.width != 156 {
		t.Fatalf("at 150%%: %dx%d; want 156x48", l.width, l.height)
	}
}

func TestControlStartingExpandsTheCompactBanner(t *testing.T) {
	b := &banner{dpi: 96, compact: true, view: describe(screenview.SessionState{ClientKind: "viewer", Live: true}), rendering: true}
	b.pending, b.hasPending = screenview.SessionState{ClientKind: "viewer", Live: true, ControlActive: true}, true
	b.applyPending()
	if b.compact {
		t.Fatal("the banner stayed compact when remote control started")
	}
	b.compact = true
	b.pending, b.hasPending = screenview.SessionState{ClientKind: "viewer", Live: true, ControlActive: true}, true
	b.applyPending()
	if !b.compact {
		t.Fatal("an unchanged state expanded a banner the user minimized")
	}
}
