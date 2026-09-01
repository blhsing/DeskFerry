//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"deskferry/internal/buildinfo"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

type screenViewer struct {
	owner    *clientApp
	mw       *walk.MainWindow
	viewport *walk.ScrollView
	canvas   *walk.CustomWidget
	status   *walk.Label
	interval *walk.ComboBox
	zoomBox  *walk.ComboBox

	mu                   sync.Mutex
	cancel               context.CancelFunc
	frame                *image.RGBA
	bitmap               *walk.Bitmap
	closed               bool
	streaming            bool
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
}

const (
	minScreenZoom = 0.10
	maxScreenZoom = 16.0
	wmGesture     = 0x0119
	gidZoom       = 3
	gfBegin       = 1
)

var (
	user32ScreenViewer     = windows.NewLazySystemDLL("user32.dll")
	getGestureInfoScreen   = user32ScreenViewer.NewProc("GetGestureInfo")
	closeGestureInfoScreen = user32ScreenViewer.NewProc("CloseGestureInfoHandle")
	setGestureConfigScreen = user32ScreenViewer.NewProc("SetGestureConfig")
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

type screenZoomCanvas struct {
	*walk.CustomWidget
	viewer *screenViewer
}

func (w *screenZoomCanvas) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if w.viewer.handlePanMessage(hwnd, msg, wParam) {
		return 0
	}
	if w.viewer.handleZoomMessage(msg, wParam, lParam) {
		return 0
	}
	return w.CustomWidget.WndProc(hwnd, msg, wParam, lParam)
}

type screenZoomScrollView struct {
	*walk.ScrollView
	viewer *screenViewer
}

func (w *screenZoomScrollView) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if w.viewer.handleZoomMessage(msg, wParam, lParam) {
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
	viewer := &screenViewer{owner: a}
	window := MainWindow{
		AssignTo: &viewer.mw,
		Title:    "DeskFerry " + buildinfo.Version + " Screen Viewer - " + cfg.SelectedDestination,
		MinSize:  Size{Width: 720, Height: 500},
		Size:     Size{Width: 1100, Height: 760},
		Layout:   VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 8}, Spacing: 7},
		Children: []Widget{
			Composite{MaxSize: Size{Height: 30}, Layout: HBox{MarginsZero: true, Spacing: 7}, Children: []Widget{
				PushButton{Text: "Capture Once", OnClicked: func() { viewer.start(false) }},
				PushButton{Text: "Start Stream", OnClicked: func() { viewer.start(true) }},
				PushButton{Text: "Stop", OnClicked: viewer.stop},
				Label{Text: "Interval"},
				ComboBox{AssignTo: &viewer.interval, Model: []string{"0.5 seconds", "1 second", "2 seconds", "5 seconds"}, CurrentIndex: 1},
				Label{Text: "Zoom"},
				ComboBox{
					AssignTo:              &viewer.zoomBox,
					Editable:              true,
					Model:                 []string{"Auto Fit", "25%", "50%", "75%", "100%", "125%", "150%", "200%", "300%", "400%"},
					CurrentIndex:          0,
					MinSize:               Size{Width: 95},
					StretchFactor:         1,
					OnCurrentIndexChanged: viewer.applyZoomSelection,
					OnEditingFinished:     viewer.applyZoomSelection,
				},
				PushButton{Text: "Full Screen", OnClicked: viewer.toggleFullscreen},
				PushButton{Text: "Save PNG", OnClicked: viewer.savePNG},
			}},
			Label{AssignTo: &viewer.status, Text: "Ready. Work-side screen viewing must be enabled."},
			ScrollView{
				AssignTo:      &viewer.viewport,
				StretchFactor: 1,
				Background:    SolidColorBrush{Color: walk.RGB(0, 0, 0)},
				Layout:        Grid{MarginsZero: true},
				OnSizeChanged: viewer.layoutScreen,
				Children: []Widget{
					CustomWidget{
						AssignTo:            &viewer.canvas,
						Alignment:           AlignHCenterVCenter,
						Background:          SolidColorBrush{Color: walk.RGB(0, 0, 0)},
						MinSize:             Size{Width: 1, Height: 1},
						PaintPixels:         viewer.paintScreen,
						InvalidatesOnResize: true,
					},
				},
			},
		},
	}
	if err := window.Create(); err != nil {
		a.showError(err)
		return
	}
	win.SetWindowLongPtr(viewer.mw.Handle(), win.GWLP_HWNDPARENT, uintptr(a.mw.Handle()))
	zoomCanvas := &screenZoomCanvas{CustomWidget: viewer.canvas, viewer: viewer}
	if err := walk.InitWrapperWindow(zoomCanvas); err != nil {
		viewer.mw.Dispose()
		a.showError(err)
		return
	}
	zoomViewport := &screenZoomScrollView{ScrollView: viewer.viewport, viewer: viewer}
	if err := walk.InitWrapperWindow(zoomViewport); err != nil {
		viewer.mw.Dispose()
		a.showError(err)
		return
	}
	enableScreenZoomGesture(viewer.canvas.Handle())
	enableScreenZoomGesture(viewer.viewport.Handle())
	win.EnableWindow(a.mw.Handle(), false)
	viewer.mw.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		viewer.mu.Lock()
		viewer.closed = true
		cancel := viewer.cancel
		viewer.cancel = nil
		bitmap := viewer.bitmap
		viewer.bitmap = nil
		viewer.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if bitmap != nil {
			bitmap.Dispose()
		}
		win.EnableWindow(a.mw.Handle(), true)
		win.SetForegroundWindow(a.mw.Handle())
	})
	viewer.mw.Show()
	win.ShowWindow(viewer.mw.Handle(), win.SW_MAXIMIZE)
	viewer.activateWindow()
	viewer.start(false)
}

