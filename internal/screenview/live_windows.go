//go:build windows

package screenview

import (
	"errors"
	"image"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/kirides/go-d3d/outputduplication"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	gdi32                    = windows.NewLazySystemDLL("gdi32.dll")
	sasDLL                   = windows.NewLazySystemDLL("sas.dll")
	procGetUserObjectInfo    = user32.NewProc("GetUserObjectInformationW")
	procGetCursorInfo        = user32.NewProc("GetCursorInfo")
	procGetIconInfo          = user32.NewProc("GetIconInfo")
	procDrawIconEx           = user32.NewProc("DrawIconEx")
	procGdiFlush             = gdi32.NewProc("GdiFlush")
	procSendInput            = user32.NewProc("SendInput")
	procMapVirtualKeyW       = user32.NewProc("MapVirtualKeyW")
	procVkKeyScanW           = user32.NewProc("VkKeyScanW")
	procGetForegroundWindow  = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procSetWindowsHookExW    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx       = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx  = user32.NewProc("UnhookWindowsHookEx")
	procGetMessageW          = user32.NewProc("GetMessageW")
	procPostThreadMessageW   = user32.NewProc("PostThreadMessageW")
	procOpenClipboard        = user32.NewProc("OpenClipboard")
	procCloseClipboard       = user32.NewProc("CloseClipboard")
	procEmptyClipboard       = user32.NewProc("EmptyClipboard")
	procGetClipboardData     = user32.NewProc("GetClipboardData")
	procSetClipboardData     = user32.NewProc("SetClipboardData")
	procGlobalAlloc          = kernel32.NewProc("GlobalAlloc")
	procGlobalFree           = kernel32.NewProc("GlobalFree")
	procGlobalLock           = kernel32.NewProc("GlobalLock")
	procGlobalUnlock         = kernel32.NewProc("GlobalUnlock")
	procGetDIBits            = gdi32.NewProc("GetDIBits")
	procGetObjectW           = gdi32.NewProc("GetObjectW")
	procSendSAS              = sasDLL.NewProc("SendSAS")
	procGetModuleHandleW     = kernel32.NewProc("GetModuleHandleW")
	procCloseDesktopLive     = user32.NewProc("CloseDesktop")
	procOpenInputDesktopLive = user32.NewProc("OpenInputDesktop")
	procSetThreadDesktopLive = user32.NewProc("SetThreadDesktop")
)

const (
	uoiName              = 2
	desktopAllAccess     = 0x01FF
	inputMouse           = 0
	inputKeyboard        = 1
	mouseMove            = 0x0001
	mouseLeftDown        = 0x0002
	mouseLeftUp          = 0x0004
	mouseRightDown       = 0x0008
	mouseRightUp         = 0x0010
	mouseMiddleDown      = 0x0020
	mouseMiddleUp        = 0x0040
	mouseXDown           = 0x0080
	mouseXUp             = 0x0100
	mouseWheel           = 0x0800
	mouseHWheel          = 0x1000
	mouseAbsolute        = 0x8000
	keyExtended          = 0x0001
	keyUp                = 0x0002
	keyUnicode           = 0x0004
	whKeyboardLL         = 13
	whMouseLL            = 14
	llkhfInjected        = 0x10
	llmhfInjected        = 0x01
	wmQuit               = 0x0012
	cfUnicodeText        = 13
	gmemMoveable         = 0x0002
	maxClipboardRunes    = 1 << 20
	desktopCheckInterval = 250 * time.Millisecond
)

// desktopWorker runs calls on one OS thread that follows the input desktop,
// so capture and input keep working across lock screens, UAC prompts and
// other secure-desktop switches when the helper runs as LocalSystem.
type desktopWorker struct {
	calls     chan func()
	name      atomic.Value
	handle    uintptr
	lastCheck time.Time
}

func newDesktopWorker() *desktopWorker {
	worker := &desktopWorker{calls: make(chan func())}
	worker.name.Store("")
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		worker.follow(true)
		close(ready)
		for call := range worker.calls {
			call()
		}
	}()
	<-ready
	return worker
}

