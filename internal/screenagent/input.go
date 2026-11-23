package screenagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"deskferry/internal/screenview"
)

// keyStroke is one key of a combination: a virtual key or a character the
// Work PC resolves for its keyboard layout.
type keyStroke struct {
	VK   int
	Ext  bool
	Char string
}

var namedKeys = map[string]keyStroke{
	"ctrl": {VK: 0x11}, "control": {VK: 0x11}, "ctrl_l": {VK: 0xA2}, "control_l": {VK: 0xA2}, "ctrl_r": {VK: 0xA3, Ext: true}, "control_r": {VK: 0xA3, Ext: true},
	"shift": {VK: 0x10}, "shift_l": {VK: 0xA0}, "shift_r": {VK: 0xA1},
	"alt": {VK: 0x12}, "alt_l": {VK: 0xA4}, "alt_r": {VK: 0xA5, Ext: true}, "option": {VK: 0x12}, "altgr": {VK: 0xA5, Ext: true},
	"super": {VK: 0x5B, Ext: true}, "super_l": {VK: 0x5B, Ext: true}, "super_r": {VK: 0x5C, Ext: true}, "win": {VK: 0x5B, Ext: true}, "windows": {VK: 0x5B, Ext: true},
	"meta": {VK: 0x5B, Ext: true}, "cmd": {VK: 0x5B, Ext: true}, "command": {VK: 0x5B, Ext: true},
	"return": {VK: 0x0D}, "enter": {VK: 0x0D}, "kp_enter": {VK: 0x0D, Ext: true},
	"tab": {VK: 0x09}, "iso_left_tab": {VK: 0x09},
	"escape": {VK: 0x1B}, "esc": {VK: 0x1B},
	"backspace": {VK: 0x08}, "back_space": {VK: 0x08},
	"delete": {VK: 0x2E, Ext: true}, "del": {VK: 0x2E, Ext: true}, "insert": {VK: 0x2D, Ext: true}, "ins": {VK: 0x2D, Ext: true},
	"home": {VK: 0x24, Ext: true}, "end": {VK: 0x23, Ext: true},
	"page_up": {VK: 0x21, Ext: true}, "pageup": {VK: 0x21, Ext: true}, "prior": {VK: 0x21, Ext: true},
	"page_down": {VK: 0x22, Ext: true}, "pagedown": {VK: 0x22, Ext: true}, "next": {VK: 0x22, Ext: true},
	"left": {VK: 0x25, Ext: true}, "up": {VK: 0x26, Ext: true}, "right": {VK: 0x27, Ext: true}, "down": {VK: 0x28, Ext: true},
	"space": {VK: 0x20}, "spacebar": {VK: 0x20},
	"print": {VK: 0x2C, Ext: true}, "printscreen": {VK: 0x2C, Ext: true}, "print_screen": {VK: 0x2C, Ext: true}, "sys_req": {VK: 0x2C, Ext: true},
	"pause": {VK: 0x13}, "break": {VK: 0x13},
	"menu": {VK: 0x5D, Ext: true}, "apps": {VK: 0x5D, Ext: true},
	"caps_lock": {VK: 0x14}, "capslock": {VK: 0x14}, "num_lock": {VK: 0x90, Ext: true}, "numlock": {VK: 0x90, Ext: true}, "scroll_lock": {VK: 0x91},
	"kp_add": {VK: 0x6B}, "kp_subtract": {VK: 0x6D}, "kp_multiply": {VK: 0x6A}, "kp_divide": {VK: 0x6F, Ext: true}, "kp_decimal": {VK: 0x6E},
	"minus": {Char: "-"}, "equal": {Char: "="}, "plus": {Char: "+"}, "comma": {Char: ","}, "period": {Char: "."}, "slash": {Char: "/"},
	"backslash": {Char: "\\"}, "semicolon": {Char: ";"}, "apostrophe": {Char: "'"}, "quote": {Char: "'"}, "grave": {Char: "`"},
	"bracketleft": {Char: "["}, "bracketright": {Char: "]"}, "braceleft": {Char: "{"}, "braceright": {Char: "}"},
	"exclam": {Char: "!"}, "at": {Char: "@"}, "numbersign": {Char: "#"}, "dollar": {Char: "$"}, "percent": {Char: "%"},
	"asciicircum": {Char: "^"}, "ampersand": {Char: "&"}, "asterisk": {Char: "*"}, "parenleft": {Char: "("}, "parenright": {Char: ")"},
	"underscore": {Char: "_"}, "colon": {Char: ":"}, "quotedbl": {Char: "\""}, "less": {Char: "<"}, "greater": {Char: ">"},
	"question": {Char: "?"}, "bar": {Char: "|"}, "asciitilde": {Char: "~"},
	"volume_up": {VK: 0xAF, Ext: true}, "volume_down": {VK: 0xAE, Ext: true}, "volume_mute": {VK: 0xAD, Ext: true},
	"xf86audioraisevolume": {VK: 0xAF, Ext: true}, "xf86audiolowervolume": {VK: 0xAE, Ext: true}, "xf86audiomute": {VK: 0xAD, Ext: true},
}

