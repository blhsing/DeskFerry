package screenview

import (
	"image"
	"math/rand"
	"testing"
	"time"
)

// photoFrame fills a frame with smooth, noisy, photo-like content.
func photoFrame(width, height int, seed int64) *image.RGBA {
	random := rand.New(rand.NewSource(seed))
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := frame.PixOffset(x, y)
			frame.Pix[offset] = uint8((x*3+y+int(seed)*7)%256) ^ uint8(random.Intn(24))
			frame.Pix[offset+1] = uint8((x+y*2+int(seed)*5)%256) ^ uint8(random.Intn(24))
			frame.Pix[offset+2] = uint8((x*y/97+int(seed)*3)%256) ^ uint8(random.Intn(24))
			frame.Pix[offset+3] = 255
		}
	}
	return frame
}

func TestReportFullFrameEncodeCost(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	for _, size := range [][2]int{{1920, 1080}, {2880, 1800}} {
		frame := photoFrame(size[0], size[1], 1)
		for _, quality := range []int{70, 30} {
			grid := newTileGrid(size[0], size[1])
			grid.markAll()
			started := time.Now()
			rects, lossy, err := grid.encodeTiles(frame, func(int) bool { return true }, encodeMode{Quality: quality})
			if err != nil {
				t.Fatal(err)
			}
			bytes := 0
			for _, rect := range rects {
				bytes += len(rect.Data)
			}
			elapsed := time.Since(started)
			client := &LiveClient{fb: image.NewRGBA(frame.Bounds())}
			decodeStarted := time.Now()
			if _, err := client.drawRects(rects); err != nil {
				t.Fatal(err)
			}
			t.Logf("%dx%d q%d: %d rects (%d lossy tiles), %d KiB, encode %s, decode %s", size[0], size[1], quality, len(rects), lossy, bytes>>10, elapsed.Round(time.Millisecond), time.Since(decodeStarted).Round(time.Millisecond))
		}
	}
}
