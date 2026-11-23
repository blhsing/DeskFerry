package screenview

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"runtime"
	"sync"
)

// LiveTileSize is the change-detection tile size of the live session.
const LiveTileSize = 64

const (
	// Tiles with more distinct colors than this are treated as photographic
	// and may be sent as JPEG; others are text-like and always lossless.
	photoColorThreshold = 1024
	maxStripTiles       = 16
)

type tileClass uint8

const (
	classSolid tileClass = iota
	classText
	classPhoto
)

type pngBufferPool struct{ pool sync.Pool }

func (p *pngBufferPool) Get() *png.EncoderBuffer {
	if value, ok := p.pool.Get().(*png.EncoderBuffer); ok {
		return value
	}
	return nil
}

func (p *pngBufferPool) Put(buffer *png.EncoderBuffer) { p.pool.Put(buffer) }

var livePNG = png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: &pngBufferPool{}}

// colorSet is a small open-addressing set of RGBA values.
type colorSet struct {
	keys  [4096]uint32
	used  [4096]bool
	index [4096]uint16
	count int
	order []uint32
}

func (s *colorSet) reset() {
	for i := range s.used {
		s.used[i] = false
	}
	s.count = 0
	s.order = s.order[:0]
}

// add inserts value and reports false when the set already holds limit values
// and value is new.
func (s *colorSet) add(value uint32, limit int) bool {
	slot := (value * 2654435761) >> 20
	for {
		if !s.used[slot] {
			if s.count >= limit {
				return false
			}
			s.used[slot] = true
			s.keys[slot] = value
			s.index[slot] = uint16(s.count)
			s.order = append(s.order, value)
			s.count++
			return true
		}
		if s.keys[slot] == value {
			return true
		}
		slot = (slot + 1) & 4095
	}
}

func (s *colorSet) lookup(value uint32) uint16 {
	slot := (value * 2654435761) >> 20
	for s.keys[slot] != value || !s.used[slot] {
		slot = (slot + 1) & 4095
	}
	return s.index[slot]
}

var colorSetPool = sync.Pool{New: func() any { return &colorSet{} }}

func pixelAt(img *image.RGBA, offset int) uint32 {
	pix := img.Pix[offset : offset+4 : offset+4]
	return uint32(pix[0]) | uint32(pix[1])<<8 | uint32(pix[2])<<16 | 0xff000000
}

// countColors counts distinct colors in rect up to limit+1.
func countColors(img *image.RGBA, rect image.Rectangle, set *colorSet, limit int) int {
	set.reset()
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		offset := img.PixOffset(rect.Min.X, y)
		var last uint32
		haveLast := false
		for x := rect.Min.X; x < rect.Max.X; x++ {
			value := pixelAt(img, offset)
			offset += 4
			if haveLast && value == last {
				continue
			}
			last, haveLast = value, true
			if !set.add(value, limit) {
				return limit + 1
			}
		}
	}
	return set.count
}

func classifyTile(img *image.RGBA, rect image.Rectangle, set *colorSet) (tileClass, uint32) {
	count := countColors(img, rect, set, photoColorThreshold)
	switch {
	case count == 1:
		return classSolid, set.order[0]
	case count > photoColorThreshold:
		return classPhoto, 0
	default:
		return classText, 0
	}
}

func encodeSolid(value uint32) []byte {
	return []byte{byte(value), byte(value >> 8), byte(value >> 16), 255}
}