// parseKey parses one key name such as "Return", "ctrl", "F5", "a" or "KP_7".
func parseKey(name string) (keyStroke, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return keyStroke{}, fail("bad_request", "empty key name")
	}
	lower := strings.ToLower(trimmed)
	if key, ok := namedKeys[lower]; ok {
		return key, nil
	}
	if len(lower) >= 2 && lower[0] == 'f' {
		if number, err := strconv.Atoi(lower[1:]); err == nil && number >= 1 && number <= 24 {
			return keyStroke{VK: 0x70 + number - 1}, nil
		}
	}
	if strings.HasPrefix(lower, "kp_") && len(lower) == 4 && lower[3] >= '0' && lower[3] <= '9' {
		return keyStroke{VK: 0x60 + int(lower[3]-'0')}, nil
	}
	if utf8.RuneCountInString(trimmed) == 1 {
		r, _ := utf8.DecodeRuneInString(trimmed)
		switch {
		case r >= 'a' && r <= 'z':
			return keyStroke{VK: int(r - 'a' + 'A')}, nil
		case r >= 'A' && r <= 'Z':
			return keyStroke{VK: int(r)}, nil
		case r >= '0' && r <= '9':
			return keyStroke{VK: int(r)}, nil
		case r == ' ':
			return keyStroke{VK: 0x20}, nil
		}
		return keyStroke{Char: trimmed}, nil
	}
	return keyStroke{}, fail("bad_request", "unknown key %q; use xdotool-style names such as Return, Tab, Escape, BackSpace, Delete, Page_Down, F5, ctrl+s or super", name)
}

