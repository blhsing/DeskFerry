//go:build windows

package screenview

import "testing"

// TestSystemCursorsAreNotSolidBoxes captures the standard Windows pointers and
// checks that their surroundings stay transparent, as the viewer must not
// draw a pointer as a solid block over the picture.
func TestSystemCursorsAreNotSolidBoxes(t *testing.T) {
	loadCursor := user32.NewProc("LoadCursorW")
	cursors := map[string]uintptr{"arrow": 32512, "ibeam": 32513, "wait": 32514, "cross": 32515, "sizeall": 32646, "hand": 32649}
	for name, id := range cursors {
		handle, _, _ := loadCursor.Call(0, id)
		if handle == 0 {
			t.Fatalf("%s: LoadCursor failed", name)
		}
		shape := readCursorShape(handle)
		if shape.image == nil {
			t.Fatalf("%s: no shape", name)
		}
		transparent, opaque := 0, 0
		for i := 3; i < len(shape.image.Pix); i += 4 {
			switch shape.image.Pix[i] {
			case 0:
				transparent++
			case 255:
				opaque++
			}
		}
		total := len(shape.image.Pix) / 4
		t.Logf("%s: %dx%d, %d transparent, %d opaque", name, shape.image.Rect.Dx(), shape.image.Rect.Dy(), transparent, opaque)
		if opaque == 0 || transparent < total/3 {
			t.Errorf("%s: %d of %d pixels transparent and %d opaque; the pointer would show as a block", name, transparent, total, opaque)
		}
		width, height := shape.image.Rect.Dx(), shape.image.Rect.Dy()
		corners := 0
		for _, point := range [][2]int{{0, 0}, {width - 1, 0}, {0, height - 1}, {width - 1, height - 1}} {
			if shape.image.Pix[shape.image.PixOffset(point[0], point[1])+3] == 0 {
				corners++
			}
		}
		if corners < 3 {
			t.Errorf("%s: only %d corners are transparent", name, corners)
		}
	}
}
