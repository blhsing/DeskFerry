package relaydashboard

import (
	"strings"
	"testing"
)

func TestHTMLRendersPlaceholders(t *testing.T) {
	page := HTML("1.2.3", "", "Go relay")
	for _, want := range []string{"v1.2.3", "Go relay", `var pageRoom="";`, `id="roomUrl"`, `id="copyRoom"`, `id="rooms"`, "/relay/icon.svg"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(page, "{{") {
		t.Fatal("dashboard contains an unreplaced placeholder")
	}
}

func TestHTMLEscapesHostileRoom(t *testing.T) {
	room := "</script><script>alert(1)</script>{{VERSION}}&\u2028"
	page := HTML("1.2.3", room, "<b>kind</b>")
	if strings.Count(page, "</script>") != 1 {
		t.Fatalf("hostile room broke out of the script element")
	}
	if !strings.Contains(page, `var pageRoom="\u003c/script\u003e\u003cscript\u003ealert(1)\u003c/script\u003e{{VERSION}}\u0026\u2028";`) {
		t.Fatalf("room JSON not escaped as expected")
	}
	if strings.Contains(page, "<b>kind</b>") || !strings.Contains(page, "&lt;b&gt;kind&lt;/b&gt;") {
		t.Fatal("relay kind not HTML-escaped")
	}
}

func TestIconSVG(t *testing.T) {
	if !strings.HasPrefix(IconSVG(), "<svg") {
		t.Fatal("icon does not start with <svg")
	}
}