func (w *desktopWorker) do(fn func()) {
	done := make(chan struct{})
	w.calls <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// follow attaches the thread to the current input desktop when it changed and
// reports whether it switched. It must run on the worker thread.
func (w *desktopWorker) follow(force bool) bool {
	if !force && time.Since(w.lastCheck) < desktopCheckInterval {
		return false
	}
	w.lastCheck = time.Now()
	handle, _, _ := procOpenInputDesktopLive.Call(0, 0, desktopAllAccess)
	if handle == 0 {
		handle, _, _ = procOpenInputDesktopLive.Call(0, 0, desktopReadObjects|desktopWriteObjects|desktopSwitchDesktop)
	}
	if handle == 0 {
		return false
	}
	name := desktopName(handle)
	if !force && strings.EqualFold(name, w.currentName()) {
		procCloseDesktopLive.Call(handle)
		return false
	}
	if ok, _, _ := procSetThreadDesktopLive.Call(handle); ok == 0 {
		procCloseDesktopLive.Call(handle)
		return false
	}
	if w.handle != 0 {
		procCloseDesktopLive.Call(w.handle)
	}
	w.handle = handle
	w.name.Store(name)
	return true
}

func (w *desktopWorker) currentName() string {
	name, _ := w.name.Load().(string)
	return name
}

func desktopName(handle uintptr) string {
	buffer := make([]uint16, 256)
	var needed uint32
	ok, _, _ := procGetUserObjectInfo.Call(handle, uoiName, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)*2), uintptr(unsafe.Pointer(&needed)))
	if ok == 0 {
		return ""
	}
	return windows.UTF16ToString(buffer)
}

// inputDesktopName returns the name of the desktop that receives input, or
// "Winlogon (unavailable)" when this process may not open it.
func inputDesktopName() string {
	handle, _, _ := procOpenInputDesktopLive.Call(0, 0, desktopReadObjects)
	if handle == 0 {
		return "Winlogon"
	}
	defer procCloseDesktopLive.Call(handle)
	return desktopName(handle)
}

// liveSource captures the primary display for live sessions.
type liveSource struct {
	worker *desktopWorker

	dxgi         *dxgiCapturer
	lastDXGITry  time.Time
	buffer       *image.RGBA
	last         *image.RGBA
	method       string
	lastGDI      time.Time
	failingSince time.Time

	cursorMu    sync.Mutex
	cursor      CursorState
	cursorCache map[uintptr]cursorShape
}

type cursorShape struct {
	image      *image.RGBA
	hotX, hotY int
	id         uint32
}

// NewLiveSource returns the Windows live capture source.
func NewLiveSource() LiveSource {
	return &liveSource{worker: newDesktopWorker(), cursorCache: make(map[uintptr]cursorShape)}
}

func (s *liveSource) Method() string {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	return s.method
}

func (s *liveSource) setMethod(method string) {
	s.cursorMu.Lock()
	s.method = method
	s.cursorMu.Unlock()
}

func (s *liveSource) Cursor() CursorState {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	return s.cursor
}

func (s *liveSource) Close() {
	s.worker.do(func() {
		if s.dxgi != nil {
			s.dxgi.Close()
			s.dxgi = nil
		}
	})
}

func (s *liveSource) Next(timeout time.Duration) (*image.RGBA, bool, error) {
	var frame *image.RGBA
	var changed bool
	var err error
	// An RDP attach, unlock or desktop switch can make capture briefly
	// unavailable; ride through it before the first frame exists.
	for attempt := 1; ; attempt++ {
		s.worker.do(func() { frame, changed, err = s.next(timeout) })
		if err == nil || attempt >= initialCaptureAttempts {
			return frame, changed, err
		}
		time.Sleep(captureRetryDelay)
	}
}