func (v *screenViewer) selectedInterval() int {
	values := []int{500, 1000, 2000, 5000}
	index := v.interval.CurrentIndex()
	if index < 0 || index >= len(values) {
		return screenview.DefaultIntervalMS
	}
	return values[index]
}

func enableScreenZoomGesture(hwnd win.HWND) {
	config := screenGestureConfig{ID: gidZoom, Want: 1}
	_, _, _ = setGestureConfigScreen.Call(
		uintptr(hwnd),
		0,
		1,
		uintptr(unsafe.Pointer(&config)),
		unsafe.Sizeof(config),
	)
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

func (v *screenViewer) effectiveZoom() float64 {
	v.mu.Lock()
	zoom := v.zoom
	frame := v.frame
	v.mu.Unlock()
	if zoom > 0 {
		return zoom
	}
	if frame == nil || v.viewport == nil {
		return 1
	}
	bounds := v.viewport.ClientBoundsPixels()
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return 1
	}
	return math.Min(float64(bounds.Width)/float64(frame.Bounds().Dx()), float64(bounds.Height)/float64(frame.Bounds().Dy()))
}

func (v *screenViewer) layoutScreen() {
	if v.viewport == nil || v.canvas == nil {
		return
	}
	v.mu.Lock()
	frame := v.frame
	zoom := v.zoom
	v.mu.Unlock()
	if frame == nil {
		return
	}
	sourceWidth := frame.Bounds().Dx()
	sourceHeight := frame.Bounds().Dy()
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
		_ = v.canvas.Invalidate()
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
	_ = v.canvas.Invalidate()
}

func (v *screenViewer) paintScreen(canvas *walk.Canvas, _ walk.Rectangle) error {
	v.mu.Lock()
	bitmap := v.bitmap
	v.mu.Unlock()
	if bitmap == nil || v.canvas == nil {
		return nil
	}
	bounds := v.canvas.ClientBoundsPixels()
	return canvas.DrawImageStretchedPixels(bitmap, walk.Rectangle{Width: bounds.Width, Height: bounds.Height})
}

