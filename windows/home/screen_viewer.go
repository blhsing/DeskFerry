//go:build windows

package homewindows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"deskferry/internal/buildinfo"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
	"deskferry/windows/uikit"
)

type screenViewer struct {
	owner      *clientApp
	mw         *walk.MainWindow
	viewport   *walk.ScrollView
	canvas     *walk.CustomWidget
	status     *walk.Label
	stats      *walk.Label
	liveButton *walk.PushButton
	control    *walk.PushButton
	cadButton  *walk.PushButton
	clipSend   *walk.PushButton
	clipGet    *walk.PushButton
	zoomBox    *walk.ComboBox
	minFPSBox  *walk.ComboBox
	minFPS     int

	mu                   sync.Mutex
	cancel               context.CancelFunc
	client               *screenview.LiveClient
	closed               bool
	zoom                 float64
	zoomControlUpdating  bool
	startupSized         bool
	renderWidth          int
	renderHeight         int
	gestureStartDistance uint64
	gestureStartZoom     float64
	panActive            bool
	panStart             win.POINT
	panStartX            int
	panStartY            int

	// Framebuffer copy in the BGRA layout GDI draws.
	frameMu sync.Mutex
	// bgra is the pixel memory of frameDIB, a top-down 32-bit DIB section.
	bgra        []byte
	frameDIB    win.HBITMAP
	frameWidth  int
	frameHeight int
	lossless    bool
	legacy      bool

	cursor        screenview.CursorState
	cursorBitmap  win.HBITMAP
	cursorShapeID uint32
	localCursor   win.HCURSOR

	controlling    atomic.Bool
	controlAllowed atomic.Bool
	inputs         chan screenview.InputEvent
	lastMove       time.Time
	pendingMove    *screenview.InputEvent
	keyboardHook   uintptr
	paintPending   atomic.Bool

	framesShown atomic.Int64
	statsFrames int64
	statsBytes  uint64
	statsAt     time.Time
	statusText  string
}

const (
	minScreenZoom = 0.10
	maxScreenZoom = 16.0
	wmGesture     = 0x0119
	gidZoom       = 3
	gfBegin       = 1
	halftone      = 4
	srcCopy       = 0x00CC0020
	acSrcOver     = 0
	acSrcAlpha    = 1
	wmMouseHWheel = 0x020E
	wmXButtonDown = 0x020B
	wmXButtonUp   = 0x020C
	wmKeyboardLL  = 13
	llkhfUp       = 0x80
	llkhfExtended = 0x01
	llkhfInjected = 0x10
	wmSetCursor   = 0x0020
	wmKillFocus   = 0x0008
	wmActivateApp = 0x001C
	moveInterval  = 8 * time.Millisecond
)

var (
	user32ScreenViewer     = windows.NewLazySystemDLL("user32.dll")
	gdi32ScreenViewer      = windows.NewLazySystemDLL("gdi32.dll")
	msimg32ScreenViewer    = windows.NewLazySystemDLL("msimg32.dll")
	kernel32ScreenViewer   = windows.NewLazySystemDLL("kernel32.dll")
	getGestureInfoScreen   = user32ScreenViewer.NewProc("GetGestureInfo")
	closeGestureInfoScreen = user32ScreenViewer.NewProc("CloseGestureInfoHandle")
	setGestureConfigScreen = user32ScreenViewer.NewProc("SetGestureConfig")
	setStretchModeScreen   = gdi32ScreenViewer.NewProc("SetStretchBltMode")
	setBrushOrgScreen      = gdi32ScreenViewer.NewProc("SetBrushOrgEx")
	alphaBlendScreen       = msimg32ScreenViewer.NewProc("AlphaBlend")
	createIconIndirect     = user32ScreenViewer.NewProc("CreateIconIndirect")
	destroyCursorScreen    = user32ScreenViewer.NewProc("DestroyCursor")
	setWindowsHookScreen   = user32ScreenViewer.NewProc("SetWindowsHookExW")
	unhookScreen           = user32ScreenViewer.NewProc("UnhookWindowsHookEx")
	callNextHookScreen     = user32ScreenViewer.NewProc("CallNextHookEx")
	getModuleHandleScreen  = kernel32ScreenViewer.NewProc("GetModuleHandleW")

	// Only one viewer can own the keyboard hook at a time.
	activeHookViewer atomic.Pointer[screenViewer]
	keyboardHookProc uintptr
)

type screenGesturePoint struct {
	X int16
	Y int16
}

type screenGestureInfo struct {
	Size       uint32
	Flags      uint32
	ID         uint32
	Target     uintptr
	Location   screenGesturePoint
	InstanceID uint32
	SequenceID uint32
	Arguments  uint64
	ExtraSize  uint32
}

type screenGestureConfig struct {
	ID    uint32
	Want  uint32
	Block uint32
}

type screenCanvasWidget struct {
	*walk.CustomWidget
	viewer *screenViewer
}

func (w *screenCanvasWidget) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if w.viewer.controlling.Load() {
		if handled, result := w.viewer.handleControlMessage(hwnd, msg, wParam, lParam); handled {
			return result
		}
	} else {
		if w.viewer.handlePanMessage(hwnd, msg, wParam) {
			return 0
		}
		if w.viewer.handleZoomMessage(msg, wParam, lParam) {
			return 0
		}
	}
	return w.CustomWidget.WndProc(hwnd, msg, wParam, lParam)
}

type screenZoomScrollView struct {
	*walk.ScrollView
	viewer *screenViewer
}

func (w *screenZoomScrollView) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if !w.viewer.controlling.Load() && w.viewer.handleZoomMessage(msg, wParam, lParam) {
		return 0
	}
	return w.ScrollView.WndProc(hwnd, msg, wParam, lParam)
}

func (a *clientApp) openScreenViewer() {
	if err := a.saveFromUI(false); err != nil {
		a.showError(err)
		return
	}
	cfg := a.currentConfig()
	if cfg.roomProof() == "" {
		a.showError(errors.New("save a room password for this destination before viewing its screen"))
		return
	}
	viewer := &screenViewer{owner: a, inputs: make(chan screenview.InputEvent, 2048)}
	title := "DeskFerry " + buildinfo.Version + " Screen Viewer - " + cfg.SelectedDestination
	if err := viewer.createWindow(title, a.mw); err != nil {
		a.showError(err)
		return
	}
	win.EnableWindow(a.mw.Handle(), false)
	viewer.mw.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		win.EnableWindow(a.mw.Handle(), true)
		win.SetForegroundWindow(a.mw.Handle())
	})
	viewer.mw.Show()
	win.ShowWindow(viewer.mw.Handle(), win.SW_MAXIMIZE)
	viewer.activateWindow()
	viewer.start()
}

