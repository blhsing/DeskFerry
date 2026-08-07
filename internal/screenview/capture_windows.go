package screenview

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"deskferry/internal/buildinfo"
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procOpenInputDesktop   = user32.NewProc("OpenInputDesktop")
	procSetThreadDesktop   = user32.NewProc("SetThreadDesktop")
	procGetThreadDesktop   = user32.NewProc("GetThreadDesktop")
	procCloseDesktop       = user32.NewProc("CloseDesktop")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

const (
	desktopReadObjects   = 0x0001
	desktopWriteObjects  = 0x0080
	desktopSwitchDesktop = 0x0100
)

type desktopCapturer struct {
	dxgi *dxgiCapturer
}

func (c *desktopCapturer) Close() {
	if c.dxgi != nil {
		c.dxgi.Close()
		c.dxgi = nil
	}
}

func (c *desktopCapturer) Capture() (*image.RGBA, error) {
	if c.dxgi != nil {
		result, dxgiErr := c.dxgi.Capture()
		if dxgiErr == nil {
			return result, nil
		}
		c.Close()
		result, gdiErr := captureDesktopGDI()
		if gdiErr == nil {
			return result, nil
		}
		return c.captureDXGI(gdiErr, fmt.Errorf("existing DirectX desktop capture failed: %w", dxgiErr))
	}
	result, gdiErr := captureDesktopGDI()
	if gdiErr == nil {
		return result, nil
	}
	return c.captureDXGI(gdiErr)
}

func (c *desktopCapturer) captureDXGI(gdiErr error, previousErrors ...error) (*image.RGBA, error) {
	dxgiErrors := append([]error(nil), previousErrors...)
	// A display mode, RDP reconnect, lock/unlock, or input-desktop switch can
	// invalidate a duplication object. Initialize/recreate it once before
	// surfacing the combined GDI/DirectX error to the Home viewer.
	for attempt := 0; attempt < 2; attempt++ {
		replacement, err := newDXGICapturer()
		if err != nil {
			dxgiErrors = append(dxgiErrors, fmt.Errorf("DirectX desktop capture initialization failed: %w", err))
			break
		}
		c.dxgi = replacement
		if result, captureErr := c.dxgi.Capture(); captureErr == nil {
			return result, nil
		} else {
			dxgiErrors = append(dxgiErrors, fmt.Errorf("DirectX desktop capture attempt %d failed: %w", attempt+1, captureErr))
		}
		c.Close()
	}
	return nil, errors.Join(gdiErr, errors.Join(dxgiErrors...))
}

// CaptureDesktop captures the complete Windows virtual desktop from the
// interactive session in which the process is running.
func CaptureDesktop() (*image.RGBA, error) {
	capturer := &desktopCapturer{}
	defer capturer.Close()
	return capturer.Capture()
}

func captureDesktopGDI() (*image.RGBA, error) {
	x := win.GetSystemMetrics(win.SM_XVIRTUALSCREEN)
	y := win.GetSystemMetrics(win.SM_YVIRTUALSCREEN)
	width := win.GetSystemMetrics(win.SM_CXVIRTUALSCREEN)
	height := win.GetSystemMetrics(win.SM_CYVIRTUALSCREEN)
	if width <= 0 || height <= 0 {
		return nil, errors.New("the interactive desktop is not available")
	}
	var attempts []error
	if screenDC := win.GetDC(0); screenDC != 0 {
		result, err := captureDesktopFromDC(screenDC, x, y, width, height)
		win.ReleaseDC(0, screenDC)
		if err == nil {
			return result, nil
		}
		attempts = append(attempts, fmt.Errorf("screen DC: %w", err))
	} else {
		attempts = append(attempts, windowsLastError("get screen device context"))
	}

	desktopWindow := win.GetDesktopWindow()
	if desktopDC := win.GetDC(desktopWindow); desktopDC != 0 {
		result, err := captureDesktopFromDC(desktopDC, x, y, width, height)
		win.ReleaseDC(desktopWindow, desktopDC)
		if err == nil {
			return result, nil
		}
		attempts = append(attempts, fmt.Errorf("desktop-window DC: %w", err))
	} else {
		attempts = append(attempts, windowsLastError("get desktop-window device context"))
	}

	displayDriver, _ := windows.UTF16PtrFromString("DISPLAY")
	if displayDC := win.CreateDC(displayDriver, nil, nil, nil); displayDC != 0 {
		result, err := captureDesktopFromDC(displayDC, x, y, width, height)
		win.DeleteDC(displayDC)
		if err == nil {
			return result, nil
		}
		attempts = append(attempts, fmt.Errorf("DISPLAY DC: %w", err))
	} else {
		attempts = append(attempts, windowsLastError("create DISPLAY device context"))
	}
	return nil, fmt.Errorf("GDI desktop capture failed: %w", errors.Join(attempts...))
}