func (s *liveSource) next(timeout time.Duration) (*image.RGBA, bool, error) {
	if s.worker.follow(false) && s.dxgi != nil {
		s.dxgi.Close()
		s.dxgi = nil
		s.lastDXGITry = time.Time{}
	}
	s.updateCursor()
	if s.dxgi == nil && time.Since(s.lastDXGITry) > time.Second {
		s.lastDXGITry = time.Now()
		if capturer, err := newDXGICapturerWithPointer(false); err == nil {
			s.dxgi = capturer
		}
	}
	if s.dxgi != nil {
		bounds := s.dxgi.bounds
		if s.buffer == nil || s.buffer.Bounds().Dx() != bounds.Dx() || s.buffer.Bounds().Dy() != bounds.Dy() {
			s.buffer = image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
			if s.last != nil && s.last.Bounds().Size() == s.buffer.Bounds().Size() {
				copy(s.buffer.Pix, s.last.Pix)
			}
		}
		waitMS := uint(timeout / time.Millisecond)
		if s.last == nil {
			waitMS = 250
		}
		err := s.dxgi.captureInto(s.buffer, waitMS)
		switch {
		case err == nil:
			s.last = s.buffer
			s.failingSince = time.Time{}
			s.setMethod("dxgi")
			return s.buffer, true, nil
		case errors.Is(err, outputduplication.ErrNoImageYet) && s.last != nil:
			return s.last, false, nil
		case errors.Is(err, outputduplication.ErrNoImageYet):
			// A new duplication delivers nothing until the desktop changes;
			// take the first frame through GDI.
		default:
			s.dxgi.Close()
			s.dxgi = nil
		}
	}
	// GDI fallback at up to 15 frames per second.
	if wait := 66*time.Millisecond - time.Since(s.lastGDI); s.last != nil && wait > 0 {
		time.Sleep(min(wait, max(timeout, time.Millisecond)))
		if time.Since(s.lastGDI) < 66*time.Millisecond {
			return s.last, false, nil
		}
	}
	s.lastGDI = time.Now()
	frame, err := captureDesktopGDI()
	if err == nil {
		s.last = frame
		s.failingSince = time.Time{}
		if s.dxgi == nil {
			s.setMethod("gdi")
		}
		return frame, true, nil
	}
	if s.last != nil {
		// Keep the session through a desktop transition and keep retrying.
		if s.failingSince.IsZero() {
			s.failingSince = time.Now()
		}
		s.setMethod("unavailable")
		return s.last, false, nil
	}
	return nil, false, explainRemoteSessionCaptureFailure(err)
}

type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor uintptr
	X, Y   int32
}

type iconInfo struct {
	IsIcon int32
	HotX   uint32
	HotY   uint32
	_      uint32
	Mask   uintptr
	Color  uintptr
}

type bitmapInfo struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	_          uint32
	Bits       uintptr
}

func (s *liveSource) updateCursor() {
	info := cursorInfo{Size: uint32(unsafe.Sizeof(cursorInfo{}))}
	if ok, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		s.cursorMu.Lock()
		s.cursor.Visible = false
		s.cursorMu.Unlock()
		return
	}
	state := CursorState{X: int(info.X), Y: int(info.Y), Visible: info.Flags&1 != 0}
	if state.Visible && info.Cursor != 0 {
		shape, ok := s.cursorCache[info.Cursor]
		if !ok {
			shape = readCursorShape(info.Cursor)
			if len(s.cursorCache) > 64 {
				s.cursorCache = make(map[uintptr]cursorShape)
			}
			s.cursorCache[info.Cursor] = shape
		}
		state.Shape, state.HotX, state.HotY, state.ShapeID = shape.image, shape.hotX, shape.hotY, shape.id
	}
	s.cursorMu.Lock()
	s.cursor = state
	s.cursorMu.Unlock()
}

