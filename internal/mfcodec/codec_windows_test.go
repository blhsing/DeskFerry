//go:build windows

package mfcodec

import (
	"image"
	"math"
	"testing"
	"time"
)

func movingPicture(width, height, frame int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := img.PixOffset(x, y)
			dx, dy := float64(x-frame*7), float64(y-frame*3)
			img.Pix[offset] = uint8(128 + 120*math.Sin(dx/23))
			img.Pix[offset+1] = uint8(128 + 120*math.Sin(dy/17))
			img.Pix[offset+2] = uint8(128 + 120*math.Sin((dx+dy)/31))
			img.Pix[offset+3] = 255
		}
	}
	return img
}

func psnr(a, b *image.RGBA, width, height int) float64 {
	var sum float64
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pa, pb := a.PixOffset(x, y), b.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				d := float64(a.Pix[pa+c]) - float64(b.Pix[pb+c])
				sum += d * d
			}
		}
	}
	mse := sum / float64(width*height*3)
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

func roundTrip(t *testing.T, softwareOnly bool) {
	const width, height = 1000, 600 // not multiples of 16, like real rects
	alignedWidth, alignedHeight := AlignedSize(width, height)
	encoder, err := NewEncoder(EncoderOptions{Width: alignedWidth, Height: alignedHeight, FPS: 30, Bitrate: 6_000_000, SoftwareOnly: softwareOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	decoder, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	t.Logf("encoder: %s (hardware=%t)", encoder.Name(), encoder.Hardware())
	var encodeTime, decodeTime time.Duration
	decoded := 0
	bytes := 0
	var last *image.RGBA
	var lastSource *image.RGBA
	sources := map[int]*image.RGBA{}
	for frame := 0; frame < 60; frame++ {
		source := movingPicture(width, height, frame)
		sources[frame] = source
		started := time.Now()
		data, err := encoder.Encode(source, source.Bounds(), frame == 0)
		encodeTime += time.Since(started)
		if err != nil {
			t.Fatalf("encode frame %d: %v", frame, err)
		}
		bytes += len(data)
		if len(data) == 0 {
			continue
		}
		started = time.Now()
		picture, err := decoder.Decode(data, width, height)
		decodeTime += time.Since(started)
		if err != nil {
			t.Fatalf("decode frame %d: %v", frame, err)
		}
		if picture != nil {
			decoded++
			last = picture
			lastSource = source
		}
	}
	if decoded < 50 || last == nil {
		t.Fatalf("decoded only %d of 60 frames", decoded)
	}
	if last.Bounds().Dx() != width || last.Bounds().Dy() != height {
		t.Fatalf("decoded size %v", last.Bounds())
	}
	quality := psnr(last, lastSource, width, height)
	t.Logf("%d frames decoded, %d KiB total (%.1f Mbit/s at 30 fps), encode %.1f ms/frame, decode %.1f ms/frame, PSNR of the last frame %.1f dB",
		decoded, bytes>>10, float64(bytes)*8*30/60/1e6, float64(encodeTime.Milliseconds())/60, float64(decodeTime.Milliseconds())/float64(decoded), quality)
	if quality < 28 {
		t.Fatalf("PSNR %.1f dB is too low; the decoded picture does not match", quality)
	}
}

func TestHardwareOrDefaultRoundTrip(t *testing.T) { roundTrip(t, false) }

func TestSoftwareRoundTrip(t *testing.T) { roundTrip(t, true) }
