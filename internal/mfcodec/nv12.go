package mfcodec

import (
	"image"
	"runtime"
	"sync"
)

// AlignedSize rounds a picture size up to the 16-pixel macroblock grid the
// encoder uses; decoders crop the padding off again.
func AlignedSize(width, height int) (int, int) {
	return (width + 15) &^ 15, (height + 15) &^ 15
}

func parallelRows(rows int, fn func(start, end int)) {
	workers := max(1, min(runtime.NumCPU(), rows/16))
	chunk := (rows + workers - 1) / workers
	// Keep chunks on even rows so 2x2 chroma blocks never straddle workers.
	chunk = (chunk + 1) &^ 1
	var wg sync.WaitGroup
	for start := 0; start < rows; start += chunk {
		end := min(rows, start+chunk)
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			fn(start, end)
		}(start, end)
	}
	wg.Wait()
}

// RGBAToNV12 converts rect of src to an NV12 picture of width x height (both
// even and at least rect's size), repeating edge pixels into the padding. It
// uses BT.601 limited-range coefficients.
func RGBAToNV12(src *image.RGBA, rect image.Rectangle, width, height int, dst []byte) []byte {
	size := width*height + width*height/2
	if cap(dst) < size {
		dst = make([]byte, size)
	}
	dst = dst[:size]
	luma := dst[:width*height]
	chroma := dst[width*height:]
	rw, rh := rect.Dx(), rect.Dy()
	pixel := func(x, y int) (int, int, int) {
		x = min(x, rw-1)
		y = min(y, rh-1)
		offset := src.PixOffset(rect.Min.X+x, rect.Min.Y+y)
		return int(src.Pix[offset]), int(src.Pix[offset+1]), int(src.Pix[offset+2])
	}
	parallelRows(height, func(start, end int) {
		for y := start; y < end; y++ {
			row := luma[y*width : (y+1)*width]
			for x := 0; x < width; x++ {
				r, g, b := pixel(x, y)
				row[x] = uint8((66*r+129*g+25*b+128)>>8 + 16)
			}
			if y%2 != 0 {
				continue
			}
			crow := chroma[(y/2)*width : (y/2)*width+width]
			for x := 0; x < width; x += 2 {
				r0, g0, b0 := pixel(x, y)
				r1, g1, b1 := pixel(x+1, y)
				r2, g2, b2 := pixel(x, y+1)
				r3, g3, b3 := pixel(x+1, y+1)
				r := (r0 + r1 + r2 + r3 + 2) / 4
				g := (g0 + g1 + g2 + g3 + 2) / 4
				b := (b0 + b1 + b2 + b3 + 2) / 4
				crow[x] = uint8((-38*r-74*g+112*b+128)>>8 + 128)
				crow[x+1] = uint8((112*r-94*g-18*b+128)>>8 + 128)
			}
		}
	})
	return dst
}

func clamp8(value int) uint8 {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return uint8(value)
}

// NV12ToRGBA converts the top-left width x height of an NV12 picture with
// the given luma stride and plane height into a new RGBA image.
func NV12ToRGBA(data []byte, stride, planeHeight, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	chroma := data[stride*planeHeight:]
	parallelRows(height, func(start, end int) {
		for y := start; y < end; y++ {
			lrow := data[y*stride:]
			crow := chroma[(y/2)*stride:]
			offset := out.PixOffset(0, y)
			for x := 0; x < width; x++ {
				c := 298 * (int(lrow[x]) - 16)
				d := int(crow[x&^1]) - 128
				e := int(crow[x|1]) - 128
				out.Pix[offset] = clamp8((c + 409*e + 128) >> 8)
				out.Pix[offset+1] = clamp8((c - 100*d - 208*e + 128) >> 8)
				out.Pix[offset+2] = clamp8((c + 516*d + 128) >> 8)
				out.Pix[offset+3] = 255
				offset += 4
			}
		}
	})
	return out
}
