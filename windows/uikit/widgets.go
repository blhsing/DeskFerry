//go:build windows

package uikit

import (
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// AppIconResourceID is the icon group embedded by build/build-go.ps1.
const AppIconResourceID = 2

// AppIcon returns the DeskFerry application icon, or the stock application
// icon when the executable carries no icon resource (for example in tests).
func AppIcon() *walk.Icon {
	icon, err := walk.NewIconFromResourceId(AppIconResourceID)
	if err == nil {
		return icon
	}
	return walk.IconApplication()
}

// WindowBackground is the page background brush for light windows.
func WindowBackground() Brush { return SolidColorBrush{Color: ColorBg} }

// AppMark paints the app icon at Size x Size (1/96 inch units).
type AppMark struct {
	Size int
}

// Create implements declarative.Widget.
func (d AppMark) Create(builder *Builder) error {
	size := d.Size
	if size <= 0 {
		size = 32
	}
	icon, iconErr := walk.NewIconFromResourceIdWithSize(AppIconResourceID, walk.Size{Width: size, Height: size})
	var widget *walk.CustomWidget
	paint := func(canvas *walk.Canvas, _ walk.Rectangle) error {
		bounds := widget.ClientBoundsPixels()
		fillRect(canvas.HDC(), bounds, BackgroundColor(widget))
		pixels := walk.IntFrom96DPI(size, widget.DPI())
		target := walk.Rectangle{X: (bounds.Width - pixels) / 2, Y: (bounds.Height - pixels) / 2, Width: pixels, Height: pixels}
		if iconErr != nil || icon == nil {
			// No embedded icon (test binaries): draw a primary rounded mark.
			paintSupersampled(canvas.HDC(), target, BackgroundColor(widget), shape{bounds: target, radius: pixels / 4, fill: ColorPrimary})
			return nil
		}
		return canvas.DrawImageStretchedPixels(icon, target)
	}
	created, err := walk.NewCustomWidgetPixels(builder.Parent(), 0, paint)
	if err != nil {
		return err
	}
	widget = created
	if err := walk.InitWrapperWindow(&fixedCustomWidget{CustomWidget: created}); err != nil {
		created.Dispose()
		return err
	}
	widget.SetPaintMode(walk.PaintNoErase)
	fixed := Size{Width: size, Height: size}
	return builder.InitWidget(appMarkDecl{MinSize: fixed, MaxSize: fixed, Alignment: AlignHCenterVCenter}, widget, func() error {
		if icon != nil {
			widget.AddDisposable(icon)
		}
		return nil
	})
}

type fixedCustomWidget struct {
	*walk.CustomWidget
}

func (*fixedCustomWidget) CreateLayoutItem(*walk.LayoutContext) walk.LayoutItem {
	return &fixedLayoutItem{}
}

type appMarkDecl struct {
	MinSize   Size
	MaxSize   Size
	Alignment Alignment2D
}

func (appMarkDecl) Create(*Builder) error { return nil }

// Header is the app header: mark, title, muted caption, and widgets on the
// right (normally the overall status chip).
func Header(title, caption string, trailing ...Widget) Widget {
	children := []Widget{
		AppMark{Size: 32},
		withAlignment(singleLineLabel(title, DisplayFont(), ColorText), AlignHNearVCenter),
		Label{Text: caption, Font: CaptionFont(), TextColor: ColorTextMuted, Alignment: AlignHNearVCenter},
		HSpacer{},
	}
	children = append(children, trailing...)
	return Composite{Layout: HBox{MarginsZero: true, Spacing: Space12}, Children: children}
}

// FieldLabel is a form label in the aligned label column.
func FieldLabel(text string) Widget {
	return Label{
		Text:          text,
		Font:          LabelFont(),
		TextColor:     ColorTextSecondary,
		MinSize:       Size{Width: LabelColumnWidth},
		MaxSize:       Size{Width: LabelColumnWidth},
		TextAlignment: AlignNear,
		Alignment:     AlignHNearVCenter,
	}
}

// Caption is muted helper text.
func Caption(text string) Label {
	return Label{Text: text, Font: CaptionFont(), TextColor: ColorTextMuted}
}

// SectionLabel is a small label above a multi-line field.
func SectionLabel(text string) Label {
	return Label{Text: text, Font: LabelFont(), TextColor: ColorTextSecondary}
}

// Button is a native secondary push button with the shared height.
func Button(assignTo **walk.PushButton, text string, onClicked walk.EventHandler) PushButton {
	return PushButton{AssignTo: assignTo, Text: text, OnClicked: onClicked, MinSize: Size{Width: 84, Height: ButtonHeight}, MaxSize: Size{Height: ButtonHeight}}
}

// LogEdit is a monospace multi-line text box on the subtle surface.
func LogEdit(assignTo **walk.TextEdit, readOnly bool, text string) TextEdit {
	return TextEdit{
		AssignTo:   assignTo,
		ReadOnly:   readOnly,
		VScroll:    true,
		Text:       text,
		Font:       MonoFont(),
		Background: SolidColorBrush{Color: ColorSurfaceSubtle},
		TextColor:  ColorText,
	}
}

// StatusTile is a compact card: a muted caption with a status chip on the
// right, and a value line below.
type StatusTile struct {
	Caption   string
	Chip      **StatusChip
	ChipText  string
	Value     **walk.Label
	ValueText string
	MinWidth  int
}

// Create implements declarative.Widget.
func (t StatusTile) Create(builder *Builder) error {
	children := []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: Space8},
			Children: []Widget{
				withAlignment(singleLineLabel(t.Caption, CaptionStrongFont(), ColorTextMuted), AlignHNearVCenter),
				HSpacer{},
				Chip{AssignTo: t.Chip, Text: t.ChipText, MaxWidth: 170, Alignment: AlignHFarVCenter},
			},
		},
		Label{AssignTo: t.Value, Text: t.ValueText, Font: TitleFont(), TextColor: ColorText, EllipsisMode: EllipsisEnd},
	}
	minWidth := t.MinWidth
	if minWidth == 0 {
		minWidth = 220
	}
	return Card{
		Padding:       Space12,
		MinSize:       Size{Width: minWidth},
		StretchFactor: 1,
		Layout:        VBox{MarginsZero: true, Spacing: 2},
		Children:      children,
	}.Create(builder)
}

