//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"deskferry/internal/buildinfo"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

type screenViewer struct {
	owner    *clientApp
	mw       *walk.MainWindow
	image    *walk.ImageView
	status   *walk.Label
	interval *walk.ComboBox

	mu        sync.Mutex
	cancel    context.CancelFunc
	frame     *image.RGBA
	bitmap    *walk.Bitmap
	closed    bool
	streaming bool
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
			Composite{Layout: Flow{Spacing: 7}, Children: []Widget{
				PushButton{Text: "Capture Once", OnClicked: func() { viewer.start(false) }},
				PushButton{Text: "Start Stream", OnClicked: func() { viewer.start(true) }},
				PushButton{Text: "Stop", OnClicked: viewer.stop},
				Label{Text: "Interval"},
				ComboBox{AssignTo: &viewer.interval, Model: []string{"0.5 seconds", "1 second", "2 seconds", "5 seconds"}, CurrentIndex: 1},
				PushButton{Text: "Full Screen", OnClicked: viewer.toggleFullscreen},
				PushButton{Text: "Save PNG", OnClicked: viewer.savePNG},
			}},
			Label{AssignTo: &viewer.status, Text: "Ready. Work-side screen viewing must be enabled."},
			ImageView{AssignTo: &viewer.image, Mode: ImageViewModeShrink, StretchFactor: 1},
		},
	}
	if err := window.Create(); err != nil {
		a.showError(err)
		return
	}
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
	})
	viewer.mw.Show()
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
	conn, relayAddr, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
	if err != nil {
		if ctx.Err() == nil {
			v.setStatus("Screen connection failed: " + err.Error())
		}
		return
	}
	defer conn.Close()
	if err := screenview.WriteRequest(conn, request); err != nil {
		v.setStatus("Could not start screen capture: " + err.Error())
		return
	}
	v.setStatus(fmt.Sprintf("Connected through %s; waiting for the first frame...", relayAddr))
	var canvas *image.RGBA
	for {
		frame, payloads, err := screenview.ReadFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) && request.Mode == screenview.ModeSingle && canvas != nil {
				v.setStatus("Screenshot captured.")
				return
			}
			v.setStatus("Screen stream ended: " + err.Error())
			return
		}
		canvas, err = screenview.ApplyFrame(canvas, frame, payloads)
		if err != nil {
			v.setStatus("Screen frame failed: " + err.Error())
			return
		}
		v.showFrame(canvas, frame.Seq, len(frame.Rects), request.Mode == screenview.ModeStream)
		if request.Mode == screenview.ModeSingle {
			v.setStatus("Screenshot captured.")
			return
		}
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
		_ = v.image.SetImage(bitmap)
		if previous != nil {
			previous.Dispose()
		}
		if streaming {
			_ = v.status.SetText(fmt.Sprintf("Streaming frame %d (%d changed tiles).", sequence, changedTiles))
		}
	})
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
