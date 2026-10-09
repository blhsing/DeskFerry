//go:build windows

package uikit

import (
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// cardWindow paints a white rounded card with a hairline border behind its
// children. Children inherit the white background brush for text controls.
type cardWindow struct {
	*walk.Composite
	fill   walk.Color
	border walk.Color
	radius int
}

// WndProc paints the rounded card background.
func (c *cardWindow) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case win.WM_ERASEBKGND:
		c.paintBackground(win.HDC(wParam))
		return 1
	case win.WM_WINDOWPOSCHANGING:
		// Windows keeps the old client bits when a window is resized, which
		// leaves stale rounded corners behind; discard them instead.
		position := (*win.WINDOWPOS)(pointerFromHandle(lParam))
		if position.Flags&win.SWP_NOSIZE == 0 {
			position.Flags |= win.SWP_NOCOPYBITS
		}
	case win.WM_SIZE:
		// Rounded corners and borders move with the size, so repaint all of it.
		win.InvalidateRect(hwnd, nil, true)
	}
	return c.Composite.WndProc(hwnd, msg, wParam, lParam)
}

func (c *cardWindow) paintBackground(hdc win.HDC) {
	bounds := clientBounds(c.Handle())
	dpi := c.DPI()
	var outside walk.Color = ColorBg
	if parent := c.Parent(); parent != nil {
		outside = BackgroundColor(parent)
	}
	paintPanel(hdc, bounds, walk.IntFrom96DPI(c.radius, dpi), max(1, walk.IntFrom96DPI(1, dpi)), c.fill, c.border, outside)
}

// Card is a white surface with an optional title and muted description,
// followed by its children laid out by Layout (VBox by default).
type Card struct {
	AssignTo      **walk.Composite
	Title         string
	Description   string
	TitleTrailing []Widget
	Layout        Layout
	Children      []Widget
	Padding       int
	Fill          walk.Color
	Border        walk.Color
	MinSize       Size
	MaxSize       Size
	StretchFactor int
	Alignment     Alignment2D
	Row           int
	Column        int
	RowSpan       int
	ColumnSpan    int
}

type cardDecl struct {
	Layout        Layout
	Children      []Widget
	DataBinder    DataBinder
	MinSize       Size
	MaxSize       Size
	StretchFactor int
	Alignment     Alignment2D
	Row           int
	Column        int
	RowSpan       int
	ColumnSpan    int
}

func (cardDecl) Create(*Builder) error { return nil }

// Create implements declarative.Widget.
func (d Card) Create(builder *Builder) error {
	composite, err := walk.NewComposite(builder.Parent())
	if err != nil {
		return err
	}
	card := &cardWindow{Composite: composite, fill: ColorSurface, border: ColorBorder, radius: CardRadius}
	if d.Fill != 0 {
		card.fill = d.Fill
	}
	if d.Border != 0 {
		card.border = d.Border
	}
	if err := walk.InitWrapperWindow(card); err != nil {
		composite.Dispose()
		return err
	}
	brush, err := walk.NewSolidColorBrush(card.fill)
	if err != nil {
		composite.Dispose()
		return err
	}
	composite.AddDisposable(brush)
	composite.SetBackground(brush)
	if d.AssignTo != nil {
		*d.AssignTo = composite
	}

	padding := d.Padding
	if padding == 0 {
		padding = CardPadding
	}
	var children []Widget
	if d.Title != "" || d.Description != "" || len(d.TitleTrailing) > 0 {
		var headerChildren []Widget
		var titleRow []Widget
		if d.Title != "" {
			titleRow = append(titleRow, singleLineLabel(d.Title, TitleFont(), ColorText))
		}
		titleRow = append(titleRow, HSpacer{})
		titleRow = append(titleRow, d.TitleTrailing...)
		if len(titleRow) > 0 {
			headerChildren = append(headerChildren, Composite{Layout: HBox{MarginsZero: true, Spacing: Space8}, Children: titleRow})
		}
		if d.Description != "" {
			headerChildren = append(headerChildren, Label{Text: d.Description, Font: CaptionFont(), TextColor: ColorTextMuted, EllipsisMode: EllipsisEnd})
		}
		children = append(children, Composite{Layout: VBox{MarginsZero: true, Spacing: 2}, Children: headerChildren})
	}
	layout := d.Layout
	if layout == nil {
		layout = VBox{MarginsZero: true, Spacing: FieldGap}
	}
	children = append(children, Composite{Layout: layout, Children: d.Children, StretchFactor: 1})

	decl := cardDecl{
		Layout:        VBox{Margins: Margins{Left: padding, Top: padding - 2, Right: padding, Bottom: padding}, Spacing: Space8},
		Children:      children,
		MinSize:       d.MinSize,
		MaxSize:       d.MaxSize,
		StretchFactor: d.StretchFactor,
		Alignment:     d.Alignment,
		Row:           d.Row,
		Column:        d.Column,
		RowSpan:       d.RowSpan,
		ColumnSpan:    d.ColumnSpan,
	}
	return builder.InitWidget(decl, card, nil)
}
