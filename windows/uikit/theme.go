//go:build windows

// Package uikit implements the DeskFerry design system (docs/design-system.md)
// for the native lxn/walk windows: palette, typography, spacing, cards, status
// chips, primary buttons, and the shared app icon.
package uikit

import (
	"strings"

	"github.com/lxn/walk"
)

// Light theme tokens.
var (
	ColorBg            = walk.RGB(0xF5, 0xF7, 0xFB)
	ColorSurface       = walk.RGB(0xFF, 0xFF, 0xFF)
	ColorSurfaceSubtle = walk.RGB(0xF9, 0xFA, 0xFC)
	ColorBorder        = walk.RGB(0xE3, 0xE8, 0xEF)
	ColorBorderStrong  = walk.RGB(0xCD, 0xD5, 0xDF)
	ColorText          = walk.RGB(0x10, 0x18, 0x28)
	ColorTextSecondary = walk.RGB(0x47, 0x54, 0x67)
	ColorTextMuted     = walk.RGB(0x66, 0x70, 0x85)
	ColorPrimary       = walk.RGB(0x25, 0x63, 0xEB)
	ColorPrimaryHover  = walk.RGB(0x1D, 0x4E, 0xD8)
	ColorPrimaryActive = walk.RGB(0x1E, 0x40, 0xAF)
	ColorPrimarySoft   = walk.RGB(0xEE, 0xF4, 0xFF)
	ColorPrimaryOn     = walk.RGB(0xFF, 0xFF, 0xFF)
	// ColorPrimaryDisabled is primary blended about 45 % into the surface.
	ColorPrimaryDisabled = walk.RGB(0x9D, 0xB8, 0xF5)
)

// Dark theme tokens, used by the screen viewer chrome.
var (
	ColorDarkBg            = walk.RGB(0x0B, 0x12, 0x20)
	ColorDarkSurface       = walk.RGB(0x11, 0x1A, 0x2E)
	ColorDarkSurfaceSubtle = walk.RGB(0x0F, 0x17, 0x29)
	ColorDarkBorder        = walk.RGB(0x1F, 0x2A, 0x44)
	ColorDarkText          = walk.RGB(0xF2, 0xF4, 0xF7)
	ColorDarkTextSecondary = walk.RGB(0xC2, 0xC9, 0xD6)
	ColorDarkTextMuted     = walk.RGB(0x8A, 0x94, 0xA6)
	ColorDarkPrimary       = walk.RGB(0x4F, 0x8B, 0xFF)
	ColorDarkPrimarySoft   = walk.RGB(0x16, 0x25, 0x4A)
)

// Spacing scale in 1/96 inch units (walk scales them per monitor DPI).
const (
	Space4  = 4
	Space8  = 8
	Space12 = 12
	Space16 = 16
	Space20 = 20
	Space24 = 24
	Space32 = 32

	// PagePadding is the margin between a window edge and its cards.
	PagePadding = Space24
	// CardPadding is the inner padding of a card.
	CardPadding = Space16
	// CardGap is the gap between neighbouring cards.
	CardGap = Space16
	// FieldGap is the gap between form rows inside a card.
	FieldGap = Space8
	// CardRadius is the corner radius of cards.
	CardRadius = 12
	// ControlRadius is the corner radius of buttons.
	ControlRadius = 8
	// ButtonHeight is the height of native secondary buttons.
	ButtonHeight = 28
	// PrimaryButtonHeight is the height of the filled primary button.
	PrimaryButtonHeight = 36
	// LabelColumnWidth aligns the label column of form grids.
	LabelColumnWidth = 116
	// ChipHeight is the height of a status chip.
	ChipHeight = 24
)

// Tone is a semantic status colour family.
type Tone int

const (
	ToneNeutral Tone = iota
	ToneSuccess
	ToneWarning
	ToneDanger
	ToneInfo
)

// TonePalette holds the colours of a status chip.
type TonePalette struct {
	Foreground walk.Color
	Background walk.Color
	Dot        walk.Color
}

// Palette returns the light-theme colours of the tone.
func (t Tone) Palette() TonePalette {
	switch t {
	case ToneSuccess:
		return TonePalette{walk.RGB(0x06, 0x76, 0x47), walk.RGB(0xEC, 0xFD, 0xF3), walk.RGB(0x17, 0xB2, 0x6A)}
	case ToneWarning:
		return TonePalette{walk.RGB(0xB5, 0x47, 0x08), walk.RGB(0xFF, 0xFA, 0xEB), walk.RGB(0xF7, 0x90, 0x09)}
	case ToneDanger:
		return TonePalette{walk.RGB(0xB4, 0x23, 0x18), walk.RGB(0xFE, 0xF3, 0xF2), walk.RGB(0xF0, 0x44, 0x38)}
	case ToneInfo:
		return TonePalette{walk.RGB(0x18, 0x49, 0xA9), walk.RGB(0xEE, 0xF4, 0xFF), walk.RGB(0x2E, 0x90, 0xFA)}
	default:
		return TonePalette{walk.RGB(0x34, 0x40, 0x54), walk.RGB(0xF2, 0xF4, 0xF7), walk.RGB(0x98, 0xA2, 0xB3)}
	}
}

// String returns the design-system token name of the tone.
func (t Tone) String() string {
	switch t {
	case ToneSuccess:
		return "success"
	case ToneWarning:
		return "warning"
	case ToneDanger:
		return "danger"
	case ToneInfo:
		return "info"
	default:
		return "neutral"
	}
}

var (
	neutralPhrases = []string{"no active", "not installed", "not configured", "idle", "unknown", "stopped"}
	dangerPhrases  = []string{"disconnected", "offline", "error", "fail", "unreachable", "unavailable", "denied"}
	warningPhrases = []string{"checking", "check relay", "connecting", "starting", "start pending", "stopping", "stop pending", "pending", "retry", "establishing", "degraded", "migrat", "paused"}
	successPhrases = []string{"running", "connected", "online", "active", "installed", "ready"}
)

// ToneFor maps a status word or short status sentence to its tone using the
// shared status vocabulary: Running/Connected/Online/Active are success,
// Checking/Connecting/Starting/Check relay are warning, Offline/Error/failed
// are danger, and Stopped/Unknown are neutral.
func ToneFor(text string) Tone {
	value := strings.ToLower(strings.TrimSpace(text))
	if value == "" {
		return ToneNeutral
	}
	for _, phrases := range []struct {
		tone    Tone
		phrases []string
	}{
		{ToneNeutral, neutralPhrases},
		{ToneDanger, dangerPhrases},
		{ToneWarning, warningPhrases},
		{ToneSuccess, successPhrases},
	} {
		for _, phrase := range phrases.phrases {
			if strings.Contains(value, phrase) {
				return phrases.tone
			}
		}
	}
	return ToneNeutral
}
