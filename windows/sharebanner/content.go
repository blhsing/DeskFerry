//go:build windows

package sharebanner

import (
	"strings"
	"unicode"

	"github.com/lxn/walk"

	"deskferry/internal/screenview"
	"deskferry/windows/uikit"
)

const (
	// maxNameRunes caps the Home PC name, which the remote side chooses.
	maxNameRunes = 32

	captionShared = "Screen shared through DeskFerry"
	badgeAgent    = "AI agent"

	labelStopControl = "Stop control"
	labelStopSharing = "Stop sharing"
	labelStopping    = "Stopping…"
)

// clientKind says who is on the remote side.
type clientKind int

const (
	kindUnknown clientKind = iota // legacy viewer that did not say
	kindPerson
	kindAgent
)

// content is the text and tone of the banner for one session state.
type content struct {
	title   string // who is doing what
	caption string // shared through DeskFerry, and the remote-control status
	badge   string // "AI agent", or empty
	kind    clientKind
	control bool // the remote side can use the mouse and keyboard
}

// describe composes the banner content for a session state.
func describe(state screenview.SessionState) content {
	name := sanitizeName(state.ClientName)
	result := content{kind: kindOf(state.ClientKind), control: state.ControlActive}

	var who string
	switch result.kind {
	case kindAgent:
		who = "An AI agent"
		result.badge = badgeAgent
	case kindPerson:
		who = "A person"
	}
	switch {
	case who == "" && name == "":
		who = "Another computer"
	case who == "":
		who = name
	case name != "":
		who += " on " + name
	}

	switch {
	case result.control:
		result.title = who + " can use your mouse and keyboard"
		result.caption = captionShared + " · Remote control is on"
	case state.Live:
		result.title = who + " is viewing your screen"
		result.caption = captionShared + " · Remote control is off"
	default:
		result.title = who + " is viewing your screen"
		result.caption = captionShared + " · View only"
	}
	return result
}

// accessibleText is the window text announced by assistive technology.
func (c content) accessibleText() string {
	return "DeskFerry screen sharing: " + c.title + ". " + c.caption + "."
}

func kindOf(clientKind string) clientKind {
	switch strings.ToLower(strings.TrimSpace(clientKind)) {
	case "agent":
		return kindAgent
	case "viewer":
		return kindPerson
	default:
		return kindUnknown
	}
}

// sanitizeName makes a remote-supplied name safe to show on one line: it
// drops control and invisible formatting characters (including bidirectional
// overrides), collapses white space, and truncates long names.
func sanitizeName(name string) string {
	var builder strings.Builder
	space := false
	count := 0
	for _, r := range name {
		if unicode.IsSpace(r) {
			space = builder.Len() > 0
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if space {
			builder.WriteByte(' ')
			count++
			space = false
		}
		if count >= maxNameRunes {
			return strings.TrimRight(builder.String(), " ") + "…"
		}
		builder.WriteRune(r)
		count++
	}
	return builder.String()
}

// scheme holds the banner colours for one content.
type scheme struct {
	fill      walk.Color
	border    walk.Color
	title     walk.Color
	caption   walk.Color
	dot       walk.Color
	badgeFill walk.Color
	badgeText walk.Color
}

// schemeFor picks the colours. Viewing uses the dark surface with a primary
// outline; remote control fills the whole banner with the danger colour. An
// AI agent always carries the warning accent (outline and badge).
func schemeFor(c content) scheme {
	danger := uikit.ToneDanger.Palette()
	warning := uikit.ToneWarning.Palette()
	result := scheme{
		fill:    uikit.ColorDarkSurface,
		border:  uikit.ColorDarkPrimary,
		title:   uikit.ColorDarkText,
		caption: uikit.ColorDarkTextSecondary,
		dot:     uikit.ToneInfo.Palette().Dot,
	}
	if c.control {
		result.fill = danger.Foreground
		result.border = danger.Dot
		result.title = uikit.ColorPrimaryOn
		result.caption = danger.Background
		result.dot = uikit.ColorPrimaryOn
	}
	if c.kind == kindAgent {
		result.border = warning.Dot
		result.badgeFill = warning.Dot
		result.badgeText = uikit.ColorText
		if !c.control {
			result.dot = warning.Dot
		}
	}
	return result
}

// buttonStyle holds the colours of one banner button.
type buttonStyle struct {
	fill     walk.Color
	border   walk.Color
	bordered bool
	text     walk.Color
}

const (
	buttonStopControl = iota
	buttonStopSharing
	buttonCount
)

// buttonStyleFor returns the colours of a button. While viewing, Stop sharing
// is the primary button. While remote control is on, Stop control is the
// primary action (surface on danger) and Stop sharing is secondary.
func buttonStyleFor(index int, c content, colors scheme, hover, pressed, enabled bool) buttonStyle {
	white := uikit.ColorPrimaryOn
	if !c.control {
		style := buttonStyle{fill: uikit.ColorPrimary, text: uikit.ColorPrimaryOn}
		switch {
		case !enabled:
			style.fill = mix(colors.fill, uikit.ColorPrimary, 0.35)
			style.text = uikit.ColorDarkTextMuted
		case pressed:
			style.fill = uikit.ColorPrimaryActive
		case hover:
			style.fill = uikit.ColorPrimaryHover
		}
		return style
	}
	danger := uikit.ToneDanger.Palette()
	if index == buttonStopControl {
		style := buttonStyle{fill: uikit.ColorSurface, text: danger.Foreground}
		switch {
		case !enabled:
			style.fill = mix(colors.fill, white, 0.25)
			style.text = mix(colors.fill, white, 0.7)
		case pressed:
			style.fill = mix(danger.Background, danger.Foreground, 0.14)
		case hover:
			style.fill = danger.Background
		}
		return style
	}
	style := buttonStyle{fill: colors.fill, border: mix(colors.fill, white, 0.6), bordered: true, text: white}
	switch {
	case !enabled:
		style.border = mix(colors.fill, white, 0.25)
		style.text = mix(colors.fill, white, 0.6)
	case pressed:
		style.fill = mix(colors.fill, white, 0.24)
	case hover:
		style.fill = mix(colors.fill, white, 0.14)
	}
	return style
}

// mix blends amount (0..1) of b into a.
func mix(a, b walk.Color, amount float64) walk.Color {
	channel := func(x, y byte) byte {
		return byte(float64(x)*(1-amount) + float64(y)*amount + 0.5)
	}
	return walk.RGB(channel(a.R(), b.R()), channel(a.G(), b.G()), channel(a.B(), b.B()))
}
