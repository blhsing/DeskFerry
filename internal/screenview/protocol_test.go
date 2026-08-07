package screenview

import (
	"bytes"
	"image"
	"image/color"
	"reflect"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	want := Request{Mode: ModeStream, IntervalMS: 750, TileSize: 32}
	if err := WriteRequest(&wire, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	frame := Frame{Type: FrameDelta, Seq: 3, Width: 128, Height: 64, Rects: []Rect{{X: 64, Width: 64, Height: 64}}}
	payloads := [][]byte{[]byte("png-data")}
	var wire bytes.Buffer
	if err := WriteFrame(&wire, frame, payloads); err != nil {
		t.Fatal(err)
	}
	gotFrame, gotPayloads, err := ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if gotFrame.Rects[0].Length != len(payloads[0]) || !bytes.Equal(gotPayloads[0], payloads[0]) {
		t.Fatalf("unexpected frame %#v payload %q", gotFrame, gotPayloads[0])
	}
}

func TestDeltaContainsOnlyChangedTileAndReconstructs(t *testing.T) {
	previous := image.NewRGBA(image.Rect(0, 0, 128, 128))
	current := image.NewRGBA(previous.Bounds())
	for index := range current.Pix {
		current.Pix[index] = previous.Pix[index]
	}
	for y := 70; y < 80; y++ {
		for x := 70; x < 80; x++ {
			current.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	frame, payloads, err := EncodeDelta(2, previous, current, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Rects) != 1 || frame.Rects[0].X != 64 || frame.Rects[0].Y != 64 {
		t.Fatalf("expected only the changed tile, got %#v", frame.Rects)
	}
	base := cloneRGBA(previous)
	base, err = ApplyFrame(base, frame, payloads)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(base.Pix, current.Pix) {
		t.Fatal("delta reconstruction does not match current image")
	}
}

func TestUnchangedDeltaHasNoPayload(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	frame, payloads, err := EncodeDelta(2, img, img, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Rects) != 0 || len(payloads) != 0 {
		t.Fatalf("unchanged frame sent data: %#v", frame)
	}
}