func readCursorShape(cursor uintptr) cursorShape {
	var icon iconInfo
	if ok, _, _ := procGetIconInfo.Call(cursor, uintptr(unsafe.Pointer(&icon))); ok == 0 {
		return cursorShape{}
	}
	if icon.Mask != 0 {
		defer win.DeleteObject(win.HGDIOBJ(icon.Mask))
	}
	if icon.Color != 0 {
		defer win.DeleteObject(win.HGDIOBJ(icon.Color))
	}
	screenDC := win.GetDC(0)
	if screenDC == 0 {
		return cursorShape{}
	}
	defer win.ReleaseDC(0, screenDC)
	maskPixels, maskWidth, maskHeight := bitmapPixels(screenDC, icon.Mask)
	width, height := maskWidth, maskHeight
	if icon.Color == 0 {
		height = maskHeight / 2
	} else if _, colorWidth, colorHeight := bitmapPixels(screenDC, icon.Color); colorWidth > 0 {
		width, height = colorWidth, colorHeight
	}
	if rendered := renderCursor(screenDC, cursor, width, height); rendered != nil {
		return cursorShape{image: rendered, hotX: int(icon.HotX), hotY: int(icon.HotY), id: CursorShapeID(rendered, int(icon.HotX), int(icon.HotY))}
	}
	var out *image.RGBA
	if icon.Color != 0 {
		colorPixels, width, height := bitmapPixels(screenDC, icon.Color)
		if colorPixels == nil {
			return cursorShape{}
		}
		out = image.NewRGBA(image.Rect(0, 0, width, height))
		hasAlpha := false
		for i := 3; i < len(colorPixels); i += 4 {
			if colorPixels[i] != 0 {
				hasAlpha = true
				break
			}
		}
		for i := 0; i+3 < len(colorPixels); i += 4 {
			out.Pix[i], out.Pix[i+1], out.Pix[i+2] = colorPixels[i+2], colorPixels[i+1], colorPixels[i]
			alpha := colorPixels[i+3]
			if !hasAlpha {
				alpha = 255
				if maskPixels != nil && i < len(maskPixels) && maskPixels[i] != 0 {
					alpha = 0
				}
			}
			out.Pix[i+3] = alpha
		}
	} else if maskPixels != nil && maskHeight >= 2 {
		height := maskHeight / 2
		out = image.NewRGBA(image.Rect(0, 0, maskWidth, height))
		for y := 0; y < height; y++ {
			for x := 0; x < maskWidth; x++ {
				and := maskPixels[(y*maskWidth+x)*4] != 0
				xor := maskPixels[((y+height)*maskWidth+x)*4] != 0
				offset := (y*maskWidth + x) * 4
				switch {
				case !and && !xor:
					out.Pix[offset+3] = 255
				case !and && xor:
					out.Pix[offset], out.Pix[offset+1], out.Pix[offset+2], out.Pix[offset+3] = 255, 255, 255, 255
				case and && xor:
					// Inverting pixels cannot be reproduced over a
					// separate overlay; draw them dark with a light core.
					out.Pix[offset], out.Pix[offset+1], out.Pix[offset+2], out.Pix[offset+3] = 32, 32, 32, 255
				}
			}
		}
	}
	if out == nil {
		return cursorShape{}
	}
	return cursorShape{image: out, hotX: int(icon.HotX), hotY: int(icon.HotY), id: CursorShapeID(out, int(icon.HotX), int(icon.HotY))}
}

