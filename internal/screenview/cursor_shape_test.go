package screenview

import "testing"

func TestCursorFromBlackWhite(t *testing.T) {
	// Four BGRA pixels: transparent, opaque red, half-transparent white, and
	// an inverting pixel.
	black := []byte{
		0, 0, 0, 255,
		0, 0, 255, 255,
		128, 128, 128, 255,
		255, 255, 255, 255,
	}
	white := []byte{
		255, 255, 255, 255,
		0, 0, 255, 255,
		255, 255, 255, 255,
		0, 0, 0, 255,
	}
	out := cursorFromBlackWhite(black, white, 4, 1)
	check := func(index int, want [4]byte) {
		t.Helper()
		got := [4]byte(out.Pix[index*4 : index*4+4])
		if got != want {
			t.Errorf("pixel %d = %v, want %v", index, got, want)
		}
	}
	check(0, [4]byte{0, 0, 0, 0})
	check(1, [4]byte{255, 0, 0, 255})
	check(2, [4]byte{255, 255, 255, 128})
	check(3, [4]byte{32, 32, 32, 255})
}