func (v *screenViewer) start(stream bool) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	if v.cancel != nil {
		v.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	v.streaming = stream
	v.mu.Unlock()
	mode := screenview.ModeSingle
	if stream {
		mode = screenview.ModeStream
	}
	v.setStatus("Connecting to the Work screen service...")
	request := screenview.Request{Mode: mode, IntervalMS: v.selectedInterval(), TileSize: screenview.DefaultTileSize}
	go v.receive(ctx, request)
}

func (v *screenViewer) receive(ctx context.Context, request screenview.Request) {
	cfg := v.owner.currentConfig()
	conn, route, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		if ctx.Err() == nil {
			v.setStatus("Screen connection failed: " + err.Error())
		}
		return
	}
	defer conn.Close()
	v.owner.appendLog("Screen connection selected relay=%s proxy=%s protocol=%s relay_protocol=%s.", route.RelayAddr, route.Proxy, route.Protocol, route.RelayProtocol)
	v.setStatus(fmt.Sprintf("Connected through %s via %s (%s); waiting for the first frame...", route.RelayAddr, route.Proxy, route.Protocol))
	err = screenview.Receive(conn, request, func(frame screenview.Frame, canvas *image.RGBA) error {
		v.showFrame(canvas, frame.Seq, len(frame.Rects), request.Mode == screenview.ModeStream)
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		v.setStatus("Screen stream ended: " + err.Error())
		return
	}
	if request.Mode == screenview.ModeSingle {
		v.setStatus("Screenshot captured.")
	}
}

func (v *screenViewer) showFrame(source *image.RGBA, sequence uint64, changedTiles int, streaming bool) {
	copyImage := image.NewRGBA(source.Bounds())
	draw.Draw(copyImage, copyImage.Bounds(), source, source.Bounds().Min, draw.Src)
	v.mu.Lock()
	v.frame = copyImage
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return
	}
	v.mw.Synchronize(func() {
		bitmap, err := walk.NewBitmapFromImage(copyImage)
		if err != nil {
			v.setStatus("Could not display screen frame: " + err.Error())
			return
		}
		v.mu.Lock()
		previous := v.bitmap
		v.bitmap = bitmap
		v.mu.Unlock()
		v.sizeWindowForFirstFrame(copyImage.Bounds().Dx(), copyImage.Bounds().Dy())
		v.layoutScreen()
		_ = v.canvas.Invalidate()
		if previous != nil {
			previous.Dispose()
		}
		if streaming {
			_ = v.status.SetText(fmt.Sprintf("Streaming frame %d (%d changed tiles).", sequence, changedTiles))
		}
	})
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
	bounds, maximize := screenViewerStartupBounds(
		frameWidth,
		frameHeight,
		v.mw.BoundsPixels(),
		v.viewport.ClientBoundsPixels(),
		work,
		walk.Size{Width: 720, Height: 500},
	)
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
	if v.zoomBox != nil {
		_ = v.zoomBox.SetFocus()
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

func (v *screenViewer) stop() {
	v.mu.Lock()
	cancel := v.cancel
	v.cancel = nil
	v.streaming = false
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	v.setStatus("Screen stream stopped.")
}

func (v *screenViewer) toggleFullscreen() {
	_ = v.mw.SetFullscreen(!v.mw.Fullscreen())
}

func (v *screenViewer) savePNG() {
	v.mu.Lock()
	if v.frame == nil {
		v.mu.Unlock()
		walk.MsgBox(v.mw, "DeskFerry Screen Viewer", "Capture a screenshot before saving.", walk.MsgBoxOK|walk.MsgBoxIconInformation)
		return
	}
	frame := image.NewRGBA(v.frame.Bounds())
	draw.Draw(frame, frame.Bounds(), v.frame, v.frame.Bounds().Min, draw.Src)
	v.mu.Unlock()
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

func (v *screenViewer) setStatus(text string) {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed || v.mw == nil {
		return
	}
	v.mw.Synchronize(func() {
		_ = v.status.SetText(text)
	})
}
