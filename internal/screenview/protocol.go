package screenview

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
)

const (
	ModeSingle = "single"
	ModeStream = "stream"

	FrameFull  = "full"
	FrameDelta = "delta"
	FrameError = "error"

	DefaultIntervalMS = 1000
	DefaultTileSize   = 64
	MinIntervalMS     = 250
	MaxIntervalMS     = 60000

	maxMetadataSize = 1 << 20
	maxPayloadSize  = 256 << 20
)

// Request is sent by a Home agent after the relay pairs its screen session.
type Request struct {
	Mode       string `json:"mode"`
	IntervalMS int    `json:"interval_ms,omitempty"`
	TileSize   int    `json:"tile_size,omitempty"`
}

func (r *Request) Normalize() error {
	if r.Mode == "" {
		r.Mode = ModeSingle
	}
	if r.Mode != ModeSingle && r.Mode != ModeStream {
		return fmt.Errorf("unsupported screen mode %q", r.Mode)
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = DefaultIntervalMS
	}
	if r.IntervalMS < MinIntervalMS || r.IntervalMS > MaxIntervalMS {
		return fmt.Errorf("screen interval must be between %d and %d milliseconds", MinIntervalMS, MaxIntervalMS)
	}
	if r.TileSize == 0 {
		r.TileSize = DefaultTileSize
	}
	if r.TileSize < 16 || r.TileSize > 512 {
		return errors.New("screen tile size must be between 16 and 512 pixels")
	}
	return nil
}

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
	Length int `json:"length"`
}

