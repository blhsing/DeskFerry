// Package relaydashboard holds the single relay status dashboard page and
// icon shared by the Go, .NET, and Python relays. The .NET relay embeds these
// files as resources and the Python relay reads them from disk, so all three
// substitute the same plain-text placeholders:
//
//	{{VERSION}}    relay version, HTML-escaped
//	{{RELAY_KIND}} short relay description, HTML-escaped
//	{{ROOM_JSON}}  JSON string literal for the page room, with <, >, &,
//	               U+2028, and U+2029 escaped so it is safe inside <script>
//
// ROOM_JSON must be substituted last so a hostile room name cannot introduce
// another placeholder.
package relaydashboard

import (
	_ "embed"
	"encoding/json"
	"html"
	"strings"
)

//go:embed dashboard.html
var dashboardTemplate string

//go:embed icon.svg
var iconSVG string

// HTML renders the dashboard for room ("" for the overview).
func HTML(version, room, relayKind string) string {
	page := strings.ReplaceAll(dashboardTemplate, "{{VERSION}}", html.EscapeString(version))
	page = strings.ReplaceAll(page, "{{RELAY_KIND}}", html.EscapeString(relayKind))
	return strings.ReplaceAll(page, "{{ROOM_JSON}}", RoomJSON(room))
}

// RoomJSON returns room as a JSON string literal that is safe to embed in an
// HTML script element.
func RoomJSON(room string) string {
	encoded, _ := json.Marshal(room)
	return strings.NewReplacer(
		"<", `\u003c`,
		">", `\u003e`,
		"&", `\u0026`,
		"\u2028", `\u2028`,
		"\u2029", `\u2029`,
	).Replace(string(encoded))
}

// IconSVG returns the DeskFerry relay icon.
func IconSVG() string {
	return iconSVG
}