// renderCursor draws the cursor as Windows does, over black and over white,
// and derives its colors and transparency from the two; this works for every
// kind of cursor (alpha, masked color, monochrome) without interpreting its
// bitmaps.
func renderCursor(screenDC win.HDC, cursor uintptr, width, height int) *image.RGBA {
	if width <= 0 || height <= 0 || width > 512 || height > 512 {
		return nil
	}
	dc := win.CreateCompatibleDC(screenDC)
	if dc == 0 {
		return nil
	}
	defer win.DeleteDC(dc)
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      -int32(height),
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(dc, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bits == nil {
		return nil
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	previous := win.SelectObject(dc, win.HGDIOBJ(bitmap))
	defer win.SelectObject(dc, previous)
	pixels := unsafe.Slice((*byte)(bits), width*height*4)
	draw := func(background byte) []byte {
		for i := range pixels {
			pixels[i] = background
		}
		const diNormal = 0x0003
		if ok, _, _ := procDrawIconEx.Call(uintptr(dc), 0, 0, cursor, uintptr(width), uintptr(height), 0, 0, diNormal); ok == 0 {
			return nil
		}
		procGdiFlush.Call()
		return append([]byte(nil), pixels...)
	}
	black := draw(0)
	white := draw(255)
	if black == nil || white == nil {
		return nil
	}
	return cursorFromBlackWhite(black, white, width, height)
}

func bitmapPixels(dc win.HDC, bitmap uintptr) ([]byte, int, int) {
	if bitmap == 0 {
		return nil, 0, 0
	}
	var info bitmapInfo
	if n, _, _ := procGetObjectW.Call(bitmap, unsafe.Sizeof(info), uintptr(unsafe.Pointer(&info))); n == 0 || info.Width <= 0 || info.Height <= 0 || info.Width > 512 || info.Height > 1024 {
		return nil, 0, 0
	}
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       info.Width,
		BiHeight:      -info.Height,
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	type bitmapInfoWithColors struct {
		Header win.BITMAPINFOHEADER
		Colors [256]uint32
	}
	var request bitmapInfoWithColors
	request.Header = header
	pixels := make([]byte, int(info.Width*info.Height*4))
	lines, _, _ := procGetDIBits.Call(uintptr(dc), bitmap, 0, uintptr(info.Height), uintptr(unsafe.Pointer(&pixels[0])), uintptr(unsafe.Pointer(&request)), win.DIB_RGB_COLORS)
	if lines == 0 {
		return nil, 0, 0
	}
	return pixels, int(info.Width), int(info.Height)
}

// rawInput is the Windows INPUT structure on 64-bit Windows.
type rawInput struct {
	Type uint32
	_    uint32
	Data [32]byte
}

type mouseInputData struct {
	DX, DY    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	_         uint32
	Extra     uintptr
}

type keyInputData struct {
	VK    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
}

func mouseRaw(data mouseInputData) rawInput {
	input := rawInput{Type: inputMouse}
	*(*mouseInputData)(unsafe.Pointer(&input.Data[0])) = data
	return input
}

func keyRaw(data keyInputData) rawInput {
	input := rawInput{Type: inputKeyboard}
	*(*keyInputData)(unsafe.Pointer(&input.Data[0])) = data
	return input
}

// liveHost is the Windows input, clipboard and state backend.
type liveHost struct {
	worker *desktopWorker

	mu          sync.Mutex
	heldKeys    map[uint16]bool
	heldButtons map[string]bool

	lastHuman atomic.Int64
	hookStop  func()
}

// NewLiveHost returns the Windows input and state backend. It starts the
// low-level hooks that tell people's input apart from injected input.
func NewLiveHost() LiveHost {
	host := &liveHost{worker: newDesktopWorker(), heldKeys: make(map[uint16]bool), heldButtons: make(map[string]bool)}
	host.hookStop = startHumanInputHooks(&host.lastHuman)
	return host
}

func (h *liveHost) LastHumanInput() time.Time {
	value := h.lastHuman.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}

func (h *liveHost) Inject(events []InputEvent) error {
	var err error
	h.worker.do(func() {
		h.worker.follow(false)
		err = h.inject(events)
	})
	return err
}

func (h *liveHost) inject(events []InputEvent) error {
	width := int(win.GetSystemMetrics(win.SM_CXSCREEN))
	height := int(win.GetSystemMetrics(win.SM_CYSCREEN))
	var inputs []rawInput
	moveTo := func(x, y int) {
		x = max(0, min(width-1, x))
		y = max(0, min(height-1, y))
		inputs = append(inputs, mouseRaw(mouseInputData{
			DX:    int32((x*65535 + (width-1)/2) / max(1, width-1)),
			DY:    int32((y*65535 + (height-1)/2) / max(1, height-1)),
			Flags: mouseMove | mouseAbsolute,
		}))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, event := range events {
		x, y, hasPoint := event.Point()
		switch event.T {
		case "move":
			if !hasPoint {
				return commandError("bad_request", "move needs x and y")
			}
			moveTo(x, y)
		case "button":
			if hasPoint {
				moveTo(x, y)
			}
			flags, data, err := buttonFlags(event.B, event.Down)
			if err != nil {
				return err
			}
			inputs = append(inputs, mouseRaw(mouseInputData{Flags: flags, MouseData: data}))
			if event.Down {
				h.heldButtons[event.B] = true
			} else {
				delete(h.heldButtons, event.B)
			}
		case "wheel":
			if hasPoint {
				moveTo(x, y)
			}
			if event.DY != 0 {
				inputs = append(inputs, mouseRaw(mouseInputData{Flags: mouseWheel, MouseData: uint32(int32(event.DY))}))
			}
			if event.DX != 0 {
				inputs = append(inputs, mouseRaw(mouseInputData{Flags: mouseHWheel, MouseData: uint32(int32(event.DX))}))
			}
		case "key":
			if event.VK <= 0 || event.VK > 0xFE {
				return commandError("bad_request", "key needs a virtual-key code from 1 to 254")
			}
			inputs = append(inputs, h.keyInput(uint16(event.VK), uint16(event.Scan), event.Ext || extendedVK(uint16(event.VK)), event.Down))
		case "char_key":
			runes := []rune(event.C)
			if len(runes) != 1 {
				return commandError("bad_request", "char_key needs one character")
			}
			inputs = append(inputs, h.charKeyInputs(runes[0], event.Down)...)
		case "text":
			inputs = append(inputs, textInputs(event.S)...)
		case "release_all":
			inputs = append(inputs, h.releaseInputs()...)
		default:
			return commandError("bad_request", "unknown input event %q", event.T)
		}
	}
	return sendInputs(inputs)
}

func sendInputs(inputs []rawInput) error {
	if len(inputs) == 0 {
		return nil
	}
	sent, _, callErr := procSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(rawInput{}))
	if int(sent) != len(inputs) {
		if strings.EqualFold(inputDesktopName(), "Winlogon") {
			return commandError("screen_locked", "Windows accepted %d of %d input events; the Work PC shows a secure desktop (lock screen, sign-in or UAC prompt) that this DeskFerry helper cannot control", sent, len(inputs))
		}
		return commandError("failed", "Windows accepted %d of %d input events: %v", sent, len(inputs), callErr)
	}
	return nil
}

func buttonFlags(button string, down bool) (uint32, uint32, error) {
	pick := func(downFlag, upFlag uint32) uint32 {
		if down {
			return downFlag
		}
		return upFlag
	}
	switch button {
	case "left", "":
		return pick(mouseLeftDown, mouseLeftUp), 0, nil
	case "right":
		return pick(mouseRightDown, mouseRightUp), 0, nil
	case "middle":
		return pick(mouseMiddleDown, mouseMiddleUp), 0, nil
	case "x1":
		return pick(mouseXDown, mouseXUp), 1, nil
	case "x2":
		return pick(mouseXDown, mouseXUp), 2, nil
	}
	return 0, 0, commandError("bad_request", "unknown mouse button %q", button)
}

// extendedVK lists virtual keys that need KEYEVENTF_EXTENDEDKEY.
func extendedVK(vk uint16) bool {
	switch vk {
	case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2D, 0x2E, // PgUp..Down, Insert, Delete
		0x5B, 0x5C, 0x5D, // Windows keys, Apps
		0x6F,       // Divide
		0x90,       // NumLock
		0xA3, 0xA5, // right Ctrl, right Alt
		0x2C: // PrintScreen
		return true
	}
	return false
}

func (h *liveHost) keyInput(vk, scan uint16, extended, down bool) rawInput {
	if scan == 0 {
		mapped, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
		scan = uint16(mapped)
	}
	flags := uint32(0)
	if extended {
		flags |= keyExtended
	}
	if !down {
		flags |= keyUp
		delete(h.heldKeys, vk)
	} else {
		h.heldKeys[vk] = extended
	}
	return keyRaw(keyInputData{VK: vk, Scan: scan, Flags: flags})
}

func (h *liveHost) charKeyInputs(character rune, down bool) []rawInput {
	if character > 0xFFFF {
		if !down {
			return nil
		}
		return textInputs(string(character))
	}
	result, _, _ := procVkKeyScanW.Call(uintptr(character))
	if int16(result) == -1 {
		if !down {
			return nil
		}
		return textInputs(string(character))
	}
	vk := uint16(result & 0xFF)
	shift := (result >> 8) & 0xFF
	modifiers := []uint16{}
	if shift&1 != 0 {
		modifiers = append(modifiers, 0x10)
	}
	if shift&2 != 0 {
		modifiers = append(modifiers, 0x11)
	}
	if shift&4 != 0 {
		modifiers = append(modifiers, 0x12)
	}
	if !down {
		return []rawInput{h.keyInput(vk, 0, false, false)}
	}
	// Hold the layout's modifiers only while the key goes down; the character
	// is produced on key down.
	var inputs []rawInput
	var pressed []uint16
	for _, modifier := range modifiers {
		if !h.heldKeys[modifier] {
			inputs = append(inputs, h.keyInput(modifier, 0, false, true))
			pressed = append(pressed, modifier)
		}
	}
	inputs = append(inputs, h.keyInput(vk, 0, false, true))
	for _, modifier := range pressed {
		inputs = append(inputs, h.keyInput(modifier, 0, false, false))
	}
	return inputs
}

func textInputs(text string) []rawInput {
	var inputs []rawInput
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, character := range text {
		switch character {
		case '\n', '\r':
			inputs = append(inputs, keyRaw(keyInputData{VK: 0x0D}), keyRaw(keyInputData{VK: 0x0D, Flags: keyUp}))
			continue
		case '\t':
			inputs = append(inputs, keyRaw(keyInputData{VK: 0x09}), keyRaw(keyInputData{VK: 0x09, Flags: keyUp}))
			continue
		}
		for _, unit := range utf16.Encode([]rune{character}) {
			inputs = append(inputs,
				keyRaw(keyInputData{Scan: unit, Flags: keyUnicode}),
				keyRaw(keyInputData{Scan: unit, Flags: keyUnicode | keyUp}))
		}
	}
	return inputs
}

func (h *liveHost) releaseInputs() []rawInput {
	var inputs []rawInput
	for vk, extended := range h.heldKeys {
		flags := uint32(keyUp)
		if extended {
			flags |= keyExtended
		}
		inputs = append(inputs, keyRaw(keyInputData{VK: vk, Flags: flags}))
	}
	h.heldKeys = make(map[uint16]bool)
	for button := range h.heldButtons {
		if flags, data, err := buttonFlags(button, false); err == nil {
			inputs = append(inputs, mouseRaw(mouseInputData{Flags: flags, MouseData: data}))
		}
	}
	h.heldButtons = make(map[string]bool)
	return inputs
}

func (h *liveHost) ReleaseAll() {
	h.worker.do(func() {
		h.worker.follow(false)
		h.mu.Lock()
		inputs := h.releaseInputs()
		h.mu.Unlock()
		_ = sendInputs(inputs)
	})
}

func (h *liveHost) Desktop() (string, bool, string) {
	name := inputDesktopName()
	var title string
	h.worker.do(func() {
		h.worker.follow(false)
		hwnd, _, _ := procGetForegroundWindow.Call()
		if hwnd == 0 {
			return
		}
		buffer := make([]uint16, 512)
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		title = windows.UTF16ToString(buffer[:n])
	})
	return name, !strings.EqualFold(name, "Default"), title
}

func (h *liveHost) ClipboardGet() (string, error) {
	var text string
	var err error
	h.worker.do(func() {
		if err = openClipboard(); err != nil {
			return
		}
		defer procCloseClipboard.Call()
		handle, _, _ := procGetClipboardData.Call(cfUnicodeText)
		if handle == 0 {
			return
		}
		pointer, _, _ := procGlobalLock.Call(handle)
		if pointer == 0 {
			err = commandError("failed", "could not read the clipboard")
			return
		}
		defer procGlobalUnlock.Call(handle)
		units := unsafe.Slice((*uint16)(unsafe.Pointer(pointer)), maxClipboardRunes)
		end := 0
		for end < len(units) && units[end] != 0 {
			end++
		}
		text = string(utf16.Decode(units[:end]))
	})
	return text, err
}

func (h *liveHost) ClipboardSet(text string) error {
	if len(text) > maxClipboardRunes {
		return commandError("bad_request", "clipboard text is too long")
	}
	var err error
	h.worker.do(func() {
		if err = openClipboard(); err != nil {
			return
		}
		defer procCloseClipboard.Call()
		procEmptyClipboard.Call()
		units := utf16.Encode([]rune(text))
		units = append(units, 0)
		handle, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(units)*2))
		if handle == 0 {
			err = commandError("failed", "could not allocate clipboard memory")
			return
		}
		pointer, _, _ := procGlobalLock.Call(handle)
		if pointer == 0 {
			procGlobalFree.Call(handle)
			err = commandError("failed", "could not lock clipboard memory")
			return
		}
		copy(unsafe.Slice((*uint16)(unsafe.Pointer(pointer)), len(units)), units)
		procGlobalUnlock.Call(handle)
		if ok, _, _ := procSetClipboardData.Call(cfUnicodeText, handle); ok == 0 {
			procGlobalFree.Call(handle)
			err = commandError("failed", "could not set the clipboard")
		}
	})
	return err
}