// createWindow builds the viewer window with its dark toolbar strip.
func (v *screenViewer) createWindow(title string, owner *walk.MainWindow) error {
	toolbarLabel := func(text string) Widget {
		return Label{Text: text, TextColor: uikit.ColorDarkTextSecondary, Alignment: AlignHNearVCenter}
	}
	toolButton := func(assign **walk.PushButton, text, tooltip string, onClicked walk.EventHandler) Widget {
		return PushButton{AssignTo: assign, Text: text, ToolTipText: tooltip, OnClicked: onClicked, MinSize: Size{Width: 84, Height: uikit.ButtonHeight}, MaxSize: Size{Height: uikit.ButtonHeight}}
	}
	spacer := func() Widget {
		return HSpacer{Size: uikit.Space8, MinSize: Size{Width: uikit.Space8}, MaxSize: Size{Width: uikit.Space8}}
	}
	window := MainWindow{
		AssignTo:   &v.mw,
		Title:      title,
		Icon:       appIcon(),
		Font:       uikit.BodyFont(),
		Background: SolidColorBrush{Color: uikit.ColorDarkBg},
		MinSize:    Size{Width: 720, Height: 500},
		Size:       Size{Width: 1100, Height: 760},
		Layout:     VBox{MarginsZero: true, Spacing: 0},
		Children: []Widget{
			Composite{
				Background: SolidColorBrush{Color: uikit.ColorDarkSurface},
				Layout:     HBox{Margins: Margins{Left: uikit.Space12, Top: uikit.Space8, Right: uikit.Space12, Bottom: uikit.Space8}, Spacing: uikit.Space8},
				Children: []Widget{
					uikit.AppMark{Size: 20},
					Label{Text: "Screen viewer", Font: uikit.TitleFont(), TextColor: uikit.ColorDarkText, Alignment: AlignHNearVCenter},
					spacer(),
					toolButton(&v.liveButton, "Stop", "Start or stop the live screen", v.toggleLive),
					toolButton(&v.control, "Take control", "Use this PC's mouse and keyboard on the Work PC (Ctrl+Alt+Shift+C)", v.toggleControl),
					toolButton(&v.cadButton, "Ctrl+Alt+Del", "Send Ctrl+Alt+Del to the Work PC", v.sendSAS),
					toolButton(&v.clipSend, "Send clipboard", "Copy this PC's clipboard text to the Work PC", v.sendClipboard),
					toolButton(&v.clipGet, "Get clipboard", "Copy the Work PC's clipboard text to this PC", v.getClipboard),
					spacer(),
					toolbarLabel("Zoom"),
					ComboBox{
						AssignTo:              &v.zoomBox,
						Editable:              true,
						Model:                 []string{"Auto Fit", "25%", "50%", "75%", "100%", "125%", "150%", "200%", "300%", "400%"},
						CurrentIndex:          0,
						MinSize:               Size{Width: 110},
						MaxSize:               Size{Width: 130},
						Alignment:             AlignHNearVCenter,
						OnCurrentIndexChanged: v.applyZoomSelection,
						OnEditingFinished:     v.applyZoomSelection,
					},
					spacer(),
					toolbarLabel("Min fps"),
					ComboBox{
						AssignTo:              &v.minFPSBox,
						Editable:              true,
						Model:                 minFPSChoices,
						MinSize:               Size{Width: 110},
						MaxSize:               Size{Width: 130},
						Alignment:             AlignHNearVCenter,
						ToolTipText:           "Lowest frame rate before the Work PC compresses changing areas; 0 sends everything losslessly",
						OnCurrentIndexChanged: v.applyMinFPSSelection,
						OnEditingFinished:     v.applyMinFPSSelection,
					},
					HSpacer{},
					toolButton(nil, "Full screen", "Toggle full screen", v.toggleFullscreen),
					toolButton(nil, "Save PNG", "Save the current screen as a PNG image", v.savePNG),
				},
			},
			ScrollView{
				AssignTo:      &v.viewport,
				StretchFactor: 1,
				Background:    SolidColorBrush{Color: walk.RGB(0, 0, 0)},
				Layout:        Grid{MarginsZero: true},
				OnSizeChanged: v.layoutScreen,
				Children: []Widget{
					CustomWidget{
						AssignTo:            &v.canvas,
						Alignment:           AlignHCenterVCenter,
						Background:          SolidColorBrush{Color: walk.RGB(0, 0, 0)},
						MinSize:             Size{Width: 1, Height: 1},
						PaintPixels:         v.paintScreen,
						InvalidatesOnResize: true,
						ClearsBackground:    false,
					},
				},
			},
			Composite{
				Background: SolidColorBrush{Color: uikit.ColorDarkSurfaceSubtle},
				Layout:     HBox{Margins: Margins{Left: uikit.Space12, Top: uikit.Space4, Right: uikit.Space12, Bottom: uikit.Space4}},
				Children: []Widget{
					Label{AssignTo: &v.status, Text: "Connecting to the Work screen service...", Font: uikit.CaptionFont(), TextColor: uikit.ColorDarkTextMuted, EllipsisMode: EllipsisEnd},
					HSpacer{},
					Label{AssignTo: &v.stats, Font: uikit.CaptionFont(), TextColor: uikit.ColorDarkTextMuted},
				},
			},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	uikit.ResizeWindow(v.mw, 1100, 760)
	if owner != nil {
		win.SetWindowLongPtr(v.mw.Handle(), win.GWLP_HWNDPARENT, uintptr(owner.Handle()))
	}
	canvas := &screenCanvasWidget{CustomWidget: v.canvas, viewer: v}
	if err := walk.InitWrapperWindow(canvas); err != nil {
		v.mw.Dispose()
		return err
	}
	zoomViewport := &screenZoomScrollView{ScrollView: v.viewport, viewer: v}
	if err := walk.InitWrapperWindow(zoomViewport); err != nil {
		v.mw.Dispose()
		return err
	}
	enableScreenZoomGesture(v.canvas.Handle())
	enableScreenZoomGesture(v.viewport.Handle())
	v.minFPS = loadViewerMinFPS(v.owner.currentConfig().SelectedDestination)
	v.showMinFPS()
	v.updateControlButtons()
	go v.inputSender()
	v.mw.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		v.mu.Lock()
		v.closed = true
		cancel := v.cancel
		v.cancel = nil
		client := v.client
		v.mu.Unlock()
		v.setControlling(false)
		if cancel != nil {
			cancel()
		}
		if client != nil {
			_ = client.Close()
		}
		close(v.inputs)
		v.releaseCursorResources()
		v.frameMu.Lock()
		if v.frameDIB != 0 {
			win.DeleteObject(win.HGDIOBJ(v.frameDIB))
			v.frameDIB, v.bgra = 0, nil
		}
		v.frameMu.Unlock()
	})
	return nil
}

func enableScreenZoomGesture(hwnd win.HWND) {
	config := screenGestureConfig{ID: gidZoom, Want: 1}
	_, _, _ = setGestureConfigScreen.Call(uintptr(hwnd), 0, 1, uintptr(unsafe.Pointer(&config)), unsafe.Sizeof(config))
}

func (v *screenViewer) liveClient() *screenview.LiveClient {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.client
}

