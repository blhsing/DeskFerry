//go:build windows

package sharebanner

import (
	"math"

	"github.com/lxn/walk"
)

// surface is a top-down image of premultiplied BGRA pixels, the format a
// layered window takes. Shapes are anti-aliased from their exact distance to
// the edge, so rounded corners stay smooth at every DPI.
type surface struct {
	width  int
	height int
	pix    []byte
}

// roundRectDistance is the signed distance from (px, py) to the edge of a
// rounded rectangle; it is negative inside.
func roundRectDistance(px, py, x, y, w, h, radius float64) float64 {
	radius = math.Max(0, math.Min(radius, math.Min(w, h)/2))
	qx := math.Abs(px-(x+w/2)) - (w/2 - radius)
	qy := math.Abs(py-(y+h/2)) - (h/2 - radius)
	outside := math.Sqrt(math.Max(qx, 0)*math.Max(qx, 0) + math.Max(qy, 0)*math.Max(qy, 0))
	return math.Min(math.Max(qx, qy), 0) + outside - radius
}

// span clamps a floating-point range to pixel indices within limit.
func span(from, to float64, limit int) (int, int) {
	return max(0, int(math.Floor(from))), min(limit, int(math.Ceil(to)))
}

func (s *surface) blend(index int, color walk.Color, alpha float64) {
	if alpha <= 0 {
		return
	}
	alpha = math.Min(alpha, 1)
	keep := 1 - alpha
	pixel := s.pix[index : index+4 : index+4]
	pixel[0] = byte(float64(color.B())*alpha + float64(pixel[0])*keep + 0.5)
	pixel[1] = byte(float64(color.G())*alpha + float64(pixel[1])*keep + 0.5)
	pixel[2] = byte(float64(color.R())*alpha + float64(pixel[2])*keep + 0.5)
	pixel[3] = byte(255*alpha + float64(pixel[3])*keep + 0.5)
}

// fillRoundRect paints an anti-aliased rounded rectangle. A radius of half
// the smaller side gives a pill or a circle.
func (s *surface) fillRoundRect(x, y, w, h, radius float64, color walk.Color) {
	if w <= 0 || h <= 0 {
		return
	}
	left, right := span(x, x+w, s.width)
	top, bottom := span(y, y+h, s.height)
	// Pixels at least one pixel inside the straight edges and clear of the
	// corner arcs are fully covered and need no distance calculation.
	radius = math.Max(0, math.Min(radius, math.Min(w, h)/2))
	innerLeft, innerRight := int(math.Ceil(x+1)), int(math.Floor(x+w-1))
	innerTop, innerBottom := int(math.Ceil(y+1)), int(math.Floor(y+h-1))
	arcLeft, arcRight := int(math.Ceil(x+radius)), int(math.Floor(x+w-radius))
	arcTop, arcBottom := int(math.Ceil(y+radius)), int(math.Floor(y+h-radius))
	blue, green, red := color.B(), color.G(), color.R()
	for row := top; row < bottom; row++ {
		rowInside := row >= innerTop && row < innerBottom
		rowClear := row >= arcTop && row < arcBottom
		for column := left; column < right; column++ {
			index := (row*s.width + column) * 4
			if rowInside && column >= arcLeft && column < arcRight || rowClear && column >= innerLeft && column < innerRight {
				s.pix[index], s.pix[index+1], s.pix[index+2], s.pix[index+3] = blue, green, red, 255
				continue
			}
			distance := roundRectDistance(float64(column)+0.5, float64(row)+0.5, x, y, w, h, radius)
			s.blend(index, color, 0.5-distance)
		}
	}
}

// alphaChannel returns a copy of the alpha values.
func (s *surface) alphaChannel() []byte {
	alpha := make([]byte, s.width*s.height)
	for i := range alpha {
		alpha[i] = s.pix[i*4+3]
	}
	return alpha
}

// setAlphaChannel restores alpha values saved by alphaChannel. GDI text and
// icon drawing leave the alpha byte undefined, so it is put back afterwards.
func (s *surface) setAlphaChannel(alpha []byte) {
	for i, value := range alpha {
		s.pix[i*4+3] = value
	}
}