func openClipboard() error {
	for attempt := 0; attempt < 10; attempt++ {
		if ok, _, _ := procOpenClipboard.Call(0); ok != 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return commandError("failed", "the Work PC's clipboard is in use by another program")
}

func (h *liveHost) SAS() error {
	if err := procSendSAS.Find(); err != nil {
		return commandError("unsupported", "this edition of Windows does not provide SendSAS")
	}
	if !softwareSASAllowed() {
		return commandError("unsupported", "Windows policy SoftwareSASGeneration does not allow services to send Ctrl+Alt+Del; restart the DeskFerry Agent service with remote control enabled to set it")
	}
	procSendSAS.Call(0)
	return nil
}

func softwareSASAllowed() bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, _, err := key.GetIntegerValue("SoftwareSASGeneration")
	return err == nil && (value == 1 || value == 3)
}

// EnableSoftwareSAS allows services to send Ctrl+Alt+Del when the policy is
// not configured. An administrator's explicit setting is left unchanged.
func EnableSoftwareSAS() (bool, error) {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()
	if _, _, err := key.GetIntegerValue("SoftwareSASGeneration"); err == nil {
		return false, nil
	}
	return true, key.SetDWordValue("SoftwareSASGeneration", 1)
}

func (h *liveHost) UITree(command Command) ([]UINode, error) {
	return runUIAutomation("tree", command)
}

