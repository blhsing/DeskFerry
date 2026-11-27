//go:build windows

package sharebanner

import (
	"testing"

	"github.com/lxn/walk"
)

func newTestSurface(width, height int) *surface {
	return &surface{width: width, height: height, pix: make([]byte, width*height*4)}
}

func (s *surface) at(x, y int) (b, g, r, a byte) {
	pixel := s.pix[(y*s.width+x)*4:]
	return pixel[0], pixel[1], pixel[2], pixel[3]
}

func TestFillRoundRect(t *testing.T) {
	canvas := newTestSurface(40, 24)
	color := walk.RGB(0x11, 0x1A, 0x2E)
	canvas.fillRoundRect(4, 2, 32, 20, 8, color)

	if _, _, _, a := canvas.at(0, 0); a != 0 {
		t.Fatalf("outside pixel alpha = %d, want 0", a)
	}
	if _, _, _, a := canvas.at(4, 2); a != 0 {
		t.Fatalf("pixel beyond the rounded corner alpha = %d, want 0", a)
	}
	if b, g, r, a := canvas.at(20, 12); a != 255 || r != 0x11 || g != 0x1A || b != 0x2E {
		t.Fatalf("centre pixel = %02x%02x%02x alpha %d", r, g, b, a)
	}
	if _, _, _, a := canvas.at(20, 2); a != 255 {
		t.Fatalf("top edge pixel alpha = %d, want 255", a)
	}
	partial := 0
	for y := 0; y < canvas.height; y++ {
		for x := 0; x < canvas.width; x++ {
			b, g, r, a := canvas.at(x, y)
			if a > 0 && a < 255 {
				partial++
			}
			if b > a || g > a || r > a {
				t.Fatalf("pixel %d,%d is not premultiplied: %02x%02x%02x alpha %d", x, y, r, g, b, a)
			}
		}
	}
	if partial == 0 {
		t.Fatal("rounded corners have no anti-aliased pixels")
	}
}

func TestFillRoundRectCoversOpaquely(t *testing.T) {
	canvas := newTestSurface(16, 16)
	canvas.fillRoundRect(0, 0, 16, 16, 0, walk.RGB(0xFF, 0x00, 0x00))
	canvas.fillRoundRect(4, 4, 8, 8, 4, walk.RGB(0x00, 0xFF, 0x00))
	if b, g, r, a := canvas.at(8, 8); a != 255 || r != 0 || g != 0xFF || b != 0 {
		t.Fatalf("circle centre = %02x%02x%02x alpha %d", r, g, b, a)
	}
	if b, g, r, a := canvas.at(1, 1); a != 255 || r != 0xFF || g != 0 || b != 0 {
		t.Fatalf("background = %02x%02x%02x alpha %d", r, g, b, a)
	}
}

func TestAlphaChannelRoundTrip(t *testing.T) {
	canvas := newTestSurface(8, 8)
	canvas.fillRoundRect(0, 0, 8, 8, 3, walk.RGB(1, 2, 3))
	saved := canvas.alphaChannel()
	for i := 3; i < len(canvas.pix); i += 4 {
		canvas.pix[i] = 0
	}
	canvas.setAlphaChannel(saved)
	if _, _, _, a := canvas.at(4, 4); a != 255 {
		t.Fatalf("restored alpha = %d, want 255", a)
	}
	if _, _, _, a := canvas.at(0, 0); a == 255 {
		t.Fatalf("restored corner alpha = %d, want less than 255", a)
	}
}

func TestFillCapsuleCoversTheStrokeOnly(t *testing.T) {
	s := &surface{width: 20, height: 20, pix: make([]byte, 20*20*4)}
	s.fillCapsule(4, 4, 16, 16, 2, walk.RGB(255, 0, 0))
	alpha := func(x, y int) byte { return s.pix[(y*20+x)*4+3] }
	if alpha(10, 10) < 250 {
		t.Fatalf("the stroke's middle has alpha %d; want opaque", alpha(10, 10))
	}
	if alpha(16, 4) != 0 || alpha(4, 16) != 0 {
		t.Fatal("pixels far from the stroke were painted")
	}
	if alpha(16, 16) == 0 {
		t.Fatal("the round cap at the end was not painted")
	}
}
