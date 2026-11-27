//go:build windows

package sharebanner

import (
	"github.com/lxn/walk"
	"github.com/lxn/win"

	"deskferry/windows/uikit"
)

// The compact form is a 32-pixel pill: the app mark, an indicator of who is
// connected (a circle for a person, a square for an AI agent), and icon
// buttons for Stop control (while it is on), Stop sharing and Expand. It
// keeps the full banner's colours, so remote control still fills it with the
// danger colour.

// Compact metrics in 1/96 inch units.
const (
	compactHeight    = 32
	compactPadLeft   = 8
	compactMarkSize  = 16
	compactMarkGap   = 6
	compactSlot      = 10 // the indicator's box
	compactSlotGap   = 8
	compactButton    = 24
	compactButtonGap = 4
	compactPadRight  = 4
	iconBox          = 16
)

func (b *banner) computeCompactLayout() layout {
	px := b.px
	l := layout{height: px(compactHeight)}
	x := px(compactPadLeft)
	markTop := (l.height - px(compactMarkSize)) / 2
	l.mark = win.RECT{Left: x, Top: markTop, Right: x + px(compactMarkSize), Bottom: markTop + px(compactMarkSize)}
	x = l.mark.Right + px(compactMarkGap)
	slotTop := (l.height - px(compactSlot)) / 2
	l.dot = win.RECT{Left: x, Top: slotTop, Right: x + px(compactSlot), Bottom: slotTop + px(compactSlot)}
	x = l.dot.Right + px(compactSlotGap)
	buttonTop := (l.height - px(compactButton)) / 2
	for _, index := range [...]int{buttonStopControl, buttonStopSharing, buttonToggle} {
		if !b.buttonVisible(index) {
			continue
		}
		l.buttons[index] = win.RECT{Left: x, Top: buttonTop, Right: x + px(compactButton), Bottom: buttonTop + px(compactButton)}
		x = l.buttons[index].Right + px(compactButtonGap)
	}
	l.width = x - px(compactButtonGap) + px(compactPadRight)
	return l
}

func (b *banner) paintCompactShapes(canvas *surface, colors scheme) [buttonCount]buttonStyle {
	l := b.layout
	body := win.RECT{Right: l.width, Bottom: l.height}
	radius := float64(b.px(bodyRadius))
	border := max(1, b.px(bodyBorder))
	fillRect(canvas, body, radius, colors.border)
	fillRect(canvas, inset(body, border), radius-float64(border), colors.fill)
	if b.smallIcon == 0 {
		fillRect(canvas, l.mark, float64(l.mark.Right-l.mark.Left)/4, uikit.ColorPrimary)
	}
	b.paintIndicator(canvas, colors)
	styles := b.paintButtons(canvas, colors)
	for index := range l.buttons {
		if b.buttonVisible(index) {
			b.paintIcon(canvas, index, styles[index].text)
		}
	}
	return styles
}

// paintIndicator shows who is connected without text: a circle for a person,
// a rounded square for an AI agent (ringed in white on the danger fill, where
// the warning colour alone has too little contrast).
func (b *banner) paintIndicator(canvas *surface, colors scheme) {
	slot := b.layout.dot
	scale := float64(b.dpi) / 96
	x, y := float64(slot.Left), float64(slot.Top)
	size := float64(compactSlot) * scale
	if b.view.kind != kindAgent {
		dot := 8 * scale
		canvas.fillRoundRect(x+(size-dot)/2, y+(size-dot)/2, dot, dot, dot/2, colors.dot)
		return
	}
	if !b.view.control {
		canvas.fillRoundRect(x, y, size, size, 3*scale, colors.badgeFill)
		return
	}
	canvas.fillRoundRect(x, y, size, size, 3*scale, uikit.ColorPrimaryOn)
	inner := 7 * scale
	canvas.fillRoundRect(x+(size-inner)/2, y+(size-inner)/2, inner, inner, 1.5*scale, colors.badgeFill)
}

// iconShape is one primitive of an icon in a 16x16 box: a capsule from
// (x1, y1) to (x2, y2) with thickness t, or, when rect is set, a rounded
// rectangle at (x1, y1) of size x2 by y2 with radius t.
type iconShape struct {
	rect              bool
	x1, y1, x2, y2, t float64
}

func capsule(x1, y1, x2, y2, t float64) iconShape {
	return iconShape{x1: x1, y1: y1, x2: x2, y2: y2, t: t}
}
func roundRect(x, y, w, h, r float64) iconShape {
	return iconShape{rect: true, x1: x, y1: y, x2: w, y2: h, t: r}
}

var (
	iconMinimize = []iconShape{capsule(4, 11, 12, 11, 2)}
	iconExpand   = []iconShape{
		capsule(8.5, 3, 13, 3, 2), capsule(13, 3, 13, 7.5, 2),
		capsule(3, 8.5, 3, 13, 2), capsule(3, 13, 7.5, 13, 2),
	}
	iconStopSharing = []iconShape{roundRect(3, 3, 10, 10, 2.5)}
	iconStopControl = []iconShape{
		capsule(3, 3, 10.5, 6, 2.5), capsule(3, 3, 6, 10.5, 2.5),
		capsule(7, 7, 10.5, 6, 2.5), capsule(7, 7, 6, 10.5, 2.5),
		roundRect(4.25, 4.25, 3, 3, 1.5),
		capsule(10.25, 10.25, 13.75, 13.75, 2), capsule(13.75, 10.25, 10.25, 13.75, 2),
	}
	iconPending = []iconShape{
		roundRect(2.75, 6.75, 2.5, 2.5, 1.25), roundRect(6.75, 6.75, 2.5, 2.5, 1.25), roundRect(10.75, 6.75, 2.5, 2.5, 1.25),
	}
)

// iconFor returns the icon of a button in its current state.
func (b *banner) iconFor(index int) []iconShape {
	switch {
	case index == buttonToggle && b.compact:
		return iconExpand
	case index == buttonToggle:
		return iconMinimize
	case index == buttonStopControl && b.stopControlPending, index == buttonStopSharing && b.stopSharingPending:
		return iconPending
	case index == buttonStopControl:
		return iconStopControl
	default:
		return iconStopSharing
	}
}

// paintIcon draws a button's icon centred in it, scaled from the 16x16 box.
func (b *banner) paintIcon(canvas *surface, index int, color walk.Color) {
	rect := b.layout.buttons[index]
	scale := float64(b.dpi) / 96
	box := b.px(iconBox)
	ox := float64(rect.Left + (rect.Right-rect.Left-box)/2)
	oy := float64(rect.Top + (rect.Bottom-rect.Top-box)/2)
	for _, shape := range b.iconFor(index) {
		if shape.rect {
			canvas.fillRoundRect(ox+shape.x1*scale, oy+shape.y1*scale, shape.x2*scale, shape.y2*scale, shape.t*scale, color)
			continue
		}
		canvas.fillCapsule(ox+shape.x1*scale, oy+shape.y1*scale, ox+shape.x2*scale, oy+shape.y2*scale, shape.t*scale, color)
	}
}
