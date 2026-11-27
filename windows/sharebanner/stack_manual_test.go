//go:build windows

package sharebanner

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/win"

	"deskferry/internal/screenview"
)

// TestManualBannersDoNotOverlap shows a person's banner and an AI agent's
// banner on this PC for a few seconds and checks that they do not overlap.
// Set DESKFERRY_MANUAL_BANNERS=1 to run it.
func TestManualBannersDoNotOverlap(t *testing.T) {
	if os.Getenv("DESKFERRY_MANUAL_BANNERS") == "" {
		t.Skip("set DESKFERRY_MANUAL_BANNERS=1 to show two banners on this PC")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	show := func(state screenview.SessionState) chan screenview.SessionState {
		states := make(chan screenview.SessionState, 1)
		states <- state
		go func() {
			runtime.LockOSThread()
			Run(ctx, states, Actions{StopControl: func() {}, StopSharing: func() {}})
		}()
		return states
	}
	person := show(screenview.SessionState{ClientName: "m50-ben_hsing", ClientKind: "viewer", ControlActive: true, Live: true})
	time.Sleep(time.Second)
	agent := show(screenview.SessionState{ClientName: "DeskFerry agent", ClientKind: "agent", ControlActive: true, Live: true})
	time.Sleep(3 * time.Second)
	var rects []win.RECT
	var previous uintptr
	for {
		hwnd, _, _ := procFindWindowExW.Call(0, previous, uintptr(unsafe.Pointer(classNamePtr)), 0)
		if hwnd == 0 {
			break
		}
		previous = hwnd
		var rect win.RECT
		if win.IsWindowVisible(win.HWND(hwnd)) && win.GetWindowRect(win.HWND(hwnd), &rect) {
			rects = append(rects, rect)
		}
	}
	t.Logf("banners: %+v", rects)
	if len(rects) != 2 {
		t.Fatalf("%d banners visible; want 2", len(rects))
	}
	a, b := rects[0], rects[1]
	if a.Left < b.Right && b.Left < a.Right && a.Top < b.Bottom && b.Top < a.Bottom {
		t.Fatal("the banners overlap")
	}
	// Closing the first banner moves the second up within the 2-second check.
	person <- screenview.SessionState{Ended: true}
	time.Sleep(3 * time.Second)
	previous = 0
	for {
		hwnd, _, _ := procFindWindowExW.Call(0, previous, uintptr(unsafe.Pointer(classNamePtr)), 0)
		if hwnd == 0 {
			break
		}
		previous = hwnd
		var rect win.RECT
		if win.IsWindowVisible(win.HWND(hwnd)) && win.GetWindowRect(win.HWND(hwnd), &rect) {
			t.Logf("remaining banner: %+v", rect)
			if rect.Top != min(a.Top, b.Top) {
				t.Fatalf("the remaining banner stayed at %d; want it back at the top (%d)", rect.Top, min(a.Top, b.Top))
			}
		}
	}
	agent <- screenview.SessionState{Ended: true}
	time.Sleep(500 * time.Millisecond)
}
