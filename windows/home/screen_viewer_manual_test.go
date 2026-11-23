//go:build windows

package homewindows

import (
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"deskferry/internal/screenview"
)

// TestManualScreenViewerWindow opens the real viewer window for a saved
// destination for a few seconds and reports what it received and drew. Set
// DESKFERRY_MANUAL_VIEWER_DESTINATION to a destination name.
func TestManualScreenViewerWindow(t *testing.T) {
	name := os.Getenv("DESKFERRY_MANUAL_VIEWER_DESTINATION")
	if name == "" {
		t.Skip("set DESKFERRY_MANUAL_VIEWER_DESTINATION to open the viewer")
	}
	runtime.LockOSThread()
	activateAppManifest(t)
	cfg, err := loadConfig("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := selectDestinationConfig(&cfg, name); err != nil {
		t.Fatal(err)
	}
	owner := &clientApp{cfg: cfg}
	viewer := &screenViewer{owner: owner, inputs: make(chan screenview.InputEvent, 2048)}
	if err := viewer.createWindow("DeskFerry viewer test", nil); err != nil {
		t.Fatal(err)
	}
	viewer.mw.Show()
	viewer.start()
	seconds := 15
	if value, err := strconv.Atoi(os.Getenv("DESKFERRY_MANUAL_VIEWER_SECONDS")); err == nil && value > 0 {
		seconds = value
	}
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			viewer.mw.Synchronize(func() { t.Logf("%s | %s", viewer.stats.Text(), viewer.status.Text()) })
		}
	}()
	time.AfterFunc(time.Duration(seconds)*time.Second, func() { viewer.mw.Synchronize(func() { report(t, viewer); viewer.mw.Close() }) })
	viewer.mw.Run()
}

func report(t *testing.T, viewer *screenViewer) {
	if path := os.Getenv("DESKFERRY_MANUAL_VIEWER_CAPTURE"); path != "" {
		if shot, err := screenview.CaptureDesktop(); err == nil {
			if file, err := os.Create(path); err == nil {
				_ = png.Encode(file, shot)
				_ = file.Close()
			}
		}
	}
	viewer.frameMu.Lock()
	width, height, haveBuffer := viewer.frameWidth, viewer.frameHeight, viewer.bgra != nil
	nonBlack := 0
	for i := 0; haveBuffer && i < len(viewer.bgra); i += 4 * 101 {
		if viewer.bgra[i] > 16 || viewer.bgra[i+1] > 16 || viewer.bgra[i+2] > 16 {
			nonBlack++
		}
	}
	viewer.frameMu.Unlock()
	client := viewer.liveClient()
	t.Logf("status=%q stats=%q", viewer.status.Text(), viewer.stats.Text())
	t.Logf("client=%t legacy=%t frames=%d buffer=%dx%d nonblack=%d render=%dx%d canvas=%v",
		client != nil, client != nil && client.Legacy(), viewer.framesShown.Load(), width, height, nonBlack,
		viewer.renderWidth, viewer.renderHeight, viewer.canvas.BoundsPixels())
}

// activateAppManifest gives the test thread the app's common-controls v6
// manifest, which walk's tooltips need and test binaries lack.
func activateAppManifest(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "app.manifest"))
	if err != nil {
		t.Fatal(err)
	}
	source, _ := windows.UTF16PtrFromString(path)
	type actCtx struct {
		Size                                       uint32
		Flags                                      uint32
		Source                                     *uint16
		Arch                                       uint16
		Lang                                       uint16
		AssemblyDir, ResourceName, ApplicationName *uint16
		Module                                     uintptr
	}
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	ctx := actCtx{Size: uint32(unsafe.Sizeof(actCtx{})), Source: source}
	handle, _, callErr := kernel32.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&ctx)))
	if handle == ^uintptr(0) {
		t.Fatalf("create activation context: %v", callErr)
	}
	var cookie uintptr
	if ok, _, callErr := kernel32.NewProc("ActivateActCtx").Call(handle, uintptr(unsafe.Pointer(&cookie))); ok == 0 {
		t.Fatalf("activate activation context: %v", callErr)
	}
}