// start opens the screen session: live and encrypted when a screen key is
// saved, otherwise the read-only legacy stream.
func (v *screenViewer) start() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	if v.cancel != nil {
		v.cancel()
	}
	if v.client != nil {
		_ = v.client.Close()
		v.client = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	v.mu.Unlock()
	v.setStatus("Connecting to the Work screen service...")
	v.onUI(func() { _ = v.liveButton.SetText("Stop") })
	go v.run(ctx)
}

func (v *screenViewer) run(ctx context.Context) {
	cfg := v.owner.currentConfig()
	conn, route, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		if ctx.Err() == nil {
			v.setStatus("Screen connection failed: " + err.Error())
			v.onUI(func() { _ = v.liveButton.SetText("Start") })
		}
		return
	}
	v.owner.appendLog("Screen connection selected relay=%s proxy=%s protocol=%s relay_protocol=%s.", route.RelayAddr, route.Proxy, route.Protocol, route.RelayProtocol)
	v.setStatus(fmt.Sprintf("Connected through %s via %s (%s); waiting for the Work PC...", route.RelayAddr, route.Proxy, route.Protocol))
	hostname, _ := os.Hostname()
	minFPS := v.minFPS
	client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{
		ScreenKey:    cfg.screenKey(),
		MinFPS:       &minFPS,
		VideoDecoder: screenview.MediaFoundationDecoder,
		Client:       "viewer",
		Name:         hostname,
		OnFrame:      v.onFrame,
		OnStatus:     v.onStatus,
		OnCursor:     v.onCursor,
	})
	if err != nil {
		_ = conn.Close()
		if ctx.Err() == nil {
			v.setStatus("Screen session failed: " + err.Error())
			v.onUI(func() { _ = v.liveButton.SetText("Start") })
		}
		return
	}
	v.mu.Lock()
	if v.closed || ctx.Err() != nil {
		v.mu.Unlock()
		_ = client.Close()
		return
	}
	v.client = client
	v.mu.Unlock()
	v.frameMu.Lock()
	v.legacy = client.Legacy()
	v.frameMu.Unlock()
	// Frames that arrived before the client was stored were not copied.
	v.onFrame(screenview.FrameUpdate{Dirty: []image.Rectangle{image.Rect(0, 0, 1<<15, 1<<15)}})
	switch {
	case client.Legacy():
		v.controlAllowed.Store(false)
		v.setStatus("Read-only view: the Work PC runs an older DeskFerry without live view and control.")
	default:
		v.controlAllowed.Store(client.Hello().ControlAllowed)
		if client.Hello().ControlAllowed {
			v.setStatus("Live. Select Take control to use the Work PC's mouse and keyboard.")
		} else {
			v.setStatus("Live, view only: remote control is turned off in the Work PC's DeskFerry Work Services settings.")
		}
	}
	v.onUI(v.updateControlButtons)
	v.statsAt = time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = client.Close()
			return
		case <-client.Done():
			v.setControlling(false)
			if ctx.Err() == nil {
				reason := "the connection closed"
				if err := client.Err(); err != nil {
					reason = err.Error()
				}
				v.setStatus("Screen session ended: " + reason)
				v.onUI(func() {
					_ = v.liveButton.SetText("Start")
					_ = v.stats.SetText("")
					v.updateControlButtons()
				})
			}
			return
		case now := <-ticker.C:
			v.updateStats(client, now)
		}
	}
}

func (v *screenViewer) updateStats(client *screenview.LiveClient, now time.Time) {
	frames := v.framesShown.Load()
	received := client.BytesReceived()
	elapsed := now.Sub(v.statsAt).Seconds()
	if elapsed <= 0 {
		return
	}
	fps := float64(frames-v.statsFrames) / elapsed
	mbps := float64(received-v.statsBytes) * 8 / elapsed / 1e6
	v.statsFrames, v.statsBytes, v.statsAt = frames, received, now
	v.frameMu.Lock()
	lossless, legacy, width, height := v.lossless, v.legacy, v.frameWidth, v.frameHeight
	v.frameMu.Unlock()
	quality := "lossy"
	if lossless {
		quality = "lossless"
	}
	mode := "Live"
	if legacy {
		mode = "Read-only"
	}
	if compression := client.Status().Compression; compression != "" && compression != "full quality" && !legacy {
		quality = compression
	}
	text := fmt.Sprintf("%s · %dx%d · %.0f fps · %.2f Mbit/s · %s", mode, width, height, fps, mbps, quality)
	v.onUI(func() { _ = v.stats.SetText(text) })
}

// onFrame copies the dirty rectangles into the GDI buffer.
func (v *screenViewer) onFrame(update screenview.FrameUpdate) {
	client := v.liveClient()
	if client == nil {
		// The first frame can arrive before StartLive returns; it is drawn
		// with the next one or on resize.
		return
	}
	resized := false
	client.WithFramebuffer(func(fb *image.RGBA, _ uint64, lossless bool) {
		if fb == nil {
			return
		}
		width, height := fb.Bounds().Dx(), fb.Bounds().Dy()
		v.frameMu.Lock()
		defer v.frameMu.Unlock()
		dirty := update.Dirty
		if v.frameWidth != width || v.frameHeight != height || v.bgra == nil {
			if v.frameDIB != 0 {
				win.DeleteObject(win.HGDIOBJ(v.frameDIB))
				v.frameDIB, v.bgra = 0, nil
			}
			v.frameDIB, v.bgra = createFrameDIB(width, height)
			if v.frameDIB == 0 {
				return
			}
			v.frameWidth, v.frameHeight = width, height
			dirty = []image.Rectangle{fb.Bounds()}
			resized = true
		}
		v.lossless = lossless
		for _, rect := range dirty {
			rect = rect.Intersect(fb.Bounds())
			for y := rect.Min.Y; y < rect.Max.Y; y++ {
				src := fb.Pix[fb.PixOffset(rect.Min.X, y):]
				dst := v.bgra[(y*width+rect.Min.X)*4:]
				for x := 0; x < rect.Dx()*4; x += 4 {
					dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 255
				}
			}
		}
	})
	v.framesShown.Add(1)
	if !v.paintPending.CompareAndSwap(false, true) && !resized {
		return
	}
	v.onUI(func() {
		v.paintPending.Store(false)
		v.frameMu.Lock()
		width, height := v.frameWidth, v.frameHeight
		v.frameMu.Unlock()
		v.sizeWindowForFirstFrame(width, height)
		v.layoutScreen()
		if v.canvas != nil {
			win.InvalidateRect(v.canvas.Handle(), nil, false)
		}
	})
}

func (v *screenViewer) onStatus(status screenview.Status) {
	v.controlAllowed.Store(status.ControlAllowed)
	if !status.ControlActive && v.controlling.Load() {
		// The person at the Work PC stopped control from the banner.
		v.setControlling(false)
		v.setStatus("Control was stopped on the Work PC. Viewing continues.")
	}
	if status.Locked {
		v.setStatus("The Work PC shows its lock screen or another secure desktop (" + status.Desktop + ").")
	}
	v.onUI(v.updateControlButtons)
}

