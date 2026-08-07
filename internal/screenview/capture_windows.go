package screenview

import (
	"errors"
	"fmt"
	"image"
	"os"
	"time"
	"unsafe"

	"github.com/lxn/win"

	"deskferry/internal/buildinfo"
)

// CaptureDesktop captures the complete Windows virtual desktop from the
// interactive session in which the process is running.
func CaptureDesktop() (*image.RGBA, error) {
	x := win.GetSystemMetrics(win.SM_XVIRTUALSCREEN)
	y := win.GetSystemMetrics(win.SM_YVIRTUALSCREEN)
	width := win.GetSystemMetrics(win.SM_CXVIRTUALSCREEN)
	height := win.GetSystemMetrics(win.SM_CYVIRTUALSCREEN)
	if width <= 0 || height <= 0 {
		return nil, errors.New("the interactive desktop is not available")
	}
	screenDC := win.GetDC(0)
	if screenDC == 0 {
		return nil, errors.New("get desktop device context failed")
	}
	defer win.ReleaseDC(0, screenDC)
	memoryDC := win.CreateCompatibleDC(screenDC)
	if memoryDC == 0 {
		return nil, errors.New("create desktop capture context failed")
	}
	defer win.DeleteDC(memoryDC)
	bitmap := win.CreateCompatibleBitmap(screenDC, width, height)
	if bitmap == 0 {
		return nil, errors.New("create desktop capture bitmap failed")
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	previous := win.SelectObject(memoryDC, win.HGDIOBJ(bitmap))
	if previous == 0 {
		return nil, errors.New("select desktop capture bitmap failed")
	}
	defer win.SelectObject(memoryDC, previous)
	if !win.BitBlt(memoryDC, 0, 0, width, height, screenDC, x, y, win.SRCCOPY|win.CAPTUREBLT) {
		return nil, errors.New("copy desktop pixels failed")
	}
	bgra := make([]byte, int(width*height*4))
	info := win.BITMAPINFO{BmiHeader: win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       width,
		BiHeight:      -height,
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}}
	if lines := win.GetDIBits(memoryDC, bitmap, 0, uint32(height), &bgra[0], &info, win.DIB_RGB_COLORS); lines != height {
		return nil, fmt.Errorf("read desktop pixels returned %d of %d lines", lines, height)
	}
	result := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	for offset := 0; offset < len(bgra); offset += 4 {
		result.Pix[offset] = bgra[offset+2]
		result.Pix[offset+1] = bgra[offset+1]
		result.Pix[offset+2] = bgra[offset]
		result.Pix[offset+3] = 255
	}
	return result, nil
}

// RunCaptureHelper is the interactive-session side of the Work agent screen
// service. Its stdout is reserved for the binary frame stream.
func RunCaptureHelper() error {
	request, err := ReadRequest(os.Stdin)
	if err != nil {
		return err
	}
	if console, openErr := os.OpenFile("CONOUT$", os.O_WRONLY, 0); openErr == nil {
		defer console.Close()
		_, _ = fmt.Fprintf(console, "DeskFerry %s authenticated screen viewing is active. Close this window to stop sharing the screen.\n", buildinfo.Version)
	}
	first, err := CaptureDesktop()
	if err != nil {
		_ = WriteFrame(os.Stdout, Frame{Type: FrameError, Error: err.Error()}, nil)
		return err
	}
	frame, payloads, err := EncodeFull(1, first)
	if err != nil {
		return err
	}
	if err := WriteFrame(os.Stdout, frame, payloads); err != nil {
		return err
	}
	if request.Mode == ModeSingle {
		return nil
	}
	previous := first
	ticker := time.NewTicker(time.Duration(request.IntervalMS) * time.Millisecond)
	defer ticker.Stop()
	for sequence := uint64(2); ; sequence++ {
		<-ticker.C
		current, err := CaptureDesktop()
		if err != nil {
			_ = WriteFrame(os.Stdout, Frame{Type: FrameError, Seq: sequence, Error: err.Error()}, nil)
			return err
		}
		frame, payloads, err := EncodeDelta(sequence, previous, current, request.TileSize)
		if err != nil {
			_ = WriteFrame(os.Stdout, Frame{Type: FrameError, Seq: sequence, Error: err.Error()}, nil)
			return err
		}
		if err := WriteFrame(os.Stdout, frame, payloads); err != nil {
			return err
		}
		previous = current
	}
}
