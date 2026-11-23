//go:build windows

package sharebanner

import (
	"errors"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"deskferry/internal/screenview"
	"deskferry/windows/uikit"
)

// Layout metrics in 1/96 inch units, from the design-system spacing scale.
const (
	bodyHeight     = 52
	bodyRadius     = uikit.CardRadius
	bodyBorder     = 2
	bodyPadLeft    = uikit.Space12
	bodyPadRight   = uikit.Space8
	markSize       = 24
	markGap        = uikit.Space12
	titleHeight    = 20
	captionHeight  = 16
	lineGap        = 2
	badgeHeight    = 20
	badgePad       = uikit.Space8
	badgeGap       = uikit.Space8
	dotSize        = 8
	dotGap         = 6
	textButtonGap  = uikit.Space16
	buttonHeight   = uikit.PrimaryButtonHeight
	buttonPad      = uikit.Space16
	buttonMinWidth = 104
	buttonGap      = uikit.Space8
	minTextWidth   = 96
	topGap         = uikit.Space8  // gap between the work-area top and the banner
	screenMargin   = uikit.Space16 // minimum gap to the work-area sides
	dragThreshold  = 3
)

const (
	className = "DeskFerryShareBanner"

	msgState = 0x8000 + 1 // WM_APP + 1: a new session state is pending
	msgClose = 0x8000 + 2 // WM_APP + 2: the session ended

	timerTopmost = 1
	timerPending = 2

	topmostIntervalMS = 2000
	pendingTimeoutMS  = 5000

	// clickGuard ignores button presses right after the buttons moved, so a
	// second click meant for a button that just went away cannot land on its
	// neighbour.
	clickGuard = 400 * time.Millisecond

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExLayered    = 0x00080000
	wsExNoActivate = 0x08000000
	wsPopup        = 0x80000000

	swShowNoActivate = 4
	maNoActivate     = 3

	wmSettingChange = 0x001A
	wmMouseActivate = 0x0021
	wmDisplayChange = 0x007E
	wmDpiChanged    = 0x02E0
	spiSetWorkArea  = 0x002F

	ulwAlpha   = 0x00000002
	acSrcAlpha = 0x01

	monitorDefaultToPrimary = 1
	monitorDefaultToNearest = 2

	imageIcon = 1
	diNormal  = 3
	pmRemove  = 1
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	procUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")
	procMonitorFromRect     = user32.NewProc("MonitorFromRect")
	procSetWindowTextW      = user32.NewProc("SetWindowTextW")
	procGdiFlush            = gdi32.NewProc("GdiFlush")

	registerOnce sync.Once
	registerErr  error
	classNamePtr = syscall.StringToUTF16Ptr(className)
	banners      sync.Map // win.HWND -> *banner
)

// layout is the banner geometry in pixels, relative to the window. The
// window is exactly the banner: like the other Windows cards it has a border
// and no shadow, so no transparent margin swallows clicks meant for the
// windows around it.
type layout struct {
	width, height int32
	mark          win.RECT
	badge         win.RECT
	title         win.RECT
	dot           win.RECT
	caption       win.RECT
	buttons       [buttonCount]win.RECT
}

// banner is the sharing banner window. Apart from pending, its fields belong
// to the UI thread.
type banner struct {
	hwnd    win.HWND
	actions Actions
	dpi     int

	mu         sync.Mutex
	pending    screenview.SessionState
	hasPending bool

	view   content
	layout layout

	// anchorX is the screen x of the banner's centre and anchorY the screen y
	// of its top edge. They follow the primary work area until the user
	// drags the banner.
	anchorX, anchorY int32
	userMoved        bool

	hover, pressed int // button index, or -1
	buttonsMovedAt time.Time
	tracking       bool
	dragging       bool
	dragCursor     win.POINT
	dragAnchorX    int32
	dragAnchorY    int32

	stopControlPending bool
	stopSharingPending bool

	rendering   bool
	renderAgain bool

	resourceDPI int
	titleFont   win.HFONT // also the button font
	captionFont win.HFONT
	badgeFont   win.HFONT
	icon        win.HICON
}