func (v *screenViewer) onCursor(cursor screenview.CursorState) {
	v.frameMu.Lock()
	previous := v.cursor
	v.cursor = cursor
	v.frameMu.Unlock()
	if previous.X == cursor.X && previous.Y == cursor.Y && previous.ShapeID == cursor.ShapeID && previous.Visible == cursor.Visible {
		return
	}
	v.onUI(func() {
		v.updateCursorResources(cursor)
		if v.canvas != nil && !v.controlling.Load() {
			win.InvalidateRect(v.canvas.Handle(), nil, false)
		}
	})
}

func (v *screenViewer) onUI(fn func()) {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed || v.mw == nil {
		return
	}
	v.mw.Synchronize(func() {
		v.mu.Lock()
		closed := v.closed
		v.mu.Unlock()
		if !closed {
			fn()
		}
	})
}

func (v *screenViewer) setStatus(text string) {
	v.mu.Lock()
	v.statusText = text
	v.mu.Unlock()
	v.onUI(func() { _ = v.status.SetText(text) })
}

func (v *screenViewer) toggleLive() {
	if v.liveClient() != nil {
		v.stop()
		return
	}
	v.start()
}

func (v *screenViewer) stop() {
	v.setControlling(false)
	v.mu.Lock()
	cancel := v.cancel
	v.cancel = nil
	client := v.client
	v.client = nil
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
	}
	_ = v.liveButton.SetText("Start")
	_ = v.stats.SetText("")
	v.updateControlButtons()
	v.setStatus("Screen stopped. Select Start to view the Work PC again.")
}

func (v *screenViewer) updateControlButtons() {
	client := v.liveClient()
	live := client != nil && !client.Legacy()
	allowed := live && v.controlAllowed.Load()
	controlling := v.controlling.Load()
	if v.control != nil {
		v.control.SetEnabled(allowed)
		text := "Take control"
		if controlling {
			text = "Release control"
		}
		_ = v.control.SetText(text)
	}
	for _, button := range []*walk.PushButton{v.cadButton, v.clipSend} {
		if button != nil {
			button.SetEnabled(allowed && controlling)
		}
	}
	if v.clipGet != nil {
		v.clipGet.SetEnabled(live)
	}
}

func (v *screenViewer) toggleControl() {
	v.setControlling(!v.controlling.Load())
}

// setControlling turns control on or off on the Work PC and locally.
func (v *screenViewer) setControlling(enable bool) {
	client := v.liveClient()
	if enable {
		if client == nil || client.Legacy() || !v.controlAllowed.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := client.SetControl(ctx, true, false)
		cancel()
		if err != nil {
			v.setStatus("Could not take control: " + err.Error())
			return
		}
		v.controlling.Store(true)
		v.onUI(func() {
			v.installKeyboardHook()
			if v.canvas != nil {
				_ = v.canvas.SetFocus()
			}
			v.updateControlButtons()
		})
		v.setStatus("Controlling the Work PC. Press Ctrl+Alt+Shift+C or select Release control to stop.")
		return
	}
	if !v.controlling.Swap(false) {
		return
	}
	v.queueInput(screenview.InputEvent{T: "release_all"})
	if client != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.SetControl(ctx, false, false)
		}()
	}
	v.onUI(func() {
		v.removeKeyboardHook()
		v.updateControlButtons()
		if v.canvas != nil {
			win.InvalidateRect(v.canvas.Handle(), nil, false)
		}
	})
	v.setStatus("Viewing only. Select Take control to use the Work PC's mouse and keyboard.")
}

func (v *screenViewer) queueInput(event screenview.InputEvent) {
	defer func() { _ = recover() }() // the channel closes with the window
	select {
	case v.inputs <- event:
	default:
	}
}

// inputSender batches queued input into commands, keeping the UI thread and
// the keyboard hook free of network writes.
func (v *screenViewer) inputSender() {
	for event := range v.inputs {
		batch := []screenview.InputEvent{event}
	collect:
		for len(batch) < 64 {
			select {
			case next, ok := <-v.inputs:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		if client := v.liveClient(); client != nil && !client.Legacy() {
			_ = client.SendInput(batch...)
		}
	}
}

func (v *screenViewer) sendSAS() {
	client := v.liveClient()
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.Call(ctx, screenview.Command{Cmd: "sas"}); err != nil {
			v.setStatus("Ctrl+Alt+Del failed: " + err.Error())
		}
	}()
}

func (v *screenViewer) sendClipboard() {
	client := v.liveClient()
	if client == nil {
		return
	}
	text, err := walk.Clipboard().Text()
	if err != nil {
		v.setStatus("Could not read this PC's clipboard: " + err.Error())
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.Call(ctx, screenview.Command{Cmd: "clipboard_set", Text: text}); err != nil {
			v.setStatus("Could not set the Work PC's clipboard: " + err.Error())
			return
		}
		v.setStatus(fmt.Sprintf("Sent %d characters to the Work PC's clipboard.", len([]rune(text))))
	}()
}

func (v *screenViewer) getClipboard() {
	client := v.liveClient()
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := client.Call(ctx, screenview.Command{Cmd: "clipboard_get"})
		if err != nil {
			v.setStatus("Could not read the Work PC's clipboard: " + err.Error())
			return
		}
		var result struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &result)
		v.onUI(func() {
			if err := walk.Clipboard().SetText(result.Text); err != nil {
				v.setStatus("Could not set this PC's clipboard: " + err.Error())
				return
			}
			v.setStatus(fmt.Sprintf("Copied %d characters from the Work PC's clipboard.", len([]rune(result.Text))))
		})
	}()
}

// canvasToFrame maps canvas client pixels to Work framebuffer pixels.
func (v *screenViewer) canvasToFrame(x, y int) (int, int) {
	v.frameMu.Lock()
	width, height := v.frameWidth, v.frameHeight
	v.frameMu.Unlock()
	if v.renderWidth <= 0 || v.renderHeight <= 0 || width <= 0 || height <= 0 {
		return 0, 0
	}
	fx := int(math.Floor(float64(x) * float64(width) / float64(v.renderWidth)))
	fy := int(math.Floor(float64(y) * float64(height) / float64(v.renderHeight)))
	return max(0, min(width-1, fx)), max(0, min(height-1, fy))
}

func pointFromLParam(lParam uintptr) (int, int) {
	return int(int16(uint16(lParam))), int(int16(uint16(lParam >> 16)))
}

func (v *screenViewer) mouseEvent(kind string, lParam uintptr) screenview.InputEvent {
	x, y := v.canvasToFrame(pointFromLParam(lParam))
	return screenview.InputEvent{T: kind, X: screenview.At(x), Y: screenview.At(y)}
}