func captureDesktopFromDC(screenDC win.HDC, x, y, width, height int32) (*image.RGBA, error) {
	memoryDC := win.CreateCompatibleDC(screenDC)
	if memoryDC == 0 {
		return nil, windowsLastError("create desktop capture context")
	}
	defer win.DeleteDC(memoryDC)
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       width,
		BiHeight:      -height,
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	var pixels unsafe.Pointer
	bitmap := win.CreateDIBSection(screenDC, &header, win.DIB_RGB_COLORS, &pixels, 0, 0)
	if bitmap == 0 || pixels == nil {
		return nil, windowsLastError("create desktop capture bitmap")
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	previous := win.SelectObject(memoryDC, win.HGDIOBJ(bitmap))
	if previous == 0 {
		return nil, windowsLastError("select desktop capture bitmap")
	}
	selected := true
	defer func() {
		if selected {
			win.SelectObject(memoryDC, previous)
		}
	}()
	// CAPTUREBLT includes layered windows, but some RDP display drivers reject
	// it. Retry the same copy without CAPTUREBLT before reporting the desktop as
	// unavailable.
	if !win.BitBlt(memoryDC, 0, 0, width, height, screenDC, x, y, win.SRCCOPY|win.CAPTUREBLT) {
		layeredErr := windows.GetLastError()
		if !win.BitBlt(memoryDC, 0, 0, width, height, screenDC, x, y, win.SRCCOPY) {
			return nil, fmt.Errorf("copy %dx%d desktop pixels at (%d,%d) failed (layered=%v, basic=%v)",
				width, height, x, y, layeredErr, windows.GetLastError())
		}
	}
	// Restore the original object before deleting the capture bitmap.
	if restored := win.SelectObject(memoryDC, previous); restored == 0 {
		return nil, windowsLastError("restore desktop capture bitmap")
	}
	selected = false
	bgra := unsafe.Slice((*byte)(pixels), int(width*height*4))
	result := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	for offset := 0; offset < len(bgra); offset += 4 {
		result.Pix[offset] = bgra[offset+2]
		result.Pix[offset+1] = bgra[offset+1]
		result.Pix[offset+2] = bgra[offset]
		result.Pix[offset+3] = 255
	}
	return result, nil
}

func windowsLastError(operation string) error {
	err := windows.GetLastError()
	if err == nil {
		return errors.New(operation + " failed")
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}

// bindThreadToInputDesktop makes capture follow the desktop that is actually
// visible in the selected interactive session. STARTUPINFO.lpDesktop normally
// attaches the helper to winsta0\default, but RDP reconnects, lock/unlock and
// secure-desktop transitions can make another desktop the current input
// desktop before capture starts.
func bindThreadToInputDesktop() (func(), error) {
	threadID, _, _ := procGetCurrentThreadID.Call()
	original, _, _ := procGetThreadDesktop.Call(threadID)
	input, _, openErr := procOpenInputDesktop.Call(0, 0,
		desktopReadObjects|desktopWriteObjects|desktopSwitchDesktop)
	if input == 0 {
		return func() {}, fmt.Errorf("open input desktop: %w", openErr)
	}
	if switched, _, switchErr := procSetThreadDesktop.Call(input); switched == 0 {
		procCloseDesktop.Call(input)
		return func() {}, fmt.Errorf("set capture thread desktop: %w", switchErr)
	}
	return func() {
		if original != 0 {
			procSetThreadDesktop.Call(original)
		}
		procCloseDesktop.Call(input)
	}, nil
}

// RunCaptureHelper is the interactive-session side of the Work agent screen
// service. Its stdout is reserved for the binary frame stream.
func RunCaptureHelper() error {
	request, err := ReadRequest(os.Stdin)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	restoreDesktop, desktopErr := bindThreadToInputDesktop()
	defer restoreDesktop()
	if console, openErr := os.OpenFile("CONOUT$", os.O_WRONLY, 0); openErr == nil {
		defer console.Close()
		_, _ = fmt.Fprintf(console, "DeskFerry %s authenticated screen viewing is active. Close this window to stop sharing the screen.\n", buildinfo.Version)
	}
	capturer := &desktopCapturer{}
	defer capturer.Close()
	first, err := capturer.Capture()
	if err != nil {
		if desktopErr != nil {
			err = errors.Join(fmt.Errorf("bind capture helper to input desktop: %w", desktopErr), err)
		}
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
		current, err := capturer.Capture()
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
