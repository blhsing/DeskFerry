package screenview

import (
	"image"
	"testing"
)

func TestPrimaryDisplayBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		bounds image.Rectangle
		want   bool
	}{
		{name: "primary", bounds: image.Rect(0, 0, 1920, 1080), want: true},
		{name: "primary left of origin", bounds: image.Rect(-1920, -200, 1, 880), want: true},
		{name: "secondary right", bounds: image.Rect(1920, 0, 3840, 1080)},
		{name: "secondary left", bounds: image.Rect(-1920, 0, 0, 1080)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := primaryDisplayBounds(test.bounds); got != test.want {
				t.Fatalf("primaryDisplayBounds(%v) = %t, want %t", test.bounds, got, test.want)
			}
		})
	}
}