func registerClass() error {
	registerOnce.Do(func() {
		class := win.WNDCLASSEX{
			LpfnWndProc:   syscall.NewCallback(windowProc),
			HInstance:     win.GetModuleHandle(nil),
			HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
			LpszClassName: classNamePtr,
		}
		class.CbSize = uint32(unsafe.Sizeof(class))
		if win.RegisterClassEx(&class) == 0 {
			registerErr = errors.New("register the sharing banner window class")
		}
	})
	return registerErr
}

func windowProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if value, ok := banners.Load(hwnd); ok {
		return value.(*banner).handle(hwnd, msg, wParam, lParam)
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

// newBanner creates and shows the banner without activating it.
func newBanner(actions Actions, initial screenview.SessionState) (*banner, error) {
	if err := registerClass(); err != nil {
		return nil, err
	}
	b := &banner{actions: actions, dpi: 96, hover: -1, pressed: -1}
	work := primaryWorkArea()
	hwnd := win.CreateWindowEx(
		wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate,
		classNamePtr, syscall.StringToUTF16Ptr("DeskFerry screen sharing"), wsPopup,
		work.Left, work.Top, 1, 1, 0, 0, win.GetModuleHandle(nil), nil)
	if hwnd == 0 {
		return nil, errors.New("create the sharing banner window")
	}
	b.hwnd = hwnd
	banners.Store(hwnd, b)
	if dpi := win.GetDpiForWindow(hwnd); dpi != 0 {
		b.dpi = int(dpi)
	}
	b.view = describe(initial)
	b.render()
	win.ShowWindow(hwnd, swShowNoActivate)
	b.assertTopmost()
	win.SetTimer(hwnd, timerTopmost, topmostIntervalMS, 0)
	return b, nil
}

// offer records the latest session state for the UI thread. It is safe to
// call from any goroutine; states that arrive faster than the UI thread
// handles them replace each other.
func (b *banner) offer(state screenview.SessionState) {
	b.mu.Lock()
	b.pending = state
	b.hasPending = true
	b.mu.Unlock()
	win.PostMessage(b.hwnd, msgState, 0, 0)
}

// requestClose asks the UI thread to close the banner. It is safe to call
// from any goroutine.
func (b *banner) requestClose() {
	win.PostMessage(b.hwnd, msgClose, 0, 0)
}

// messageLoop runs until the banner window is destroyed.
func (b *banner) messageLoop() {
	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) > 0 {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
	if hwnd, ok := b.window(); ok {
		// The loop failed while the window still existed: remove it and
		// the quit message its destruction posts.
		win.DestroyWindow(hwnd)
		for win.PeekMessage(&msg, 0, 0, 0, pmRemove) {
		}
	}
}

func (b *banner) window() (win.HWND, bool) {
	_, ok := banners.Load(b.hwnd)
	return b.hwnd, ok
}

func (b *banner) handle(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case msgState:
		b.applyPending()
		return 0
	case msgClose:
		win.DestroyWindow(hwnd)
		return 0
	case win.WM_CLOSE:
		// Only the end of the session closes the banner.
		return 0
	case win.WM_DESTROY:
		win.KillTimer(hwnd, timerTopmost)
		win.KillTimer(hwnd, timerPending)
		banners.Delete(hwnd)
		b.releaseResources()
		win.PostQuitMessage(0)
		return 0
	case wmMouseActivate:
		return maNoActivate
	case win.WM_TIMER:
		switch wParam {
		case timerTopmost:
			b.assertTopmost()
		case timerPending:
			win.KillTimer(hwnd, timerPending)
			if b.stopControlPending || b.stopSharingPending {
				b.stopControlPending = false
				b.stopSharingPending = false
				b.render()
			}
		}
		return 0
	case wmDpiChanged:
		if dpi := int(wParam & 0xFFFF); dpi != 0 && dpi != b.dpi {
			b.dpi = dpi
			b.render()
		}
		return 0
	case wmDisplayChange:
		b.render()
	case wmSettingChange:
		if wParam == spiSetWorkArea {
			b.render()
		}
	case win.WM_MOUSEMOVE:
		b.mouseMove(pointFromLParam(lParam))
		return 0
	case win.WM_MOUSELEAVE:
		b.tracking = false
		b.setHover(-1)
		return 0
	case win.WM_LBUTTONDOWN:
		b.mouseDown(pointFromLParam(lParam))
		return 0
	case win.WM_LBUTTONUP:
		b.mouseUp(pointFromLParam(lParam))
		return 0
	case win.WM_CAPTURECHANGED:
		if win.HWND(lParam) != hwnd {
			b.dragging = false
			if b.pressed >= 0 {
				b.pressed = -1
				b.render()
			}
		}
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

func pointFromLParam(lParam uintptr) (int32, int32) {
	return int32(int16(lParam & 0xFFFF)), int32(int16((lParam >> 16) & 0xFFFF))
}

// applyPending shows the most recent session state.
func (b *banner) applyPending() {
	b.mu.Lock()
	state, ok := b.pending, b.hasPending
	b.hasPending = false
	b.mu.Unlock()
	if !ok {
		return
	}
	view := describe(state)
	changed := view != b.view
	b.view = view
	if !view.control {
		if b.stopControlPending {
			b.stopControlPending = false
			changed = true
		}
		if b.pressed == buttonStopControl {
			b.pressed = -1
		}
	}
	if changed {
		b.render()
	}
}

// assertTopmost keeps the banner above other topmost windows and visible,
// without activating it.
func (b *banner) assertTopmost() {
	win.SetWindowPos(b.hwnd, win.HWND_TOPMOST, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOACTIVATE|win.SWP_NOOWNERZORDER)
	if !win.IsWindowVisible(b.hwnd) {
		win.ShowWindow(b.hwnd, swShowNoActivate)
	}
}

// Buttons.

func (b *banner) buttonVisible(index int) bool {
	return index == buttonStopSharing || b.view.control
}

func (b *banner) buttonEnabled(index int) bool {
	if index == buttonStopControl {
		return !b.stopControlPending && !b.stopSharingPending
	}
	return !b.stopSharingPending
}

func (b *banner) buttonLabel(index int) string {
	switch {
	case index == buttonStopControl && b.stopControlPending, index == buttonStopSharing && b.stopSharingPending:
		return labelStopping
	case index == buttonStopControl:
		return labelStopControl
	default:
		return labelStopSharing
	}
}

func contains(rect win.RECT, x, y int32) bool {
	return x >= rect.Left && x < rect.Right && y >= rect.Top && y < rect.Bottom
}

// buttonAt returns the enabled button under a window point, or -1.
func (b *banner) buttonAt(x, y int32) int {
	for index := range b.layout.buttons {
		if b.buttonVisible(index) && b.buttonEnabled(index) && contains(b.layout.buttons[index], x, y) {
			return index
		}
	}
	return -1
}

func (b *banner) setHover(index int) {
	if index != b.hover {
		b.hover = index
		b.render()
	}
}

func (b *banner) mouseMove(x, y int32) {
	if b.dragging {
		b.drag()
		return
	}
	if !b.tracking {
		track := win.TRACKMOUSEEVENT{CbSize: uint32(unsafe.Sizeof(win.TRACKMOUSEEVENT{})), DwFlags: win.TME_LEAVE, HwndTrack: b.hwnd}
		b.tracking = win.TrackMouseEvent(&track)
	}
	hover := b.buttonAt(x, y)
	if b.pressed >= 0 && hover != b.pressed {
		hover = -1
	}
	b.setHover(hover)
}

func (b *banner) mouseDown(x, y int32) {
	if index := b.buttonAt(x, y); index >= 0 {
		if time.Since(b.buttonsMovedAt) < clickGuard {
			return
		}
		b.pressed = index
		b.hover = index
		win.SetCapture(b.hwnd)
		b.render()
		return
	}
	if win.GetCursorPos(&b.dragCursor) {
		b.dragging = true
		b.dragAnchorX, b.dragAnchorY = b.anchorX, b.anchorY
		win.SetCapture(b.hwnd)
	}
}

func (b *banner) mouseUp(x, y int32) {
	pressed := b.pressed
	wasActive := b.dragging || pressed >= 0
	b.dragging = false
	b.pressed = -1
	if wasActive {
		win.ReleaseCapture()
	}
	if pressed < 0 {
		return
	}
	b.hover = b.buttonAt(x, y)
	if b.hover == pressed {
		b.click(pressed)
	}
	b.render()
}

// click runs a button's action. The action runs on its own goroutine so a
// slow callback never freezes the banner; the button shows "Stopping…" until
// the session reports the change, or a timeout re-enables it.
func (b *banner) click(index int) {
	var action func()
	switch index {
	case buttonStopControl:
		b.stopControlPending = true
		action = b.actions.StopControl
	case buttonStopSharing:
		b.stopSharingPending = true
		action = b.actions.StopSharing
	}
	b.hover = -1
	win.SetTimer(b.hwnd, timerPending, pendingTimeoutMS, 0)
	if action != nil {
		go action()
	}
}

// drag moves the banner with the cursor and keeps it on a monitor.
func (b *banner) drag() {
	var cursor win.POINT
	if !win.GetCursorPos(&cursor) {
		return
	}
	dx, dy := cursor.X-b.dragCursor.X, cursor.Y-b.dragCursor.Y
	if !b.userMoved && max(dx, -dx)+max(dy, -dy) < b.px(dragThreshold) {
		return
	}
	b.userMoved = true
	b.anchorX, b.anchorY = b.dragAnchorX+dx, b.dragAnchorY+dy
	b.clampAnchor()
	x, y := b.origin()
	// Crossing to a monitor with another DPI re-renders from wmDpiChanged.
	win.SetWindowPos(b.hwnd, 0, x, y, 0, 0, win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE)
}

// Placement.

func (b *banner) px(value int) int32 {
	return int32((value*b.dpi + 48) / 96)
}

func monitorWorkArea(rect win.RECT, flags uintptr) (win.RECT, bool) {
	monitor, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&rect)), flags)
	info := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	if monitor == 0 || !win.GetMonitorInfo(win.HMONITOR(monitor), &info) {
		return win.RECT{}, false
	}
	return info.RcWork, true
}