// encodeLossless encodes rect as PNG, using a palette when it has at most 256
// colors.
func encodeLossless(img *image.RGBA, rect image.Rectangle, set *colorSet) ([]byte, error) {
	var output bytes.Buffer
	if count := countColors(img, rect, set, 256); count <= 256 {
		palette := make(color.Palette, count)
		for index, value := range set.order {
			palette[index] = color.RGBA{R: byte(value), G: byte(value >> 8), B: byte(value >> 16), A: 255}
		}
		paletted := image.NewPaletted(image.Rect(0, 0, rect.Dx(), rect.Dy()), palette)
		for y := 0; y < rect.Dy(); y++ {
			offset := img.PixOffset(rect.Min.X, rect.Min.Y+y)
			row := paletted.Pix[y*paletted.Stride : y*paletted.Stride+rect.Dx()]
			for x := range row {
				row[x] = byte(set.lookup(pixelAt(img, offset)))
				offset += 4
			}
		}
		if err := livePNG.Encode(&output, paletted); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	sub := opaqueCopy(img, rect)
	if err := livePNG.Encode(&output, sub); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// opaqueCopy copies rect into an image with alpha forced to 255, which lets
// the PNG encoder write RGB instead of RGBA.
func opaqueCopy(img *image.RGBA, rect image.Rectangle) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	for y := 0; y < rect.Dy(); y++ {
		src := img.Pix[img.PixOffset(rect.Min.X, rect.Min.Y+y):]
		dst := out.Pix[y*out.Stride : y*out.Stride+rect.Dx()*4]
		copy(dst, src[:len(dst)])
		for x := 3; x < len(dst); x += 4 {
			dst[x] = 255
		}
	}
	return out
}

func encodeJPEG(img *image.RGBA, rect image.Rectangle, quality int) ([]byte, error) {
	var output bytes.Buffer
	sub := img.SubImage(rect)
	if err := jpeg.Encode(&output, sub, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// tileJob is one strip of horizontally adjacent tiles of one class.
type tileJob struct {
	rect  image.Rectangle
	class tileClass
	solid uint32
	lossy bool
	tiles []int
	out   EncodedRect
	err   error
}

// tileGrid tracks per-tile change and quality state.
type tileGrid struct {
	width, height int
	cols, rows    int
	dirty         []bool
	lossy         []bool
}

func newTileGrid(width, height int) *tileGrid {
	cols := (width + LiveTileSize - 1) / LiveTileSize
	rows := (height + LiveTileSize - 1) / LiveTileSize
	return &tileGrid{width: width, height: height, cols: cols, rows: rows, dirty: make([]bool, cols*rows), lossy: make([]bool, cols*rows)}
}

func (g *tileGrid) tileRect(index int) image.Rectangle {
	col, row := index%g.cols, index/g.cols
	x, y := col*LiveTileSize, row*LiveTileSize
	return image.Rect(x, y, min(x+LiveTileSize, g.width), min(y+LiveTileSize, g.height))
}

func (g *tileGrid) markAll() {
	for i := range g.dirty {
		g.dirty[i] = true
	}
}

func (g *tileGrid) anyDirty() bool {
	for _, value := range g.dirty {
		if value {
			return true
		}
	}
	return false
}

func (g *tileGrid) anyLossy() bool {
	for _, value := range g.lossy {
		if value {
			return true
		}
	}
	return false
}

// diffInto compares current against reference tile by tile, copies changed
// tiles into reference, marks them dirty and returns the changed tile count.
func (g *tileGrid) diffInto(reference, current *image.RGBA) int {
	changed := 0
	for index := range g.dirty {
		rect := g.tileRect(index)
		if rgbaRegionEqual(reference, current, rect) {
			continue
		}
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			start := reference.PixOffset(rect.Min.X, y)
			copy(reference.Pix[start:start+rect.Dx()*4], current.Pix[current.PixOffset(rect.Min.X, y):])
		}
		g.dirty[index] = true
		changed++
	}
	return changed
}

// dirtyRects merges dirty tiles into horizontal runs for reporting.
func (g *tileGrid) dirtyRects() []image.Rectangle {
	var rects []image.Rectangle
	for row := 0; row < g.rows; row++ {
		start := -1
		for col := 0; col <= g.cols; col++ {
			dirty := col < g.cols && g.dirty[row*g.cols+col]
			if dirty && start < 0 {
				start = col
			}
			if !dirty && start >= 0 {
				rects = append(rects, g.tileRect(row*g.cols+start).Union(g.tileRect(row*g.cols+col-1)))
				start = -1
			}
		}
	}
	return rects
}

// encodeTiles encodes the selected tiles of img. When lossless is false,
// photographic tiles are sent as JPEG at quality and marked lossy. It returns
// the encoded rects in raster order and clears the encoded tiles' dirty flags.
func (g *tileGrid) encodeTiles(img *image.RGBA, selected func(int) bool, lossless bool, quality int) ([]EncodedRect, int, error) {
	set := colorSetPool.Get().(*colorSet)
	var jobs []*tileJob
	lossyCount := 0
	for row := 0; row < g.rows; row++ {
		var current *tileJob
		for col := 0; col < g.cols; col++ {
			index := row*g.cols + col
			if !selected(index) {
				current = nil
				continue
			}
			rect := g.tileRect(index)
			class, solid := classifyTile(img, rect, set)
			lossy := class == classPhoto && !lossless
			extend := current != nil && current.class == class && len(current.tiles) < maxStripTiles &&
				(class != classSolid || current.solid == solid) && current.lossy == lossy
			if extend {
				current.rect = current.rect.Union(rect)
				current.tiles = append(current.tiles, index)
				continue
			}
			current = &tileJob{rect: rect, class: class, solid: solid, lossy: lossy, tiles: []int{index}}
			jobs = append(jobs, current)
		}
	}
	colorSetPool.Put(set)

	workers := max(1, min(runtime.NumCPU(), len(jobs)))
	var wg sync.WaitGroup
	next := make(chan *tileJob)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localSet := colorSetPool.Get().(*colorSet)
			defer colorSetPool.Put(localSet)
			for job := range next {
				job.out = EncodedRect{X: job.rect.Min.X, Y: job.rect.Min.Y, W: job.rect.Dx(), H: job.rect.Dy()}
				switch {
				case job.class == classSolid:
					job.out.Enc, job.out.Data = EncSolid, encodeSolid(job.solid)
				case job.lossy:
					job.out.Enc = EncJPEG
					job.out.Data, job.err = encodeJPEG(img, job.rect, quality)
				default:
					job.out.Enc = EncPNG
					job.out.Data, job.err = encodeLossless(img, job.rect, localSet)
				}
			}
		}()
	}
	for _, job := range jobs {
		next <- job
	}
	close(next)
	wg.Wait()

	rects := make([]EncodedRect, 0, len(jobs))
	for _, job := range jobs {
		if job.err != nil {
			return nil, 0, job.err
		}
		rects = append(rects, job.out)
		for _, index := range job.tiles {
			g.dirty[index] = false
			g.lossy[index] = job.lossy
			if job.lossy {
				lossyCount++
			}
		}
	}
	return rects, lossyCount, nil
}