// handleControlMessage forwards mouse input on the canvas while controlling.
func (v *screenViewer) handleControlMessage(hwnd win.HWND, msg uint32, wParam, lParam uintptr) (bool, uintptr) {
	button := func(name string, down bool) {
		event := v.mouseEvent("button", lParam)
		event.B, event.Down = name, down
		v.flushMove()
		v.queueInput(event)
		if down {
			win.SetCapture(hwnd)
			_ = v.canvas.SetFocus()
		} else if wParam&(win.MK_LBUTTON|win.MK_RBUTTON|win.MK_MBUTTON|0x20|0x40) == 0 {
			win.ReleaseCapture()
		}
	}
	switch msg {
	case win.WM_MOUSEMOVE:
		event := v.mouseEvent("move", lParam)
		if time.Since(v.lastMove) >= moveInterval {
			v.lastMove = time.Now()
			v.pendingMove = nil
			v.queueInput(event)
		} else {
			if v.pendingMove == nil {
				time.AfterFunc(moveInterval, func() { v.onUI(v.flushMove) })
			}
			v.pendingMove = &event
		}
		return true, 0
	case win.WM_LBUTTONDOWN, win.WM_LBUTTONDBLCLK:
		button("left", true)
		return true, 0
	case win.WM_LBUTTONUP:
		button("left", false)
		return true, 0
	case win.WM_RBUTTONDOWN, win.WM_RBUTTONDBLCLK:
		button("right", true)
		return true, 0
	case win.WM_RBUTTONUP:
		button("right", false)
		return true, 0
	case win.WM_MBUTTONDOWN, win.WM_MBUTTONDBLCLK:
		button("middle", true)
		return true, 0
	case win.WM_MBUTTONUP:
		button("middle", false)
		return true, 0
	case wmXButtonDown, wmXButtonUp:
		name := "x1"
		if (wParam>>16)&0xFFFF == 2 {
			name = "x2"
		}
		button(name, msg == wmXButtonDown)
		return true, 1
	case win.WM_MOUSEWHEEL, wmMouseHWheel:
		delta := int(int16(uint16(wParam >> 16)))
		// Wheel messages carry screen coordinates.
		point := win.POINT{X: int32(int16(uint16(lParam))), Y: int32(int16(uint16(lParam >> 16)))}
		win.ScreenToClient(hwnd, &point)
		x, y := v.canvasToFrame(int(point.X), int(point.Y))
		event := screenview.InputEvent{T: "wheel", X: screenview.At(x), Y: screenview.At(y)}
		if msg == win.WM_MOUSEWHEEL {
			event.DY = delta
		} else {
			event.DX = delta
		}
		v.queueInput(event)
		return true, 0
	case win.WM_KEYDOWN, win.WM_SYSKEYDOWN, win.WM_KEYUP, win.WM_SYSKEYUP, win.WM_CHAR, win.WM_SYSCHAR:
		// The low-level hook forwards keys; keep them from the local UI.
		return true, 0
	case wmSetCursor:
		if v.localCursor != 0 && uint16(lParam) == win.HTCLIENT {
			win.SetCursor(v.localCursor)
			return true, 1
		}
	case wmKillFocus:
		v.queueInput(screenview.InputEvent{T: "release_all"})
	case win.WM_GETDLGCODE:
		return true, win.DLGC_WANTALLKEYS
	}
	return false, 0
}

func (v *screenViewer) flushMove() {
	if v.pendingMove == nil {
		return
	}
	event := *v.pendingMove
	v.pendingMove = nil
	v.lastMove = time.Now()
	v.queueInput(event)
}