// parseCombo parses "ctrl+shift+t" into its keys.
func parseCombo(combo string) ([]keyStroke, error) {
	combo = strings.TrimSpace(combo)
	if combo == "+" {
		return []keyStroke{{Char: "+"}}, nil
	}
	var parts []string
	if strings.HasSuffix(combo, "++") {
		parts = append(strings.Split(strings.TrimSuffix(combo, "++"), "+"), "+")
	} else {
		parts = strings.Split(combo, "+")
	}
	keys := make([]keyStroke, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, fail("bad_request", "invalid key combination %q", combo)
		}
		key, err := parseKey(part)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func keyEvent(key keyStroke, down bool) screenview.InputEvent {
	if key.Char != "" {
		return screenview.InputEvent{T: "char_key", C: key.Char, Down: down}
	}
	return screenview.InputEvent{T: "key", VK: key.VK, Ext: key.Ext, Down: down}
}

// comboEvents presses keys in order and releases them in reverse.
func comboEvents(keys []keyStroke) []screenview.InputEvent {
	events := make([]screenview.InputEvent, 0, len(keys)*2)
	for _, key := range keys {
		events = append(events, keyEvent(key, true))
	}
	for index := len(keys) - 1; index >= 0; index-- {
		events = append(events, keyEvent(keys[index], false))
	}
	return events
}

func modifierKeys(text string) ([]keyStroke, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	return parseCombo(text)
}

func withModifiers(modifiers []keyStroke, events []screenview.InputEvent) []screenview.InputEvent {
	if len(modifiers) == 0 {
		return events
	}
	out := make([]screenview.InputEvent, 0, len(events)+len(modifiers)*2)
	for _, key := range modifiers {
		out = append(out, keyEvent(key, true))
	}
	out = append(out, events...)
	for index := len(modifiers) - 1; index >= 0; index-- {
		out = append(out, keyEvent(modifiers[index], false))
	}
	return out
}

func (a *Agent) input(ctx context.Context, action string, args Args) (Result, error) {
	client, err := a.session(ctx)
	if err != nil {
		return Result{}, err
	}
	var events []screenview.InputEvent
	var done string
	point := func(name string, required bool) (*int, *int, error) {
		coordinate, ok, err := args.point(name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			if required {
				return nil, nil, fail("bad_request", "%s needs %s [x, y]", action, name)
			}
			return nil, nil, nil
		}
		x, y, err := a.toNative(client, coordinate)
		if err != nil {
			return nil, nil, err
		}
		return screenview.At(x), screenview.At(y), nil
	}
	describe := func(x, y *int) string {
		if x == nil {
			return "at the pointer"
		}
		position := a.toScreenshot(client, *x, *y)
		return fmt.Sprintf("at [%d, %d]", position[0], position[1])
	}
	switch action {
	case "mouse_move":
		x, y, err := point("coordinate", true)
		if err != nil {
			return Result{}, err
		}
		events = []screenview.InputEvent{{T: "move", X: x, Y: y}}
		done = "Moved the pointer " + describe(x, y) + "."
	case "left_click", "right_click", "middle_click", "double_click", "triple_click":
		x, y, err := point("coordinate", false)
		if err != nil {
			return Result{}, err
		}
		modifierText, err := args.str("text")
		if err != nil {
			return Result{}, err
		}
		modifiers, err := modifierKeys(modifierText)
		if err != nil {
			return Result{}, err
		}
		button, clicks := "left", 1
		switch action {
		case "right_click":
			button = "right"
		case "middle_click":
			button = "middle"
		case "double_click":
			clicks = 2
		case "triple_click":
			clicks = 3
		}
		var clickEvents []screenview.InputEvent
		if x != nil {
			clickEvents = append(clickEvents, screenview.InputEvent{T: "move", X: x, Y: y})
		}
		for i := 0; i < clicks; i++ {
			clickEvents = append(clickEvents, screenview.InputEvent{T: "button", B: button, Down: true}, screenview.InputEvent{T: "button", B: button, Down: false})
		}
		events = withModifiers(modifiers, clickEvents)
		done = fmt.Sprintf("%s %s.", map[string]string{"left_click": "Left-clicked", "right_click": "Right-clicked", "middle_click": "Middle-clicked", "double_click": "Double-clicked", "triple_click": "Triple-clicked"}[action], describe(x, y))
	case "left_mouse_down", "left_mouse_up":
		x, y, err := point("coordinate", false)
		if err != nil {
			return Result{}, err
		}
		if x != nil {
			events = append(events, screenview.InputEvent{T: "move", X: x, Y: y})
		}
		down := action == "left_mouse_down"
		events = append(events, screenview.InputEvent{T: "button", B: "left", Down: down})
		done = map[bool]string{true: "Pressed", false: "Released"}[down] + " the left mouse button " + describe(x, y) + "."
	case "left_click_drag":
		startX, startY, err := point("start_coordinate", true)
		if err != nil {
			return Result{}, err
		}
		endX, endY, err := point("coordinate", true)
		if err != nil {
			return Result{}, err
		}
		events = append(events, screenview.InputEvent{T: "move", X: startX, Y: startY}, screenview.InputEvent{T: "button", B: "left", Down: true})
		const steps = 12
		for step := 1; step <= steps; step++ {
			x := *startX + (*endX-*startX)*step/steps
			y := *startY + (*endY-*startY)*step/steps
			events = append(events, screenview.InputEvent{T: "move", X: screenview.At(x), Y: screenview.At(y)})
		}
		events = append(events, screenview.InputEvent{T: "button", B: "left", Down: false})
		done = "Dragged from " + describe(startX, startY)[3:] + " to " + describe(endX, endY)[3:] + "."
	case "scroll":
		x, y, err := point("coordinate", false)
		if err != nil {
			return Result{}, err
		}
		direction, err := args.str("scroll_direction")
		if err != nil {
			return Result{}, err
		}
		amount, err := args.number("scroll_amount", 3)
		if err != nil {
			return Result{}, err
		}
		if amount <= 0 || amount > 100 {
			return Result{}, fail("bad_request", "scroll_amount must be from 1 to 100")
		}
		event := screenview.InputEvent{T: "wheel", X: x, Y: y}
		notches := int(amount * 120)
		switch strings.ToLower(direction) {
		case "up":
			event.DY = notches
		case "down", "":
			event.DY = -notches
		case "left":
			event.DX = -notches
		case "right":
			event.DX = notches
		default:
			return Result{}, fail("bad_request", "scroll_direction must be up, down, left or right")
		}
		modifierText, err := args.str("text")
		if err != nil {
			return Result{}, err
		}
		modifiers, err := modifierKeys(modifierText)
		if err != nil {
			return Result{}, err
		}
		events = withModifiers(modifiers, []screenview.InputEvent{event})
		done = fmt.Sprintf("Scrolled %s by %v %s.", strings.ToLower(firstNonEmpty(direction, "down")), amount, describe(x, y))
	case "type":
		text, err := args.str("text")
		if err != nil {
			return Result{}, err
		}
		if text == "" {
			return Result{}, fail("bad_request", "type needs text")
		}
		if err := a.ensureControl(ctx, client); err != nil {
			return Result{}, err
		}
		// Send long text in pieces so each command stays small.
		runes := []rune(text)
		for start := 0; start < len(runes); start += 200 {
			end := min(len(runes), start+200)
			if err := client.Input(ctx, screenview.InputEvent{T: "text", S: string(runes[start:end])}); err != nil {
				return Result{}, err
			}
		}
		return a.afterInput(ctx, client, fmt.Sprintf("Typed %d characters.", len(runes)))
	case "key":
		text, err := args.str("text")
		if err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(text) == "" {
			return Result{}, fail("bad_request", "key needs text such as \"Return\" or \"ctrl+s\"")
		}
		for _, combo := range strings.Fields(text) {
			keys, err := parseCombo(combo)
			if err != nil {
				return Result{}, err
			}
			events = append(events, comboEvents(keys)...)
		}
		done = "Pressed " + text + "."
	case "hold_key":
		text, err := args.str("text")
		if err != nil {
			return Result{}, err
		}
		seconds, err := args.number("duration", 1)
		if err != nil {
			return Result{}, err
		}
		if seconds <= 0 || seconds > 100 {
			return Result{}, fail("bad_request", "duration must be from 0 to 100 seconds")
		}
		keys, err := parseCombo(text)
		if err != nil {
			return Result{}, err
		}
		if err := a.ensureControl(ctx, client); err != nil {
			return Result{}, err
		}
		var down, up []screenview.InputEvent
		for _, key := range keys {
			down = append(down, keyEvent(key, true))
		}
		for index := len(keys) - 1; index >= 0; index-- {
			up = append(up, keyEvent(keys[index], false))
		}
		if err := client.Input(ctx, down...); err != nil {
			return Result{}, err
		}
		select {
		case <-time.After(time.Duration(seconds * float64(time.Second))):
		case <-ctx.Done():
		}
		if err := client.Input(context.Background(), up...); err != nil {
			return Result{}, err
		}
		return a.afterInput(ctx, client, fmt.Sprintf("Held %s for %v seconds.", text, seconds))
	default:
		return Result{}, fail("bad_request", "unknown action %q", action)
	}
	if err := a.ensureControl(ctx, client); err != nil {
		return Result{}, err
	}
	if err := client.Input(ctx, events...); err != nil {
		return Result{}, err
	}
	return a.afterInput(ctx, client, done)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
