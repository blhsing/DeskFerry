//go:build windows

package uikit

import (
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

const accentButtonPadding = 20

// AccentButton is the filled primary button of the design system. Native
// push buttons cannot be filled with the primary colour, so it is painted.
// It supports mouse, focus, Space and Enter activation.
type AccentButton struct {
	*walk.CustomWidget
	text     string
	hover    bool
	pressed  bool
	tracking bool
	stretch  bool
	declMin  walk.Size
	clicked  walk.EventPublisher
}

// NewAccentButton creates a primary button.
func NewAccentButton(parent walk.Container, text string) (*AccentButton, error) {
	button := &AccentButton{text: text}
	widget, err := walk.NewCustomWidgetPixels(parent, win.WS_TABSTOP, button.paint)
	if err != nil {
		return nil, err
	}
	button.CustomWidget = widget
	if err := walk.InitWrapperWindow(button); err != nil {
		widget.Dispose()
		return nil, err
	}
	widget.SetPaintMode(walk.PaintNoErase)
	widget.SetInvalidatesOnResize(true)
	button.applyText()
	return button, nil
}

// CreateLayoutItem keeps the button at its measured height; it grows
// horizontally only when Stretch is set.
func (b *AccentButton) CreateLayoutItem(*walk.LayoutContext) walk.LayoutItem {
	if b.stretch {
		return &fixedLayoutItem{flags: walk.GrowableHorz}
	}
	return &fixedLayoutItem{}
}

// Text returns the button caption.
func (b *AccentButton) Text() string { return b.text }

// SetText changes the button caption.
func (b *AccentButton) SetText(text string) error {
	if text == b.text {
		return nil
	}
	b.text = text
	b.applyText()
	return nil
}

// SetEnabled enables or disables the button and repaints it.
func (b *AccentButton) SetEnabled(enabled bool) {
	b.CustomWidget.SetEnabled(enabled)
	_ = b.Invalidate()
}

// Clicked is published when the button is activated.
func (b *AccentButton) Clicked() *walk.Event { return b.clicked.Event() }

func (b *AccentButton) applyText() {
	setWindowText(b.Handle(), b.text)
	width := max(b.textWidth96()+2*accentButtonPadding, 112, b.declMin.Width)
	height := max(PrimaryButtonHeight, b.declMin.Height)
	maxWidth := width
	if b.stretch {
		maxWidth = 0
	}
	_ = b.SetMinMaxSize(walk.Size{Width: width, Height: height}, walk.Size{Width: maxWidth, Height: height})
	_ = b.Invalidate()
}

func accentFont() *walk.Font { return walkFont(semibold(false, 10)) }

func (b *AccentButton) textWidth96() int {
	font := semibold(false, 10)
	return measureTextWidth96(b, fontSpec{family: font.Family, pointSize: font.PointSize, bold: font.Bold}, b.text)
}

func (b *AccentButton) click() {
	if !b.Enabled() || !b.Visible() {
		return
	}
	b.clicked.Publish()
}

// WndProc handles hover, press, focus and keyboard activation.
func (b *AccentButton) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case win.WM_MOUSEMOVE:
		if !b.tracking {
			track := win.TRACKMOUSEEVENT{CbSize: uint32(unsafe.Sizeof(win.TRACKMOUSEEVENT{})), DwFlags: win.TME_LEAVE, HwndTrack: hwnd}
			b.tracking = win.TrackMouseEvent(&track)
		}
		if !b.hover {
			b.hover = true
			_ = b.Invalidate()
		}
	case win.WM_MOUSELEAVE:
		b.tracking = false
		b.hover = false
		_ = b.Invalidate()
	case win.WM_LBUTTONDOWN:
		win.SetFocus(hwnd)
		win.SetCapture(hwnd)
		b.pressed = true
		_ = b.Invalidate()
	case win.WM_LBUTTONUP:
		if b.pressed {
			b.pressed = false
			win.ReleaseCapture()
			_ = b.Invalidate()
			x := int(int16(win.LOWORD(uint32(lParam))))
			y := int(int16(win.HIWORD(uint32(lParam))))
			bounds := clientBounds(hwnd)
			if x >= 0 && y >= 0 && x < bounds.Width && y < bounds.Height {
				b.click()
			}
		}
	case win.WM_CAPTURECHANGED:
		if b.pressed {
			b.pressed = false
			_ = b.Invalidate()
		}
	case win.WM_SETFOCUS, win.WM_KILLFOCUS, win.WM_ENABLE:
		_ = b.Invalidate()
	case win.WM_GETDLGCODE:
		code := uintptr(win.DLGC_BUTTON)
		if lParam != 0 {
			message := (*win.MSG)(pointerFromHandle(lParam))
			if message.Message == win.WM_KEYDOWN && message.WParam == win.VK_RETURN {
				code |= win.DLGC_WANTMESSAGE
			}
		}
		return code
	case win.WM_KEYDOWN:
		if wParam == win.VK_SPACE || wParam == win.VK_RETURN {
			b.click()
			return 0
		}
	}
	return b.CustomWidget.WndProc(hwnd, msg, wParam, lParam)
}