// primaryWorkArea is the work area of the primary monitor, whose top-left
// corner is the origin of the virtual screen.
func primaryWorkArea() win.RECT {
	if work, ok := monitorWorkArea(win.RECT{Right: 1, Bottom: 1}, monitorDefaultToPrimary); ok {
		return work
	}
	return win.RECT{Right: win.GetSystemMetrics(win.SM_CXSCREEN), Bottom: win.GetSystemMetrics(win.SM_CYSCREEN)}
}

// screenRect is the banner in screen coordinates.
func (b *banner) screenRect() win.RECT {
	left := b.anchorX - b.layout.width/2
	return win.RECT{Left: left, Top: b.anchorY, Right: left + b.layout.width, Bottom: b.anchorY + b.layout.height}
}

// currentWorkArea is the work area the banner is on.
func (b *banner) currentWorkArea() win.RECT {
	if !b.userMoved {
		return primaryWorkArea()
	}
	if work, ok := monitorWorkArea(b.screenRect(), monitorDefaultToNearest); ok {
		return work
	}
	return primaryWorkArea()
}

// clampAnchor places the banner at the top centre of the primary work area,
// or keeps a banner the user moved inside its monitor's work area.
func (b *banner) clampAnchor() {
	work := b.currentWorkArea()
	width, height := b.layout.width, b.layout.height
	if !b.userMoved {
		b.anchorX = (work.Left + work.Right) / 2
		b.anchorY = work.Top + b.px(topGap)
		return
	}
	half := width / 2
	if work.Right-work.Left <= width {
		b.anchorX = (work.Left + work.Right) / 2
	} else {
		b.anchorX = max(work.Left+half, min(b.anchorX, work.Right-(width-half)))
	}
	b.anchorY = max(work.Top, min(b.anchorY, work.Bottom-height))
}

