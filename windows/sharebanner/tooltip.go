//go:build windows

package sharebanner

import (
	"syscall"
	"unsafe"

	"github.com/lxn/win"
)

// Icon-only buttons name themselves in a standard Windows tooltip. The banner
// never takes focus, so the tooltip is the way to learn what an icon does.

const maxTooltipWidth = 320

// tooltipText is a button's tooltip, or "" when its label is already shown.
func (b *banner) tooltipText(index int) string {
	if !b.compact && index != buttonToggle {
		return ""
	}
	return b.buttonLabel(index)
}

// updateTooltips keeps one tool per visible icon button, over its rectangle.
func (b *banner) updateTooltips() {
	if b.tooltip == 0 {
		b.tooltip = win.CreateWindowEx(win.WS_EX_TOPMOST, syscall.StringToUTF16Ptr("tooltips_class32"), nil,
			win.WS_POPUP|win.TTS_ALWAYSTIP|win.TTS_NOPREFIX, win.CW_USEDEFAULT, win.CW_USEDEFAULT, win.CW_USEDEFAULT, win.CW_USEDEFAULT,
			b.hwnd, 0, win.GetModuleHandle(nil), nil)
		if b.tooltip == 0 {
			return
		}
		win.SendMessage(b.tooltip, win.TTM_SETMAXTIPWIDTH, 0, uintptr(b.px(maxTooltipWidth)))
	}
	b.updateInfoTooltip()
	for index := range b.layout.buttons {
		text := ""
		if b.buttonVisible(index) {
			text = b.tooltipText(index)
		}
		info := win.TOOLINFO{UFlags: win.TTF_SUBCLASS, Hwnd: b.hwnd, UId: uintptr(index + 1), Rect: b.layout.buttons[index]}
		info.CbSize = uint32(unsafe.Sizeof(info))
		switch {
		case text == "" && b.tooltipShown[index] != "":
			win.SendMessage(b.tooltip, win.TTM_DELTOOL, 0, uintptr(unsafe.Pointer(&info)))
			b.tooltipShown[index] = ""
		case text == "":
		case b.tooltipShown[index] == "":
			info.LpszText = syscall.StringToUTF16Ptr(text)
			win.SendMessage(b.tooltip, win.TTM_ADDTOOL, 0, uintptr(unsafe.Pointer(&info)))
			b.tooltipShown[index] = text
		default:
			win.SendMessage(b.tooltip, win.TTM_NEWTOOLRECT, 0, uintptr(unsafe.Pointer(&info)))
			if text != b.tooltipShown[index] {
				info.LpszText = syscall.StringToUTF16Ptr(text)
				win.SendMessage(b.tooltip, win.TTM_UPDATETIPTEXT, 0, uintptr(unsafe.Pointer(&info)))
				b.tooltipShown[index] = text
			}
		}
	}
}

// infoToolID is the tool over the compact form's mark and indicator, which
// tells who is connected in words.
const infoToolID = 100

func (b *banner) updateInfoTooltip() {
	info := win.TOOLINFO{UFlags: win.TTF_SUBCLASS, Hwnd: b.hwnd, UId: infoToolID,
		Rect: win.RECT{Left: 0, Top: 0, Right: b.layout.dot.Right + b.px(compactSlotGap)/2, Bottom: b.layout.height}}
	info.CbSize = uint32(unsafe.Sizeof(info))
	text := ""
	if b.compact {
		text = b.view.title + "\r\n" + b.view.caption
	}
	switch {
	case text == "" && b.infoShown != "":
		win.SendMessage(b.tooltip, win.TTM_DELTOOL, 0, uintptr(unsafe.Pointer(&info)))
	case text == "":
	case b.infoShown == "":
		info.LpszText = syscall.StringToUTF16Ptr(text)
		win.SendMessage(b.tooltip, win.TTM_ADDTOOL, 0, uintptr(unsafe.Pointer(&info)))
	default:
		win.SendMessage(b.tooltip, win.TTM_NEWTOOLRECT, 0, uintptr(unsafe.Pointer(&info)))
		if text != b.infoShown {
			info.LpszText = syscall.StringToUTF16Ptr(text)
			win.SendMessage(b.tooltip, win.TTM_UPDATETIPTEXT, 0, uintptr(unsafe.Pointer(&info)))
		}
	}
	b.infoShown = text
}
