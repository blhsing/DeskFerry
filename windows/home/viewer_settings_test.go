//go:build windows

package homewindows

import "testing"

func TestParseMinFPS(t *testing.T) {
	for text, want := range map[string]int{"0 (lossless)": 0, "30": 30, " 45 fps": 45, "60fps": 60, "120": 120} {
		if got, ok := parseMinFPS(text); !ok || got != want {
			t.Fatalf("parseMinFPS(%q) = %d, %t", text, got, ok)
		}
	}
	for _, text := range []string{"", "-1", "121", "fast"} {
		if _, ok := parseMinFPS(text); ok {
			t.Fatalf("parseMinFPS(%q) accepted", text)
		}
	}
	if formatMinFPS(0) != "0 (lossless)" || formatMinFPS(24) != "24" {
		t.Fatal("formatMinFPS")
	}
}

func TestViewerMinFPSIsRememberedPerDestination(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	if got := loadViewerMinFPS("PX13"); got != 30 {
		t.Fatalf("default = %d", got)
	}
	if err := saveViewerMinFPS("PX13", 0); err != nil {
		t.Fatal(err)
	}
	if loadViewerMinFPS("px13") != 0 || loadViewerMinFPS("Home") != 30 {
		t.Fatal("Min fps was not remembered per destination")
	}
}
