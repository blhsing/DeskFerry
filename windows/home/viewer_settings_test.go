//go:build windows

package homewindows

import (
	"image"
	"testing"
	"time"
)

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

func TestCursorMaskBitsMarksTransparentPixels(t *testing.T) {
	shape := image.NewRGBA(image.Rect(0, 0, 18, 2))
	shape.Pix[shape.PixOffset(0, 0)+3] = 255  // opaque
	shape.Pix[shape.PixOffset(17, 1)+3] = 200 // mostly opaque
	bits := cursorMaskBits(shape)
	if len(bits) != 8 {
		t.Fatalf("mask is %d bytes, want 2 rows of 4", len(bits))
	}
	if bits[0] != 0x7F || bits[1] != 0xFF || bits[2] != 0xC0 {
		t.Fatalf("first row = % X, want 7F FF C0", bits[:4])
	}
	if bits[4] != 0xFF || bits[5] != 0xFF || bits[6] != 0x80 {
		t.Fatalf("second row = % X, want FF FF 80", bits[4:8])
	}
}

func TestScreenReconnectDelayGrowsToThirtySeconds(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for attempt, delay := range want {
		if got := screenReconnectDelay(attempt); got != delay {
			t.Fatalf("attempt %d: %s, want %s", attempt, got, delay)
		}
	}
}