func (h *liveHost) UIFind(command Command) ([]UINode, error) {
	if strings.TrimSpace(command.Text) == "" {
		return nil, commandError("bad_request", "ui_find needs text")
	}
	return runUIAutomation("find", command)
}

// startHumanInputHooks installs low-level keyboard and mouse hooks that record
// the time of input not injected by software.
func startHumanInputHooks(last *atomic.Int64) func() {
	threadID := make(chan uint32, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		restore, _ := bindThreadToInputDesktop()
		defer restore()
		id, _, _ := procGetCurrentThreadID.Call()
		module, _, _ := procGetModuleHandleW.Call(0)
		keyboard := windows.NewCallback(func(code int, wParam, lParam uintptr) uintptr {
			if code >= 0 && lParam != 0 {
				flags := *(*uint32)(unsafe.Pointer(lParam + 8))
				if flags&llkhfInjected == 0 {
					last.Store(time.Now().UnixNano())
				}
			}
			result, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
			return result
		})
		mouse := windows.NewCallback(func(code int, wParam, lParam uintptr) uintptr {
			if code >= 0 && lParam != 0 {
				flags := *(*uint32)(unsafe.Pointer(lParam + 12))
				if flags&llmhfInjected == 0 {
					last.Store(time.Now().UnixNano())
				}
			}
			result, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
			return result
		})
		keyboardHook, _, _ := procSetWindowsHookExW.Call(whKeyboardLL, keyboard, module, 0)
		mouseHook, _, _ := procSetWindowsHookExW.Call(whMouseLL, mouse, module, 0)
		threadID <- uint32(id)
		var message [48]byte
		for {
			result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message[0])), 0, 0, 0)
			if int32(result) <= 0 {
				break
			}
		}
		if keyboardHook != 0 {
			procUnhookWindowsHookEx.Call(keyboardHook)
		}
		if mouseHook != 0 {
			procUnhookWindowsHookEx.Call(mouseHook)
		}
	}()
	id := <-threadID
	return func() { procPostThreadMessageW.Call(uintptr(id), wmQuit, 0, 0) }
}
