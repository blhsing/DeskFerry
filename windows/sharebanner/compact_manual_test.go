//go:build windows

package sharebanner

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/win"

	"deskferry/internal/screenview"
)

// TestManualCompactBanners shows a person's and an AI agent's banner on this
// PC, minimizes both, then starts remote control for the person, saving a
// screenshot of the banner area after each step to
// DESKFERRY_MANUAL_BANNER_SHOTS (a folder). Set it to run the test.
func TestManualCompactBanners(t *testing.T) {
	dir := os.Getenv("DESKFERRY_MANUAL_BANNER_SHOTS")
	if dir == "" {
		t.Skip("set DESKFERRY_MANUAL_BANNER_SHOTS to a folder to show banners on this PC")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	show := func(state screenview.SessionState) chan screenview.SessionState {
		states := make(chan screenview.SessionState, 4)
		states <- state
		go func() {
			runtime.LockOSThread()
			Run(ctx, states, Actions{StopControl: func() {}, StopSharing: func() {}})
		}()
		return states
	}
	person := show(screenview.SessionState{ClientName: "m50-ben_hsing", ClientKind: "viewer", Live: true})
	time.Sleep(700 * time.Millisecond)
	agent := show(screenview.SessionState{ClientName: "DeskFerry agent", ClientKind: "agent", ControlActive: true, Live: true})
	time.Sleep(2500 * time.Millisecond)
	shot(t, filepath.Join(dir, "1-full.png"))

	banners.Range(func(key, value any) bool {
		b := value.(*banner)
		r := b.layout.buttons[buttonToggle]
		point := uintptr(uint16((r.Left+r.Right)/2)) | uintptr(uint16((r.Top+r.Bottom)/2))<<16
		win.PostMessage(b.hwnd, win.WM_LBUTTONDOWN, 1, point)
		win.PostMessage(b.hwnd, win.WM_LBUTTONUP, 0, point)
		return true
	})
	time.Sleep(2500 * time.Millisecond)
	shot(t, filepath.Join(dir, "2-compact.png"))

	person <- screenview.SessionState{ClientName: "m50-ben_hsing", ClientKind: "viewer", Live: true, ControlActive: true}
	time.Sleep(2500 * time.Millisecond)
	shot(t, filepath.Join(dir, "3-person-took-control.png"))
	person <- screenview.SessionState{Ended: true}
	agent <- screenview.SessionState{Ended: true}
	time.Sleep(500 * time.Millisecond)
}

// shot saves the top-centre of the primary work area, where banners stack.
func shot(t *testing.T, path string) {
	t.Helper()
	work := primaryWorkArea()
	width, height := int32(1000), int32(160)
	left := (work.Left+work.Right)/2 - width/2
	screen := win.GetDC(0)
	defer win.ReleaseDC(0, screen)
	mem := win.CreateCompatibleDC(screen)
	defer win.DeleteDC(mem)
	header := win.BITMAPINFOHEADER{BiWidth: width, BiHeight: -height, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	header.BiSize = uint32(unsafe.Sizeof(header))
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(screen, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	old := win.SelectObject(mem, win.HGDIOBJ(bitmap))
	defer win.SelectObject(mem, old)
	const captureBlt = 0x40000000
	win.BitBlt(mem, 0, 0, width, height, screen, left, work.Top, win.SRCCOPY|captureBlt)
	pixels := unsafe.Slice((*byte)(bits), int(width*height*4))
	img := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	for i := 0; i+3 < len(pixels); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pixels[i+2], pixels[i+1], pixels[i], 255
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
	var rects []win.RECT
	banners.Range(func(key, value any) bool {
		var rect win.RECT
		win.GetWindowRect(value.(*banner).hwnd, &rect)
		rects = append(rects, rect)
		return true
	})
	t.Logf("%s: banners %+v", filepath.Base(path), rects)
}
