//go:build windows

package sharebanner

import (
	"strings"
	"testing"

	"deskferry/internal/screenview"
)

func TestDescribe(t *testing.T) {
	tests := []struct {
		name    string
		state   screenview.SessionState
		title   string
		caption string
		badge   string
	}{
		{
			name:    "person viewing",
			state:   screenview.SessionState{ClientName: "HOME-PC", ClientKind: "viewer", Live: true},
			title:   "A person on HOME-PC is viewing your screen",
			caption: "Screen shared through DeskFerry · Remote control is off",
		},
		{
			name:    "person controlling",
			state:   screenview.SessionState{ClientName: "HOME-PC", ClientKind: "viewer", Live: true, ControlActive: true},
			title:   "A person on HOME-PC can use your mouse and keyboard",
			caption: "Screen shared through DeskFerry · Remote control is on",
		},
		{
			name:    "agent viewing",
			state:   screenview.SessionState{ClientName: "HOME-PC", ClientKind: "agent", Live: true},
			title:   "An AI agent on HOME-PC is viewing your screen",
			caption: "Screen shared through DeskFerry · Remote control is off",
			badge:   "AI agent",
		},
		{
			name:    "agent controlling without a name",
			state:   screenview.SessionState{ClientKind: " Agent ", Live: true, ControlActive: true},
			title:   "An AI agent can use your mouse and keyboard",
			caption: "Screen shared through DeskFerry · Remote control is on",
			badge:   "AI agent",
		},
		{
			name:    "person without a name",
			state:   screenview.SessionState{ClientKind: "viewer", Live: true},
			title:   "A person is viewing your screen",
			caption: "Screen shared through DeskFerry · Remote control is off",
		},
		{
			name:    "legacy viewer",
			state:   screenview.SessionState{ClientName: "HOME-PC"},
			title:   "HOME-PC is viewing your screen",
			caption: "Screen shared through DeskFerry · View only",
		},
		{
			name:    "nothing known yet",
			state:   screenview.SessionState{},
			title:   "Another computer is viewing your screen",
			caption: "Screen shared through DeskFerry · View only",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := describe(test.state)
			if got.title != test.title || got.caption != test.caption || got.badge != test.badge {
				t.Fatalf("describe() = %q / %q / badge %q, want %q / %q / badge %q",
					got.title, got.caption, got.badge, test.title, test.caption, test.badge)
			}
			if got.control != test.state.ControlActive {
				t.Fatalf("control = %v, want %v", got.control, test.state.ControlActive)
			}
			if !strings.Contains(got.accessibleText(), "DeskFerry") {
				t.Fatalf("accessible text %q does not name DeskFerry", got.accessibleText())
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"HOME-PC":                      "HOME-PC",
		"  Ben's   laptop \r\n":        "Ben's laptop",
		"evil\u202ename\x00\x07":       "evilname",
		"line one\nline two":           "line one line two",
		"書房的電腦":                        "書房的電腦",
		"":                             "",
		" \t ":                         "",
		strings.Repeat("a", 32):        strings.Repeat("a", 32),
		strings.Repeat("a", 33):        strings.Repeat("a", 32) + "…",
		strings.Repeat("ab ", 20):      strings.Repeat("ab ", 10) + "ab…",
		strings.Repeat("a", 31) + " b": strings.Repeat("a", 31) + "…",
	}
	for input, want := range tests {
		if got := sanitizeName(input); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSchemeDistinguishesStates(t *testing.T) {
	personView := schemeFor(describe(screenview.SessionState{ClientKind: "viewer", Live: true}))
	personControl := schemeFor(describe(screenview.SessionState{ClientKind: "viewer", Live: true, ControlActive: true}))
	agentView := schemeFor(describe(screenview.SessionState{ClientKind: "agent", Live: true}))
	agentControl := schemeFor(describe(screenview.SessionState{ClientKind: "agent", Live: true, ControlActive: true}))

	if personView.fill == personControl.fill || agentView.fill == agentControl.fill {
		t.Fatal("remote control must change the banner fill")
	}
	if personView.border == agentView.border || personControl.border == agentControl.border {
		t.Fatal("an AI agent must have its own accent")
	}
	if agentView.badgeFill == agentView.fill || agentControl.badgeFill == agentControl.fill {
		t.Fatal("the AI agent badge must stand out from the banner fill")
	}
}

func TestButtonStyles(t *testing.T) {
	view := describe(screenview.SessionState{Live: true})
	control := describe(screenview.SessionState{Live: true, ControlActive: true})
	for _, c := range []content{view, control} {
		colors := schemeFor(c)
		for index := 0; index < buttonCount; index++ {
			normal := buttonStyleFor(index, c, colors, false, false, true)
			hover := buttonStyleFor(index, c, colors, true, false, true)
			pressed := buttonStyleFor(index, c, colors, true, true, true)
			disabled := buttonStyleFor(index, c, colors, false, false, false)
			if normal.fill == hover.fill || hover.fill == pressed.fill {
				t.Errorf("button %d (control=%v): hover and pressed need their own fill", index, c.control)
			}
			if normal.text == normal.fill || disabled.text == disabled.fill {
				t.Errorf("button %d (control=%v): text must differ from its fill", index, c.control)
			}
			if disabled == normal {
				t.Errorf("button %d (control=%v): disabled must look different", index, c.control)
			}
		}
	}
}

func TestMix(t *testing.T) {
	black, white := mix(0, 0, 0), mix(0xFFFFFF, 0xFFFFFF, 1)
	if got := mix(black, white, 0); got != black {
		t.Fatalf("mix 0 = %#x", got)
	}
	if got := mix(black, white, 1); got != white {
		t.Fatalf("mix 1 = %#x", got)
	}
	if got := mix(black, white, 0.5); got.R() != 0x80 || got.G() != 0x80 || got.B() != 0x80 {
		t.Fatalf("mix 0.5 = %#x", got)
	}
}
