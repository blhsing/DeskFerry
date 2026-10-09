//go:build windows

package uikit

import (
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

var (
	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procCreateSolidBrush    = gdi32.NewProc("CreateSolidBrush")
	procCreatePen           = gdi32.NewProc("CreatePen")
	procEnumFontFamiliesExW = gdi32.NewProc("EnumFontFamiliesExW")
	procFillRect            = user32.NewProc("FillRect")
	procSetWindowTextW      = user32.NewProc("SetWindowTextW")
)

// supersample is the anti-aliasing factor used for rounded shapes. GDI has no
// anti-aliasing, so shapes are drawn 4x larger and reduced with HALFTONE.
const supersample = 4

func createSolidBrush(color walk.Color) win.HGDIOBJ {
	handle, _, _ := procCreateSolidBrush.Call(uintptr(color))
	return win.HGDIOBJ(handle)
}

func createPen(style int32, width int32, color walk.Color) win.HGDIOBJ {
	handle, _, _ := procCreatePen.Call(uintptr(style), uintptr(width), uintptr(color))
	return win.HGDIOBJ(handle)
}

func fillRect(hdc win.HDC, bounds walk.Rectangle, color walk.Color) {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	brush := createSolidBrush(color)
	if brush == 0 {
		return
	}
	defer win.DeleteObject(brush)
	rc := win.RECT{Left: int32(bounds.X), Top: int32(bounds.Y), Right: int32(bounds.X + bounds.Width), Bottom: int32(bounds.Y + bounds.Height)}
	_, _, _ = procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&rc)), uintptr(brush))
}

func setWindowText(hwnd win.HWND, text string) {
	ptr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	_, _, _ = procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ptr)))
}

// shape is one anti-aliased primitive drawn by paintSupersampled.
type shape struct {
	bounds      walk.Rectangle // target pixels
	radius      int            // corner radius in pixels; -1 draws an ellipse
	fill        walk.Color
	border      walk.Color
	borderWidth int // pixels; 0 draws no border
}

// paintSupersampled renders the clip rectangle of the given shapes over the
// background colour with anti-aliased edges and copies it to hdc.
func paintSupersampled(hdc win.HDC, clip walk.Rectangle, background walk.Color, shapes ...shape) {
	if clip.Width <= 0 || clip.Height <= 0 {
		return
	}
	width, height := int32(clip.Width*supersample), int32(clip.Height*supersample)
	mem := win.CreateCompatibleDC(hdc)
	if mem == 0 {
		return
	}
	defer win.DeleteDC(mem)
	bitmap := win.CreateCompatibleBitmap(hdc, width, height)
	if bitmap == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	oldBitmap := win.SelectObject(mem, win.HGDIOBJ(bitmap))
	defer win.SelectObject(mem, oldBitmap)

	fillRect(mem, walk.Rectangle{Width: int(width), Height: int(height)}, background)
	for _, item := range shapes {
		brush := createSolidBrush(item.fill)
		pen := win.GetStockObject(win.NULL_PEN)
		ownPen := false
		if item.borderWidth > 0 {
			pen = createPen(win.PS_INSIDEFRAME, int32(item.borderWidth*supersample), item.border)
			ownPen = true
		}
		oldBrush := win.SelectObject(mem, brush)
		oldPen := win.SelectObject(mem, pen)
		left := int32((item.bounds.X - clip.X) * supersample)
		top := int32((item.bounds.Y - clip.Y) * supersample)
		right := int32((item.bounds.X + item.bounds.Width - clip.X) * supersample)
		bottom := int32((item.bounds.Y + item.bounds.Height - clip.Y) * supersample)
		if !ownPen {
			// A null pen leaves the right and bottom edges unpainted.
			right++
			bottom++
		}
		if item.radius < 0 {
			win.Ellipse(mem, left, top, right, bottom)
		} else {
			diameter := int32(item.radius * 2 * supersample)
			win.RoundRect(mem, left, top, right, bottom, diameter, diameter)
		}
		win.SelectObject(mem, oldPen)
		win.SelectObject(mem, oldBrush)
		win.DeleteObject(brush)
		if ownPen {
			win.DeleteObject(pen)
		}
	}

	oldMode := win.SetStretchBltMode(hdc, win.HALFTONE)
	var oldOrigin win.POINT
	win.SetBrushOrgEx(hdc, 0, 0, &oldOrigin)
	win.StretchBlt(hdc, int32(clip.X), int32(clip.Y), int32(clip.Width), int32(clip.Height), mem, 0, 0, width, height, win.SRCCOPY)
	win.SetStretchBltMode(hdc, oldMode)
	win.SetBrushOrgEx(hdc, oldOrigin.X, oldOrigin.Y, nil)
}

