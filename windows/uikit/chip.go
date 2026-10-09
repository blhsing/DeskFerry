//go:build windows

package uikit

import (
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

const (
	chipPadLeft  = 10
	chipDot      = 8
	chipDotGap   = 6
	chipPadRight = 12
)

// StatusChip is a pill with a coloured dot and a short status word. Its tone
// follows the text automatically (see ToneFor) unless SetStatus is used.
type StatusChip struct {
	*walk.CustomWidget
	text     string
	tone     Tone
	maxWidth int
	declMin  walk.Size
	onChange func(text string, tone Tone)
}

// NewStatusChip creates a status chip.
func NewStatusChip(parent walk.Container, text string) (*StatusChip, error) {
	chip := &StatusChip{text: text, tone: ToneFor(text)}
	widget, err := walk.NewCustomWidgetPixels(parent, 0, chip.paint)
	if err != nil {
		return nil, err
	}
	chip.CustomWidget = widget
	if err := walk.InitWrapperWindow(chip); err != nil {
		widget.Dispose()
		return nil, err
	}
	widget.SetPaintMode(walk.PaintNoErase)
	widget.SetInvalidatesOnResize(true)
	chip.applyText()
	return chip, nil
}

// CreateLayoutItem keeps the chip at its measured size.
func (c *StatusChip) CreateLayoutItem(*walk.LayoutContext) walk.LayoutItem {
	return &fixedLayoutItem{}
}

// Text returns the chip text.
func (c *StatusChip) Text() string { return c.text }

// Tone returns the current tone.
func (c *StatusChip) Tone() Tone { return c.tone }

// SetText sets the chip text and derives its colour from the status vocabulary.
func (c *StatusChip) SetText(text string) error {
	c.SetStatus(text, ToneFor(text))
	return nil
}

// SetStatus sets the chip text with an explicit tone.
func (c *StatusChip) SetStatus(text string, tone Tone) {
	if text == c.text && tone == c.tone {
		return
	}
	c.text = text
	c.tone = tone
	c.applyText()
	if c.onChange != nil {
		c.onChange(text, tone)
	}
}

// OnChange registers a callback that runs on the UI thread after the chip changes.
func (c *StatusChip) OnChange(handler func(text string, tone Tone)) {
	c.onChange = handler
}

func (c *StatusChip) applyText() {
	_ = c.SetToolTipText(c.text)
	setWindowText(c.Handle(), c.text)
	width := chipPadLeft + chipDot + chipDotGap + c.textWidth96() + chipPadRight
	if c.maxWidth > 0 && width > c.maxWidth {
		width = c.maxWidth
	}
	width = max(width, c.declMin.Width)
	size := walk.Size{Width: width, Height: max(ChipHeight, c.declMin.Height)}
	_ = c.SetMinMaxSize(size, size)
	_ = c.Invalidate()
}

func (c *StatusChip) textWidth96() int {
	font := semibold(false, 9)
	return measureTextWidth96(c, fontSpec{family: font.Family, pointSize: font.PointSize, bold: font.Bold}, c.text)
}

func chipFont() *walk.Font { return walkFont(semibold(false, 9)) }

func (c *StatusChip) paint(canvas *walk.Canvas, _ walk.Rectangle) error {
	bounds := c.ClientBoundsPixels()
	dpi := c.DPI()
	scale := func(value int) int { return walk.IntFrom96DPI(value, dpi) }
	outside := BackgroundColor(c)
	palette := c.tone.Palette()
	pillHeight := min(bounds.Height, scale(ChipHeight))
	pill := walk.Rectangle{X: 0, Y: (bounds.Height - pillHeight) / 2, Width: bounds.Width, Height: pillHeight}
	dotSize := scale(chipDot)
	dot := walk.Rectangle{X: scale(chipPadLeft), Y: pill.Y + (pillHeight-dotSize)/2, Width: dotSize, Height: dotSize}
	fillRect(canvas.HDC(), bounds, outside)
	paintSupersampled(canvas.HDC(), bounds, outside,
		shape{bounds: pill, radius: pillHeight / 2, fill: palette.Background},
		shape{bounds: dot, radius: -1, fill: palette.Dot},
	)
	textX := dot.X + dotSize + scale(chipDotGap)
	textBounds := walk.Rectangle{X: textX, Y: pill.Y, Width: max(0, bounds.Width-textX-scale(chipPadRight)+scale(2)), Height: pillHeight}
	return canvas.DrawTextPixels(c.text, chipFont(), palette.Foreground, textBounds, walk.TextLeft|walk.TextVCenter|walk.TextSingleLine|walk.TextEndEllipsis)
}

// Chip is the declarative form of StatusChip.
type Chip struct {
	AssignTo   **StatusChip
	Text       string
	MaxWidth   int
	MinSize    Size
	Alignment  Alignment2D
	Row        int
	Column     int
	RowSpan    int
	ColumnSpan int
}

// Create implements declarative.Widget.
func (d Chip) Create(builder *Builder) error {
	chip, err := NewStatusChip(builder.Parent(), d.Text)
	if err != nil {
		return err
	}
	chip.maxWidth = d.MaxWidth
	chip.declMin = walk.Size{Width: d.MinSize.Width, Height: d.MinSize.Height}
	if d.AssignTo != nil {
		*d.AssignTo = chip
	}
	return builder.InitWidget(chipDecl{Alignment: d.Alignment, Row: d.Row, Column: d.Column, RowSpan: d.RowSpan, ColumnSpan: d.ColumnSpan}, chip, func() error {
		chip.applyText()
		return nil
	})
}

type chipDecl struct {
	Alignment  Alignment2D
	Row        int
	Column     int
	RowSpan    int
	ColumnSpan int
}

func (chipDecl) Create(*Builder) error { return nil }