type kbdLLHookStruct struct {
	VKCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

func (v *screenViewer) installKeyboardHook() {
	if v.keyboardHook != 0 {
		return
	}
	if keyboardHookProc == 0 {
		keyboardHookProc = windows.NewCallback(screenKeyboardHook)
	}
	module, _, _ := getModuleHandleScreen.Call(0)
	hook, _, _ := setWindowsHookScreen.Call(wmKeyboardLL, keyboardHookProc, module, 0)
	if hook == 0 {
		v.setStatus("Keyboard forwarding is unavailable; only the mouse controls the Work PC.")
		return
	}
	v.keyboardHook = hook
	activeHookViewer.Store(v)
}

func (v *screenViewer) removeKeyboardHook() {
	if v.keyboardHook == 0 {
		return
	}
	unhookScreen.Call(v.keyboardHook)
	v.keyboardHook = 0
	activeHookViewer.CompareAndSwap(v, nil)
}

var hookModifiers struct {
	ctrl, alt, shift bool
}

// screenKeyboardHook forwards every key to the Work PC while a controlling
// viewer's canvas has the keyboard focus, including Windows, Alt+Tab and
// other shortcuts the local shell would otherwise take.
func screenKeyboardHook(code int, wParam, lParam uintptr) uintptr {
	v := activeHookViewer.Load()
	if code < 0 || v == nil || lParam == 0 || !v.controlling.Load() || v.canvas == nil {
		result, _, _ := callNextHookScreen.Call(0, uintptr(code), wParam, lParam)
		return result
	}
	info := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
	up := info.Flags&llkhfUp != 0
	switch info.VKCode {
	case 0x11, 0xA2, 0xA3:
		hookModifiers.ctrl = !up
	case 0x12, 0xA4, 0xA5:
		hookModifiers.alt = !up
	case 0x10, 0xA0, 0xA1:
		hookModifiers.shift = !up
	}
	if win.GetFocus() != v.canvas.Handle() || info.Flags&llkhfInjected != 0 {
		result, _, _ := callNextHookScreen.Call(0, uintptr(code), wParam, lParam)
		return result
	}
	if info.VKCode == 'C' && !up && hookModifiers.ctrl && hookModifiers.alt && hookModifiers.shift {
		v.queueInput(screenview.InputEvent{T: "release_all"})
		go v.setControlling(false)
		return 1
	}
	v.queueInput(screenview.InputEvent{
		T:    "key",
		VK:   int(info.VKCode),
		Scan: int(info.ScanCode),
		Ext:  info.Flags&llkhfExtended != 0,
		Down: !up,
	})
	return 1
}

func (v *screenViewer) handlePanMessage(hwnd win.HWND, msg uint32, wParam uintptr) bool {
	switch msg {
	case win.WM_LBUTTONDOWN:
		if !v.screenCanPan() {
			return false
		}
		var point win.POINT
		if !win.GetCursorPos(&point) {
			return false
		}
		v.panActive = true
		v.panStart = point
		v.panStartX = screenScrollPosition(v.viewport.Handle(), win.SB_HORZ)
		v.panStartY = screenScrollPosition(v.viewport.Handle(), win.SB_VERT)
		win.SetCapture(hwnd)
		return true

	case win.WM_MOUSEMOVE:
		if !v.panActive || wParam&win.MK_LBUTTON == 0 {
			return false
		}
		var point win.POINT
		if win.GetCursorPos(&point) {
			v.setScreenPan(v.panStartX-int(point.X-v.panStart.X), v.panStartY-int(point.Y-v.panStart.Y))
		}
		return true

	case win.WM_LBUTTONUP:
		if !v.panActive {
			return false
		}
		v.panActive = false
		win.ReleaseCapture()
		return true

	case win.WM_CAPTURECHANGED:
		v.panActive = false
	}
	return false
}

func (v *screenViewer) screenCanPan() bool {
	if v.viewport == nil {
		return false
	}
	bounds := v.viewport.ClientBoundsPixels()
	return v.renderWidth > bounds.Width || v.renderHeight > bounds.Height
}

func screenScrollPosition(hwnd win.HWND, bar int32) int {
	info := win.SCROLLINFO{CbSize: uint32(unsafe.Sizeof(win.SCROLLINFO{})), FMask: win.SIF_POS}
	if !win.GetScrollInfo(hwnd, bar, &info) {
		return 0
	}
	return int(info.NPos)
}

func screenScrollLimit(hwnd win.HWND, bar int32) int {
	info := win.SCROLLINFO{CbSize: uint32(unsafe.Sizeof(win.SCROLLINFO{})), FMask: win.SIF_PAGE | win.SIF_RANGE}
	if !win.GetScrollInfo(hwnd, bar, &info) {
		return 0
	}
	return max(0, int(info.NMax+1-int32(info.NPage)))
}

func (v *screenViewer) setScreenPan(x, y int) {
	if v.viewport == nil || v.canvas == nil {
		return
	}
	hwnd := v.viewport.Handle()
	x = max(0, min(screenScrollLimit(hwnd, win.SB_HORZ), x))
	y = max(0, min(screenScrollLimit(hwnd, win.SB_VERT), y))
	for _, value := range []struct {
		bar int32
		pos int
	}{{win.SB_HORZ, x}, {win.SB_VERT, y}} {
		info := win.SCROLLINFO{CbSize: uint32(unsafe.Sizeof(win.SCROLLINFO{})), FMask: win.SIF_POS, NPos: int32(value.pos)}
		win.SetScrollInfo(hwnd, value.bar, &info, true)
	}
	content := win.GetParent(v.canvas.Handle())
	win.SetWindowPos(content, 0, int32(-x), int32(-y), 0, 0, win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE)
}

func (v *screenViewer) handleZoomMessage(msg uint32, wParam, lParam uintptr) bool {
	switch msg {
	case win.WM_MOUSEWHEEL:
		delta := int16(uint16(wParam >> 16))
		if delta == 0 {
			return false
		}
		v.zoomBy(math.Pow(1.1, float64(delta)/120.0))
		return true

	case wmGesture:
		info := screenGestureInfo{Size: uint32(unsafe.Sizeof(screenGestureInfo{}))}
		ok, _, _ := getGestureInfoScreen.Call(lParam, uintptr(unsafe.Pointer(&info)))
		if ok == 0 || info.ID != gidZoom {
			return false
		}
		_, _, _ = closeGestureInfoScreen.Call(lParam)
		if info.Flags&gfBegin != 0 || v.gestureStartDistance == 0 {
			v.gestureStartDistance = info.Arguments
			v.gestureStartZoom = v.effectiveZoom()
			return true
		}
		if info.Arguments > 0 && v.gestureStartDistance > 0 {
			v.setZoom(v.gestureStartZoom*float64(info.Arguments)/float64(v.gestureStartDistance), true)
		}
		return true
	}
	return false
}

var minFPSChoices = []string{"0 (lossless)", "15", "24", "30", "45", "60"}

// parseMinFPS accepts "0 (lossless)", "30", "30 fps" and similar.
func parseMinFPS(text string) (int, bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	if index := strings.IndexAny(text, " (f"); index > 0 {
		text = text[:index]
	}
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || value < 0 || value > screenview.MaxMinFPS {
		return 0, false
	}
	return value, true
}

func formatMinFPS(value int) string {
	if value == 0 {
		return minFPSChoices[0]
	}
	return strconv.Itoa(value)
}

func (v *screenViewer) showMinFPS() {
	if v.minFPSBox == nil {
		return
	}
	v.zoomControlUpdating = true
	_ = v.minFPSBox.SetText(formatMinFPS(v.minFPS))
	v.zoomControlUpdating = false
}

func (v *screenViewer) applyMinFPSSelection() {
	if v.minFPSBox == nil || v.zoomControlUpdating {
		return
	}
	value, ok := parseMinFPS(v.minFPSBox.Text())
	if !ok {
		v.setStatus(fmt.Sprintf("Min fps must be a whole number from 0 (lossless) to %d.", screenview.MaxMinFPS))
		v.showMinFPS()
		return
	}
	if value == v.minFPS {
		return
	}
	v.minFPS = value
	v.showMinFPS()
	if err := saveViewerMinFPS(v.owner.currentConfig().SelectedDestination, value); err != nil {
		v.setStatus("Could not save Min fps: " + err.Error())
	}
	client := v.liveClient()
	if client == nil || client.Legacy() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.SetMinFPS(ctx, value); err != nil {
			v.setStatus("Could not change Min fps: " + err.Error())
			return
		}
		if value == 0 {
			v.setStatus("Min fps 0: the Work PC now sends everything losslessly.")
		} else {
			v.setStatus(fmt.Sprintf("Min fps %d: the Work PC compresses changing areas only when fewer frames arrive.", value))
		}
	}()
}

func (v *screenViewer) applyZoomSelection() {
	if v.zoomBox == nil || v.zoomControlUpdating {
		return
	}
	value, ok := parseScreenZoom(v.zoomBox.Text())
	if !ok {
		v.setStatus("Zoom must be Auto Fit or a value from 10% through 1600%.")
		return
	}
	v.setZoom(value, false)
}

func parseScreenZoom(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "Auto Fit") || strings.EqualFold(text, "Auto") || strings.EqualFold(text, "Fit") {
		return 0, true
	}
	text = strings.TrimSpace(strings.TrimSuffix(text, "%"))
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < minScreenZoom*100 || value > maxScreenZoom*100 {
		return 0, false
	}
	return value / 100, true
}

func (v *screenViewer) zoomBy(factor float64) {
	if factor <= 0 {
		return
	}
	v.setZoom(v.effectiveZoom()*factor, true)
}

func (v *screenViewer) setZoom(value float64, updateControl bool) {
	if value != 0 {
		value = math.Max(minScreenZoom, math.Min(maxScreenZoom, value))
	}
	v.mu.Lock()
	v.zoom = value
	v.mu.Unlock()
	if updateControl && v.zoomBox != nil {
		label := "Auto Fit"
		if value > 0 {
			label = strconv.FormatFloat(value*100, 'f', 1, 64)
			label = strings.TrimSuffix(strings.TrimSuffix(label, "0"), ".") + "%"
		}
		v.zoomControlUpdating = true
		_ = v.zoomBox.SetText(label)
		v.zoomControlUpdating = false
	}
	v.layoutScreen()
}

