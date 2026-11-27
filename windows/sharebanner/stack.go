//go:build windows

package sharebanner

import (
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
)

// Several screen sessions (a person and an AI agent, say) each show their own
// banner, from their own helper process. Banners the user has not moved stack
// from the top of the work area in the order they appeared, so none covers
// another; each banner records that order, and whether it was moved, in
// window properties the other banners read.

var (
	procFindWindowExW = user32.NewProc("FindWindowExW")
	procGetPropW      = user32.NewProc("GetPropW")
	procSetPropW      = user32.NewProc("SetPropW")
	procRemovePropW   = user32.NewProc("RemovePropW")

	propOrder = syscall.StringToUTF16Ptr("DeskFerryShareBannerOrder")
	propMoved = syscall.StringToUTF16Ptr("DeskFerryShareBannerMoved")
)

// stackedTop returns the top edge for a banner of width x height centred at
// centerX: top, or just below the earlier banners it would otherwise overlap,
// keeping gap between them.
func stackedTop(centerX, top, width, height, gap int32, earlier []win.RECT) int32 {
	left := centerX - width/2
	right := left + width
	y := top
	for moved := true; moved; {
		moved = false
		for _, other := range earlier {
			if other.Left < right && other.Right > left && other.Top < y+height+gap && other.Bottom+gap > y {
				y = other.Bottom + gap
				moved = true
			}
		}
	}
	return y
}

// markOrder records when this banner appeared.
func (b *banner) markOrder() {
	procSetPropW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(propOrder)), uintptr(time.Now().UnixNano()))
}

// markMoved records that the user dragged this banner, so unmoved banners keep
// clear of it wherever it is.
func (b *banner) markMoved() {
	procSetPropW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(propMoved)), 1)
}

func (b *banner) clearProps() {
	procRemovePropW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(propOrder)))
	procRemovePropW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(propMoved)))
}

// otherBanners returns the visible banners this one must keep clear of: those
// that appeared before it, and any the user moved.
func (b *banner) otherBanners() []win.RECT {
	mine, _, _ := procGetPropW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(propOrder)))
	var rects []win.RECT
	var previous uintptr
	for {
		hwnd, _, _ := procFindWindowExW.Call(0, previous, uintptr(unsafe.Pointer(classNamePtr)), 0)
		if hwnd == 0 {
			return rects
		}
		previous = hwnd
		if win.HWND(hwnd) == b.hwnd || !win.IsWindowVisible(win.HWND(hwnd)) {
			continue
		}
		order, _, _ := procGetPropW.Call(hwnd, uintptr(unsafe.Pointer(propOrder)))
		moved, _, _ := procGetPropW.Call(hwnd, uintptr(unsafe.Pointer(propMoved)))
		earlier := order != 0 && (order < mine || (order == mine && hwnd < uintptr(b.hwnd)))
		if !earlier && moved == 0 {
			continue
		}
		var rect win.RECT
		if win.GetWindowRect(win.HWND(hwnd), &rect) {
			rects = append(rects, rect)
		}
	}
}

// restack moves a banner the user has not moved to its place in the stack,
// for example up after a banner above it closed.
func (b *banner) restack() {
	if b.userMoved || b.dragging {
		return
	}
	oldX, oldY := b.anchorX, b.anchorY
	b.clampAnchor()
	if b.anchorX != oldX || b.anchorY != oldY {
		x, y := b.origin()
		win.SetWindowPos(b.hwnd, 0, x, y, 0, 0, win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE)
	}
}