func (b *AccentButton) paint(canvas *walk.Canvas, _ walk.Rectangle) error {
	bounds := b.ClientBoundsPixels()
	dpi := b.DPI()
	scale := func(value int) int { return walk.IntFrom96DPI(value, dpi) }
	outside := BackgroundColor(b)
	fill := ColorPrimary
	switch {
	case !b.Enabled():
		fill = ColorPrimaryDisabled
	case b.pressed:
		fill = ColorPrimaryActive
	case b.hover:
		fill = ColorPrimaryHover
	}
	radius := scale(ControlRadius)
	shapes := []shape{{bounds: bounds, radius: radius, fill: fill}}
	if b.Focused() && b.Enabled() {
		inset := scale(3)
		ring := walk.Rectangle{X: inset, Y: inset, Width: bounds.Width - 2*inset, Height: bounds.Height - 2*inset}
		shapes = append(shapes, shape{bounds: ring, radius: max(1, radius-inset), fill: fill, border: walk.RGB(0xBF, 0xD3, 0xFE), borderWidth: max(1, scale(1))})
	}
	paintSupersampled(canvas.HDC(), bounds, outside, shapes...)
	return canvas.DrawTextPixels(b.text, accentFont(), ColorPrimaryOn, bounds, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine|walk.TextEndEllipsis)
}

// PrimaryButton is the declarative form of AccentButton. Set Stretch to let
// the button fill the available width.
type PrimaryButton struct {
	AssignTo      **AccentButton
	Text          string
	OnClicked     walk.EventHandler
	Stretch       bool
	MinSize       Size
	StretchFactor int
	Alignment     Alignment2D
	Row           int
	Column        int
	RowSpan       int
	ColumnSpan    int
}

// Create implements declarative.Widget.
func (d PrimaryButton) Create(builder *Builder) error {
	button, err := NewAccentButton(builder.Parent(), d.Text)
	if err != nil {
		return err
	}
	button.stretch = d.Stretch
	button.declMin = walk.Size{Width: d.MinSize.Width, Height: d.MinSize.Height}
	if d.AssignTo != nil {
		*d.AssignTo = button
	}
	decl := buttonDecl{StretchFactor: d.StretchFactor, Alignment: d.Alignment, Row: d.Row, Column: d.Column, RowSpan: d.RowSpan, ColumnSpan: d.ColumnSpan}
	return builder.InitWidget(decl, button, func() error {
		if d.OnClicked != nil {
			button.Clicked().Attach(d.OnClicked)
		}
		button.applyText()
		return nil
	})
}

type buttonDecl struct {
	StretchFactor int
	Alignment     Alignment2D
	Row           int
	Column        int
	RowSpan       int
	ColumnSpan    int
}

func (buttonDecl) Create(*Builder) error { return nil }