func (v *screenViewer) frameSize() (int, int) {
	v.frameMu.Lock()
	defer v.frameMu.Unlock()
	return v.frameWidth, v.frameHeight
}

func (v *screenViewer) effectiveZoom() float64 {
	v.mu.Lock()
	zoom := v.zoom
	v.mu.Unlock()
	if zoom > 0 {
		return zoom
	}
	width, height := v.frameSize()
	if width == 0 || v.viewport == nil {
		return 1
	}
	bounds := v.viewport.ClientBoundsPixels()
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return 1
	}
	return math.Min(float64(bounds.Width)/float64(width), float64(bounds.Height)/float64(height))
}

func (v *screenViewer) layoutScreen() {
	if v.viewport == nil || v.canvas == nil {
		return
	}
	sourceWidth, sourceHeight := v.frameSize()
	if sourceWidth == 0 {
		return
	}
	v.mu.Lock()
	zoom := v.zoom
	v.mu.Unlock()
	autoFit := zoom == 0
	if autoFit {
		bounds := v.viewport.ClientBoundsPixels()
		if bounds.Width <= 0 || bounds.Height <= 0 {
			return
		}
		zoom = math.Min(float64(bounds.Width)/float64(sourceWidth), float64(bounds.Height)/float64(sourceHeight))
	}
	width := max(1, int(math.Round(float64(sourceWidth)*zoom)))
	height := max(1, int(math.Round(float64(sourceHeight)*zoom)))
	if width == v.renderWidth && height == v.renderHeight {
		if autoFit {
			v.setScreenPan(0, 0)
		}
		return
	}
	v.renderWidth = width
	v.renderHeight = height
	size := walk.Size{Width: width, Height: height}
	_ = v.canvas.SetMinMaxSizePixels(size, size)
	_ = v.canvas.SetSizePixels(size)
	v.viewport.RequestLayout()
	if autoFit {
		v.setScreenPan(0, 0)
	}
	win.InvalidateRect(v.canvas.Handle(), nil, false)
}

func (v *screenViewer) paintScreen(canvas *walk.Canvas, _ walk.Rectangle) error {
	if v.canvas == nil {
		return nil
	}
	bounds := v.canvas.ClientBoundsPixels()
	hdc := canvas.HDC()
	v.frameMu.Lock()
	defer v.frameMu.Unlock()
	if v.frameDIB == 0 || v.frameWidth == 0 {
		return nil
	}
	// walk's paint DC does not accept StretchDIBits, so stretch the DIB
	// section from a memory DC instead.
	frameDC := win.CreateCompatibleDC(hdc)
	if frameDC == 0 {
		return nil
	}
	previousBitmap := win.SelectObject(frameDC, win.HGDIOBJ(v.frameDIB))
	setStretchModeScreen.Call(uintptr(hdc), halftone)
	setBrushOrgScreen.Call(uintptr(hdc), 0, 0, 0)
	win.StretchBlt(hdc, 0, 0, int32(bounds.Width), int32(bounds.Height), frameDC, 0, 0, int32(v.frameWidth), int32(v.frameHeight), win.SRCCOPY)
	win.SelectObject(frameDC, previousBitmap)
	win.DeleteDC(frameDC)
	if !v.controlling.Load() && v.cursor.Visible && v.cursorBitmap != 0 && v.cursor.Shape != nil {
		scaleX := float64(bounds.Width) / float64(v.frameWidth)
		scaleY := float64(bounds.Height) / float64(v.frameHeight)
		shape := v.cursor.Shape.Bounds()
		x := int(float64(v.cursor.X-v.cursor.HotX) * scaleX)
		y := int(float64(v.cursor.Y-v.cursor.HotY) * scaleY)
		width := max(1, int(float64(shape.Dx())*math.Max(scaleX, 0.5)))
		height := max(1, int(float64(shape.Dy())*math.Max(scaleY, 0.5)))
		memoryDC := win.CreateCompatibleDC(hdc)
		if memoryDC != 0 {
			previous := win.SelectObject(memoryDC, win.HGDIOBJ(v.cursorBitmap))
			blend := uintptr(acSrcOver) | 255<<16 | uintptr(acSrcAlpha)<<24
			alphaBlendScreen.Call(uintptr(hdc), uintptr(x), uintptr(y), uintptr(width), uintptr(height),
				uintptr(memoryDC), 0, 0, uintptr(shape.Dx()), uintptr(shape.Dy()), blend)
			win.SelectObject(memoryDC, previous)
			win.DeleteDC(memoryDC)
		}
	}
	return nil
}

// updateCursorResources builds the overlay bitmap and the local cursor for a
// new remote pointer shape. It runs on the UI thread.
func (v *screenViewer) updateCursorResources(cursor screenview.CursorState) {
	if cursor.ShapeID == v.cursorShapeID || cursor.Shape == nil {
		return
	}
	v.releaseCursorResources()
	v.cursorShapeID = cursor.ShapeID
	bitmap, bits := createPremultipliedDIB(cursor.Shape)
	if bitmap == 0 {
		return
	}
	v.cursorBitmap = bitmap
	_ = bits
	// The local cursor gets a real AND mask (transparent where the pointer
	// is) and straight-alpha color that is black where transparent. With an
	// empty mask, Windows draws every pixel opaque whenever it draws the
	// cursor without its alpha channel, which shows as a black box over
	// anything that is not black.
	width, height := cursor.Shape.Bounds().Dx(), cursor.Shape.Bounds().Dy()
	color, _ := createCursorColorDIB(cursor.Shape)
	if color == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(color))
	maskBits := cursorMaskBits(cursor.Shape)
	mask := win.CreateBitmap(int32(width), int32(height), 1, 1, unsafe.Pointer(&maskBits[0]))
	if mask != 0 {
		info := struct {
			IsIcon int32
			HotX   uint32
			HotY   uint32
			_      uint32
			Mask   win.HBITMAP
			Color  win.HBITMAP
		}{HotX: uint32(cursor.HotX), HotY: uint32(cursor.HotY), Mask: mask, Color: color}
		handle, _, _ := createIconIndirect.Call(uintptr(unsafe.Pointer(&info)))
		win.DeleteObject(win.HGDIOBJ(mask))
		v.localCursor = win.HCURSOR(handle)
	}
}

func (v *screenViewer) releaseCursorResources() {
	if v.cursorBitmap != 0 {
		win.DeleteObject(win.HGDIOBJ(v.cursorBitmap))
		v.cursorBitmap = 0
	}
	if v.localCursor != 0 {
		destroyCursorScreen.Call(uintptr(v.localCursor))
		v.localCursor = 0
	}
	v.cursorShapeID = 0
}

// createFrameDIB creates a top-down 32-bit DIB section for the frame and
// returns its pixel memory.
func createFrameDIB(width, height int) (win.HBITMAP, []byte) {
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      -int32(height),
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	var pixels unsafe.Pointer
	bitmap := win.CreateDIBSection(0, &header, win.DIB_RGB_COLORS, &pixels, 0, 0)
	if bitmap == 0 || pixels == nil {
		return 0, nil
	}
	return bitmap, unsafe.Slice((*byte)(pixels), width*height*4)
}