// origin is the window's top-left corner on the screen.
func (b *banner) origin() (int32, int32) {
	rect := b.screenRect()
	return rect.Left, rect.Top
}

// Resources.

func createFont(family string, points int, bold bool, dpi int) win.HFONT {
	font := win.LOGFONT{
		LfHeight:  -int32((points*dpi + 36) / 72),
		LfWeight:  win.FW_NORMAL,
		LfCharSet: win.DEFAULT_CHARSET,
		LfQuality: win.CLEARTYPE_QUALITY,
	}
	if bold {
		font.LfWeight = win.FW_BOLD
	}
	name := syscall.StringToUTF16(family)
	copy(font.LfFaceName[:len(font.LfFaceName)-1], name)
	return win.CreateFontIndirect(&font)
}

// ensureResources creates the fonts and the app icon for the current DPI.
func (b *banner) ensureResources() {
	if b.resourceDPI == b.dpi {
		return
	}
	b.releaseResources()
	b.resourceDPI = b.dpi
	strong := uikit.CaptionStrongFont() // the design system's semibold face
	caption := uikit.CaptionFont()
	b.titleFont = createFont(strong.Family, uikit.BodyFont().PointSize, strong.Bold, b.dpi)
	b.captionFont = createFont(caption.Family, caption.PointSize, caption.Bold, b.dpi)
	b.badgeFont = createFont(strong.Family, strong.PointSize, strong.Bold, b.dpi)
	size := b.px(markSize)
	b.icon = win.HICON(win.LoadImage(win.GetModuleHandle(nil), win.MAKEINTRESOURCE(uikit.AppIconResourceID), imageIcon, size, size, 0))
}