// paintPanel paints a rounded panel that fills bounds. Only the corners are
// supersampled so large cards stay cheap to repaint.
func paintPanel(hdc win.HDC, bounds walk.Rectangle, radius, borderWidth int, fill, border, outside walk.Color) {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	if borderWidth > 0 {
		fillRect(hdc, bounds, border)
		fillRect(hdc, walk.Rectangle{X: bounds.X + borderWidth, Y: bounds.Y + borderWidth, Width: bounds.Width - 2*borderWidth, Height: bounds.Height - 2*borderWidth}, fill)
	} else {
		fillRect(hdc, bounds, fill)
	}
	corner := min(radius+1, bounds.Width/2, bounds.Height/2)
	if corner <= 0 {
		return
	}
	panel := shape{bounds: bounds, radius: radius, fill: fill, border: border, borderWidth: borderWidth}
	for _, clip := range []walk.Rectangle{
		{X: bounds.X, Y: bounds.Y, Width: corner, Height: corner},
		{X: bounds.X + bounds.Width - corner, Y: bounds.Y, Width: corner, Height: corner},
		{X: bounds.X, Y: bounds.Y + bounds.Height - corner, Width: corner, Height: corner},
		{X: bounds.X + bounds.Width - corner, Y: bounds.Y + bounds.Height - corner, Width: corner, Height: corner},
	} {
		paintSupersampled(hdc, clip, outside, panel)
	}
}

// BackgroundColor returns the colour behind a widget: the first solid
// background brush found on it or its ancestors, or ColorBg.
func BackgroundColor(window walk.Window) walk.Color {
	for window != nil {
		if brush, ok := window.Background().(*walk.SolidColorBrush); ok && brush != nil {
			return brush.Color()
		}
		widget, ok := window.(walk.Widget)
		if !ok {
			break
		}
		parent := widget.Parent()
		if parent == nil {
			if form := widget.Form(); form != nil {
				if brush, ok := form.Background().(*walk.SolidColorBrush); ok && brush != nil {
					return brush.Color()
				}
			}
			break
		}
		window = parent
	}
	return ColorBg
}

func clientBounds(hwnd win.HWND) walk.Rectangle {
	var rc win.RECT
	win.GetClientRect(hwnd, &rc)
	return walk.Rectangle{X: int(rc.Left), Y: int(rc.Top), Width: int(rc.Right - rc.Left), Height: int(rc.Bottom - rc.Top)}
}

// pointerFromHandle converts a message parameter that carries a pointer.
func pointerFromHandle(value uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&value))
}

// measureTextWidth96 returns the single-line width of text in 1/96 inch
// units for the given declarative font.
func measureTextWidth96(window walk.Window, font fontSpec, text string) int {
	dpi := 96
	if window != nil && window.Handle() != 0 {
		dpi = window.DPI()
	}
	hdc := win.GetDC(0)
	if hdc == 0 {
		return 8 * len(text)
	}
	defer win.ReleaseDC(0, hdc)
	logFont := win.LOGFONT{LfHeight: -int32(walk.IntFrom96DPI(font.pointSize*96, dpi) / 72), LfWeight: win.FW_NORMAL, LfCharSet: win.DEFAULT_CHARSET, LfQuality: win.CLEARTYPE_QUALITY}
	if font.bold {
		logFont.LfWeight = win.FW_BOLD
	}
	family := syscall.StringToUTF16(font.family)
	copy(logFont.LfFaceName[:len(logFont.LfFaceName)-1], family)
	hFont := win.CreateFontIndirect(&logFont)
	if hFont == 0 {
		return 8 * len(text)
	}
	defer win.DeleteObject(win.HGDIOBJ(hFont))
	old := win.SelectObject(hdc, win.HGDIOBJ(hFont))
	defer win.SelectObject(hdc, old)
	runes := syscall.StringToUTF16(text)
	var size win.SIZE
	if len(runes) <= 1 || !win.GetTextExtentPoint32(hdc, &runes[0], int32(len(runes)-1), &size) {
		return 0
	}
	return walk.IntTo96DPI(int(size.CX), dpi) + 1
}

type fontSpec struct {
	family    string
	pointSize int
	bold      bool
}

// fixedLayoutItem sizes a custom widget from its min/max size only; walk's
// CustomWidget otherwise reports a greedy item that steals spare space.
type fixedLayoutItem struct {
	walk.LayoutItemBase
	flags walk.LayoutFlags
}

func (li *fixedLayoutItem) LayoutFlags() walk.LayoutFlags { return li.flags }
func (li *fixedLayoutItem) IdealSize() walk.Size          { return li.Geometry().MinSize }
func (li *fixedLayoutItem) MinSize() walk.Size            { return li.Geometry().MinSize }