func createPremultipliedDIB(shape *image.RGBA) (win.HBITMAP, []byte) {
	width, height := shape.Bounds().Dx(), shape.Bounds().Dy()
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      -int32(height),
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	var pixels unsafe.Pointer
	bitmap := win.CreateDIBSection(0, &header, win.DIB_RGB_COLORS, &pixels, 0, 0)
	if bitmap == 0 || pixels == nil {
		return 0, nil
	}
	bits := unsafe.Slice((*byte)(pixels), width*height*4)
	for i := 0; i+3 < len(bits) && i+3 < len(shape.Pix); i += 4 {
		alpha := uint32(shape.Pix[i+3])
		bits[i] = byte(uint32(shape.Pix[i+2]) * alpha / 255)
		bits[i+1] = byte(uint32(shape.Pix[i+1]) * alpha / 255)
		bits[i+2] = byte(uint32(shape.Pix[i]) * alpha / 255)
		bits[i+3] = byte(alpha)
	}
	return bitmap, bits
}

// createCursorColorDIB returns the pointer's color as a 32-bit DIB section
// with straight alpha, black where the pointer is fully transparent.
func createCursorColorDIB(shape *image.RGBA) (win.HBITMAP, []byte) {
	width, height := shape.Bounds().Dx(), shape.Bounds().Dy()
	header := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      -int32(height),
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	var pixels unsafe.Pointer
	bitmap := win.CreateDIBSection(0, &header, win.DIB_RGB_COLORS, &pixels, 0, 0)
	if bitmap == 0 || pixels == nil {
		return 0, nil
	}
	bits := unsafe.Slice((*byte)(pixels), width*height*4)
	for i := 0; i+3 < len(bits) && i+3 < len(shape.Pix); i += 4 {
		if shape.Pix[i+3] == 0 {
			bits[i], bits[i+1], bits[i+2], bits[i+3] = 0, 0, 0, 0
			continue
		}
		bits[i], bits[i+1], bits[i+2], bits[i+3] = shape.Pix[i+2], shape.Pix[i+1], shape.Pix[i], shape.Pix[i+3]
	}
	return bitmap, bits
}

// cursorMaskBits returns a monochrome AND mask for the pointer: 1 (keep the
// screen) where it is mostly transparent, rows padded to 16 bits.
func cursorMaskBits(shape *image.RGBA) []byte {
	width, height := shape.Bounds().Dx(), shape.Bounds().Dy()
	stride := (width + 15) / 16 * 2
	bits := make([]byte, max(1, stride*height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if shape.Pix[shape.PixOffset(x, y)+3] < 128 {
				bits[y*stride+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	return bits
}

func (v *screenViewer) sizeWindowForFirstFrame(frameWidth, frameHeight int) {
	v.mu.Lock()
	if v.startupSized {
		v.mu.Unlock()
		return
	}
	v.startupSized = true
	v.mu.Unlock()
	if v.mw == nil || v.viewport == nil || frameWidth <= 0 || frameHeight <= 0 {
		return
	}
	var monitor win.MONITORINFO
	monitor.CbSize = uint32(unsafe.Sizeof(monitor))
	if !win.GetMonitorInfo(win.MonitorFromWindow(v.mw.Handle(), win.MONITOR_DEFAULTTONEAREST), &monitor) {
		return
	}
	work := walk.Rectangle{
		X:      int(monitor.RcWork.Left),
		Y:      int(monitor.RcWork.Top),
		Width:  int(monitor.RcWork.Right - monitor.RcWork.Left),
		Height: int(monitor.RcWork.Bottom - monitor.RcWork.Top),
	}
	bounds, maximize := screenViewerStartupBounds(frameWidth, frameHeight, v.mw.BoundsPixels(), v.viewport.ClientBoundsPixels(), work, walk.Size{Width: 720, Height: 500})
	if maximize {
		win.ShowWindow(v.mw.Handle(), win.SW_MAXIMIZE)
		v.activateWindow()
		return
	}
	win.ShowWindow(v.mw.Handle(), win.SW_RESTORE)
	_ = v.mw.SetBoundsPixels(bounds)
	v.activateWindow()
}

func (v *screenViewer) activateWindow() {
	if v.mw == nil {
		return
	}
	win.BringWindowToTop(v.mw.Handle())
	win.SetForegroundWindow(v.mw.Handle())
	win.SetActiveWindow(v.mw.Handle())
	if v.canvas != nil {
		_ = v.canvas.SetFocus()
	}
}

func screenViewerStartupBounds(frameWidth, frameHeight int, window, viewport, work walk.Rectangle, minimum walk.Size) (walk.Rectangle, bool) {
	chromeWidth := max(0, window.Width-viewport.Width)
	chromeHeight := max(0, window.Height-viewport.Height)
	desiredWidth := max(minimum.Width, frameWidth+chromeWidth)
	desiredHeight := max(minimum.Height, frameHeight+chromeHeight)
	if desiredWidth >= work.Width || desiredHeight >= work.Height {
		return work, true
	}
	return walk.Rectangle{
		X:      work.X + (work.Width-desiredWidth)/2,
		Y:      work.Y + (work.Height-desiredHeight)/2,
		Width:  desiredWidth,
		Height: desiredHeight,
	}, false
}

func (v *screenViewer) toggleFullscreen() {
	_ = v.mw.SetFullscreen(!v.mw.Fullscreen())
}

func (v *screenViewer) savePNG() {
	client := v.liveClient()
	var frame *image.RGBA
	if client != nil {
		frame, _, _ = client.Snapshot()
	}
	if frame == nil {
		walk.MsgBox(v.mw, "DeskFerry Screen Viewer", "Wait for the Work PC's screen before saving.", walk.MsgBoxOK|walk.MsgBoxIconInformation)
		return
	}
	dialog := new(walk.FileDialog)
	dialog.Title = "Save DeskFerry Screenshot"
	dialog.Filter = "PNG images (*.png)|*.png"
	dialog.FilePath = filepath.Join(defaultScreenshotDirectory(), "DeskFerry-"+time.Now().Format("20060102-150405")+".png")
	accepted, err := dialog.ShowSave(v.mw)
	if err != nil || !accepted {
		if err != nil {
			walk.MsgBox(v.mw, "DeskFerry Screen Viewer", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		}
		return
	}
	file, err := os.Create(dialog.FilePath)
	if err == nil {
		err = png.Encode(file, frame)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		walk.MsgBox(v.mw, "DeskFerry Screen Viewer", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}
	v.setStatus("Saved " + dialog.FilePath)
}

func defaultScreenshotDirectory() string {
	if home, err := os.UserHomeDir(); err == nil {
		pictures := filepath.Join(home, "Pictures")
		if info, statErr := os.Stat(pictures); statErr == nil && info.IsDir() {
			return pictures
		}
	}
	return "."
}