func (b *banner) releaseResources() {
	for _, font := range []*win.HFONT{&b.titleFont, &b.captionFont, &b.badgeFont} {
		if *font != 0 {
			win.DeleteObject(win.HGDIOBJ(*font))
			*font = 0
		}
	}
	if b.icon != 0 {
		win.DestroyIcon(b.icon)
		b.icon = 0
	}
	b.resourceDPI = 0
}

func textWidth(hdc win.HDC, font win.HFONT, text string) int32 {
	units := syscall.StringToUTF16(text)
	if len(units) <= 1 {
		return 0
	}
	old := win.SelectObject(hdc, win.HGDIOBJ(font))
	defer win.SelectObject(hdc, old)
	var size win.SIZE
	if !win.GetTextExtentPoint32(hdc, &units[0], int32(len(units)-1), &size) {
		return int32(8 * len(text))
	}
	return size.CX + 2
}

// Layout and painting.

// computeLayout measures the content and sizes the banner to fit it, within
// the width of the work area.
func (b *banner) computeLayout(hdc win.HDC) layout {
	px := b.px
	var badgeWidth, badgeSpace int32
	if b.view.badge != "" {
		badgeWidth = textWidth(hdc, b.badgeFont, b.view.badge) + 2*px(badgePad)
		badgeSpace = badgeWidth + px(badgeGap)
	}
	titleWidth := textWidth(hdc, b.titleFont, b.view.title)
	captionWidth := textWidth(hdc, b.captionFont, b.view.caption)
	dotSpace := px(dotSize) + px(dotGap)
	textWidthTotal := max(badgeSpace+titleWidth, dotSpace+captionWidth)

	// Buttons keep the width of their normal label while they show "Stopping…".
	var buttonWidths [buttonCount]int32
	var buttonsWidth int32
	for index, label := range [buttonCount]string{labelStopControl, labelStopSharing} {
		if !b.buttonVisible(index) {
			continue
		}
		buttonWidths[index] = max(px(buttonMinWidth), textWidth(hdc, b.titleFont, label)+2*px(buttonPad))
		if buttonsWidth > 0 {
			buttonsWidth += px(buttonGap)
		}
		buttonsWidth += buttonWidths[index]
	}

	textLeft := px(bodyPadLeft) + px(markSize) + px(markGap)
	fixed := textLeft + px(textButtonGap) + buttonsWidth + px(bodyPadRight)
	work := b.currentWorkArea()
	available := work.Right - work.Left - 2*px(screenMargin) - fixed
	textWidthTotal = max(px(minTextWidth), min(textWidthTotal, available))

	l := layout{width: fixed + textWidthTotal, height: px(bodyHeight)}
	markTop := (l.height - px(markSize)) / 2
	l.mark = win.RECT{Left: px(bodyPadLeft), Top: markTop, Right: px(bodyPadLeft) + px(markSize), Bottom: markTop + px(markSize)}

	left := textLeft
	right := left + textWidthTotal
	titleTop := (l.height - px(titleHeight) - px(lineGap) - px(captionHeight)) / 2
	titleLeft := left
	if badgeWidth > 0 {
		badgeTop := titleTop + (px(titleHeight)-px(badgeHeight))/2
		l.badge = win.RECT{Left: left, Top: badgeTop, Right: left + badgeWidth, Bottom: badgeTop + px(badgeHeight)}
		titleLeft += badgeSpace
	}
	l.title = win.RECT{Left: titleLeft, Top: titleTop, Right: right, Bottom: titleTop + px(titleHeight)}
	captionTop := l.title.Bottom + px(lineGap)
	dotTop := captionTop + (px(captionHeight)-px(dotSize))/2 + px(1)
	l.dot = win.RECT{Left: left, Top: dotTop, Right: left + px(dotSize), Bottom: dotTop + px(dotSize)}
	l.caption = win.RECT{Left: left + dotSpace, Top: captionTop, Right: right, Bottom: captionTop + px(captionHeight)}

	buttonLeft := right + px(textButtonGap)
	buttonTop := (l.height - px(buttonHeight)) / 2
	for index, width := range buttonWidths {
		if width == 0 {
			continue
		}
		l.buttons[index] = win.RECT{Left: buttonLeft, Top: buttonTop, Right: buttonLeft + width, Bottom: buttonTop + px(buttonHeight)}
		buttonLeft += width + px(buttonGap)
	}
	return l
}