// Frame is the JSON metadata that precedes zero or more PNG rectangle payloads.
type Frame struct {
	Type   string `json:"type"`
	Seq    uint64 `json:"seq,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Rects  []Rect `json:"rects,omitempty"`
	Error  string `json:"error,omitempty"`
}

func WriteRequest(w io.Writer, request Request) error {
	if err := request.Normalize(); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(request)
}

func ReadRequest(r io.Reader) (Request, error) {
	var request Request
	decoder := json.NewDecoder(io.LimitReader(r, maxMetadataSize))
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode screen request: %w", err)
	}
	if err := request.Normalize(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func WriteFrame(w io.Writer, frame Frame, payloads [][]byte) error {
	if len(frame.Rects) != len(payloads) {
		return errors.New("screen frame rectangle and payload counts differ")
	}
	for index := range frame.Rects {
		frame.Rects[index].Length = len(payloads[index])
	}
	metadata, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode screen frame metadata: %w", err)
	}
	if len(metadata) > maxMetadataSize {
		return errors.New("screen frame metadata is too large")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(metadata)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	if _, err := w.Write(metadata); err != nil {
		return err
	}
	for _, payload := range payloads {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func ReadFrame(r io.Reader) (Frame, [][]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return Frame{}, nil, err
	}
	metadataSize := int(binary.BigEndian.Uint32(size[:]))
	if metadataSize <= 0 || metadataSize > maxMetadataSize {
		return Frame{}, nil, fmt.Errorf("invalid screen frame metadata size %d", metadataSize)
	}
	metadata := make([]byte, metadataSize)
	if _, err := io.ReadFull(r, metadata); err != nil {
		return Frame{}, nil, err
	}
	var frame Frame
	if err := json.Unmarshal(metadata, &frame); err != nil {
		return Frame{}, nil, fmt.Errorf("decode screen frame metadata: %w", err)
	}
	payloads := make([][]byte, len(frame.Rects))
	total := 0
	for index, rect := range frame.Rects {
		if rect.X < 0 || rect.Y < 0 || rect.Width <= 0 || rect.Height <= 0 || rect.Length <= 0 {
			return Frame{}, nil, errors.New("screen frame contains an invalid rectangle")
		}
		total += rect.Length
		if total > maxPayloadSize {
			return Frame{}, nil, errors.New("screen frame payload is too large")
		}
		payloads[index] = make([]byte, rect.Length)
		if _, err := io.ReadFull(r, payloads[index]); err != nil {
			return Frame{}, nil, err
		}
	}
	return frame, payloads, nil
}

func EncodeFull(seq uint64, source image.Image) (Frame, [][]byte, error) {
	current := cloneRGBA(source)
	payload, err := encodePNG(current)
	if err != nil {
		return Frame{}, nil, err
	}
	bounds := current.Bounds()
	frame := Frame{Type: FrameFull, Seq: seq, Width: bounds.Dx(), Height: bounds.Dy(), Rects: []Rect{{Width: bounds.Dx(), Height: bounds.Dy()}}}
	return frame, [][]byte{payload}, nil
}

// EncodeDelta compares fixed-size tiles and encodes only tiles whose pixels
// changed. It never falls back to a full frame, even when the whole image has
// changed, so streaming mode has a strict delta-only contract after frame one.
func EncodeDelta(seq uint64, previous, current image.Image, tileSize int) (Frame, [][]byte, error) {
	if tileSize <= 0 {
		tileSize = DefaultTileSize
	}
	prev := cloneRGBA(previous)
	next := cloneRGBA(current)
	bounds := next.Bounds()
	frame := Frame{Type: FrameDelta, Seq: seq, Width: bounds.Dx(), Height: bounds.Dy()}
	if prev.Bounds().Size() != bounds.Size() {
		return Frame{}, nil, errors.New("screen dimensions changed; start a new stream")
	}
	var payloads [][]byte
	for y := 0; y < bounds.Dy(); y += tileSize {
		for x := 0; x < bounds.Dx(); x += tileSize {
			width := min(tileSize, bounds.Dx()-x)
			height := min(tileSize, bounds.Dy()-y)
			rect := image.Rect(x, y, x+width, y+height)
			if rgbaRegionEqual(prev, next, rect) {
				continue
			}
			tile := image.NewRGBA(image.Rect(0, 0, width, height))
			draw.Draw(tile, tile.Bounds(), next, image.Pt(x, y), draw.Src)
			payload, err := encodePNG(tile)
			if err != nil {
				return Frame{}, nil, err
			}
			frame.Rects = append(frame.Rects, Rect{X: x, Y: y, Width: width, Height: height})
			payloads = append(payloads, payload)
		}
	}
	return frame, payloads, nil
}

// ApplyFrame reconstructs full images from the initial frame and later deltas.
func ApplyFrame(base *image.RGBA, frame Frame, payloads [][]byte) (*image.RGBA, error) {
	if frame.Type == FrameError {
		return base, errors.New(frame.Error)
	}
	if frame.Type != FrameFull && frame.Type != FrameDelta {
		return base, fmt.Errorf("unsupported screen frame type %q", frame.Type)
	}
	if frame.Width <= 0 || frame.Height <= 0 || len(frame.Rects) != len(payloads) {
		return base, errors.New("invalid screen frame")
	}
	if frame.Type == FrameFull || base == nil || base.Bounds().Dx() != frame.Width || base.Bounds().Dy() != frame.Height {
		if frame.Type != FrameFull {
			return base, errors.New("received a screen delta before a matching full frame")
		}
		base = image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	}
	for index, rect := range frame.Rects {
		decoded, err := png.Decode(bytes.NewReader(payloads[index]))
		if err != nil {
			return base, fmt.Errorf("decode screen rectangle: %w", err)
		}
		if decoded.Bounds().Dx() != rect.Width || decoded.Bounds().Dy() != rect.Height || rect.X+rect.Width > frame.Width || rect.Y+rect.Height > frame.Height {
			return base, errors.New("screen rectangle dimensions do not match its metadata")
		}
		draw.Draw(base, image.Rect(rect.X, rect.Y, rect.X+rect.Width, rect.Y+rect.Height), decoded, decoded.Bounds().Min, draw.Src)
	}
	return base, nil
}

func cloneRGBA(source image.Image) *image.RGBA {
	bounds := source.Bounds()
	result := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(result, result.Bounds(), source, bounds.Min, draw.Src)
	return result
}

func rgbaRegionEqual(left, right *image.RGBA, rect image.Rectangle) bool {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		leftStart := left.PixOffset(rect.Min.X, y)
		rightStart := right.PixOffset(rect.Min.X, y)
		length := rect.Dx() * 4
		if !bytes.Equal(left.Pix[leftStart:leftStart+length], right.Pix[rightStart:rightStart+length]) {
			return false
		}
	}
	return true
}

func encodePNG(source image.Image) ([]byte, error) {
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		return nil, fmt.Errorf("encode screen PNG: %w", err)
	}
	return output.Bytes(), nil
}