// singleLineLabel is a label that never wraps: its minimum width is the
// measured text width plus a small safety margin for font rounding.
func singleLineLabel(text string, font Font, color walk.Color) Label {
	width := measureTextWidth96(nil, fontSpec{family: font.Family, pointSize: font.PointSize, bold: font.Bold}, text)*112/100 + 6
	return Label{Text: text, Font: font, TextColor: color, MinSize: Size{Width: width}}
}

func withAlignment(label Label, alignment Alignment2D) Label {
	label.Alignment = alignment
	return label
}

// ResizeWindow sets a form's outer size in 1/96 inch units without changing
// its minimum (walk's own SetSize runs before the first layout pass and pins
// the minimum track size to the requested size). The size is clamped to the
// work area of the form's monitor and the form is moved inside it, so a
// window never opens larger than the screen.
func ResizeWindow(form walk.Form, width, height int) {
	hwnd := form.Handle()
	size := walk.SizeFrom96DPI(walk.Size{Width: width, Height: height}, form.DPI())
	var rect win.RECT
	win.GetWindowRect(hwnd, &rect)
	x, y := rect.Left, rect.Top
	w, h := int32(size.Width), int32(size.Height)
	monitor := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	if win.GetMonitorInfo(win.MonitorFromWindow(hwnd, win.MONITOR_DEFAULTTONEAREST), &monitor) {
		work := monitor.RcWork
		w = min(w, work.Right-work.Left)
		h = min(h, work.Bottom-work.Top)
		x = max(work.Left, min(x, work.Right-w))
		y = max(work.Top, min(y, work.Bottom-h))
	}
	win.SetWindowPos(hwnd, 0, x, y, w, h, win.SWP_NOZORDER|win.SWP_NOACTIVATE)
}