// render repaints the banner and moves it to its place. A render requested
// while one is running (a DPI change caused by the move) runs afterwards.
func (b *banner) render() {
	if b.rendering {
		b.renderAgain = true
		return
	}
	b.rendering = true
	defer func() { b.rendering = false }()
	for {
		b.renderAgain = false
		b.renderOnce()
		if !b.renderAgain {
			return
		}
	}
}

func (b *banner) renderOnce() {
	b.ensureResources()
	screen := win.GetDC(0)
	if screen == 0 {
		return
	}
	defer win.ReleaseDC(0, screen)
	mem := win.CreateCompatibleDC(screen)
	if mem == 0 {
		return
	}
	defer win.DeleteDC(mem)

	if l := b.computeLayout(mem); l != b.layout {
		// The buttons moved under the cursor: drop hover and any press.
		b.layout = l
		b.buttonsMovedAt = time.Now()
		b.hover = -1
		if b.pressed >= 0 {
			b.pressed = -1
			win.ReleaseCapture()
		}
	}
	b.clampAnchor()
	l := b.layout

	header := win.BITMAPINFOHEADER{BiWidth: l.width, BiHeight: -l.height, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	header.BiSize = uint32(unsafe.Sizeof(header))
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(screen, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bits == nil {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	oldBitmap := win.SelectObject(mem, win.HGDIOBJ(bitmap))
	defer win.SelectObject(mem, oldBitmap)

	canvas := &surface{width: int(l.width), height: int(l.height), pix: unsafe.Slice((*byte)(bits), int(l.width)*int(l.height)*4)}
	colors := schemeFor(b.view)
	styles := b.paintShapes(canvas, colors)
	// GDI draws the text and the icon on the opaque body; it does not keep
	// the alpha channel, so the alpha is saved and restored around it.
	alpha := canvas.alphaChannel()
	b.paintText(mem, colors, styles)
	_, _, _ = procGdiFlush.Call()
	canvas.setAlphaChannel(alpha)

	x, y := b.origin()
	position := win.POINT{X: x, Y: y}
	size := win.SIZE{CX: l.width, CY: l.height}
	var source win.POINT
	blend := win.BLENDFUNCTION{SourceConstantAlpha: 255, AlphaFormat: acSrcAlpha}
	_, _, _ = procUpdateLayeredWindow.Call(uintptr(b.hwnd), uintptr(screen),
		uintptr(unsafe.Pointer(&position)), uintptr(unsafe.Pointer(&size)),
		uintptr(mem), uintptr(unsafe.Pointer(&source)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)

	if text, err := syscall.UTF16PtrFromString(b.view.accessibleText()); err == nil {
		_, _, _ = procSetWindowTextW.Call(uintptr(b.hwnd), uintptr(unsafe.Pointer(text)))
	}
}

func fillRect(canvas *surface, rect win.RECT, radius float64, color walk.Color) {
	canvas.fillRoundRect(float64(rect.Left), float64(rect.Top), float64(rect.Right-rect.Left), float64(rect.Bottom-rect.Top), radius, color)
}

func inset(rect win.RECT, amount int32) win.RECT {
	return win.RECT{Left: rect.Left + amount, Top: rect.Top + amount, Right: rect.Right - amount, Bottom: rect.Bottom - amount}
}

// paintShapes paints everything except text and the icon, and returns the
// style each button was painted with.
func (b *banner) paintShapes(canvas *surface, colors scheme) [buttonCount]buttonStyle {
	l := b.layout
	body := win.RECT{Right: l.width, Bottom: l.height}
	radius := float64(b.px(bodyRadius))
	border := max(1, b.px(bodyBorder))
	fillRect(canvas, body, radius, colors.border)
	fillRect(canvas, inset(body, border), radius-float64(border), colors.fill)

	if b.icon == 0 {
		// No embedded icon (test binaries): a primary rounded mark, as uikit.AppMark draws.
		fillRect(canvas, l.mark, float64(l.mark.Right-l.mark.Left)/4, uikit.ColorPrimary)
	}
	if b.view.badge != "" {
		fillRect(canvas, l.badge, float64(l.badge.Bottom-l.badge.Top)/2, colors.badgeFill)
	}
	fillRect(canvas, l.dot, float64(l.dot.Right-l.dot.Left)/2, colors.dot)

	var styles [buttonCount]buttonStyle
	buttonRadius := float64(b.px(uikit.ControlRadius))
	for index, rect := range l.buttons {
		if !b.buttonVisible(index) {
			continue
		}
		hover := b.hover == index
		style := buttonStyleFor(index, b.view, colors, hover, hover && b.pressed == index, b.buttonEnabled(index))
		styles[index] = style
		if style.bordered {
			line := max(1, b.px(1))
			fillRect(canvas, rect, buttonRadius, style.border)
			fillRect(canvas, inset(rect, line), buttonRadius-float64(line), style.fill)
		} else {
			fillRect(canvas, rect, buttonRadius, style.fill)
		}
	}
	return styles
}

func drawText(hdc win.HDC, font win.HFONT, text string, color walk.Color, rect win.RECT, format uint32) {
	units, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	old := win.SelectObject(hdc, win.HGDIOBJ(font))
	defer win.SelectObject(hdc, old)
	win.SetTextColor(hdc, win.COLORREF(color))
	win.DrawTextEx(hdc, units, -1, &rect, format|win.DT_SINGLELINE|win.DT_VCENTER|win.DT_NOPREFIX, nil)
}

func (b *banner) paintText(hdc win.HDC, colors scheme, styles [buttonCount]buttonStyle) {
	l := b.layout
	win.SetBkMode(hdc, win.TRANSPARENT)
	if b.icon != 0 {
		win.DrawIconEx(hdc, l.mark.Left, l.mark.Top, b.icon, l.mark.Right-l.mark.Left, l.mark.Bottom-l.mark.Top, 0, 0, diNormal)
	}
	if b.view.badge != "" {
		drawText(hdc, b.badgeFont, b.view.badge, colors.badgeText, l.badge, win.DT_CENTER)
	}
	drawText(hdc, b.titleFont, b.view.title, colors.title, l.title, win.DT_LEFT|win.DT_END_ELLIPSIS)
	drawText(hdc, b.captionFont, b.view.caption, colors.caption, l.caption, win.DT_LEFT|win.DT_END_ELLIPSIS)
	for index, rect := range l.buttons {
		if b.buttonVisible(index) {
			drawText(hdc, b.titleFont, b.buttonLabel(index), styles[index].text, rect, win.DT_CENTER)
		}
	}
}
