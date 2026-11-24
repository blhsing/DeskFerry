package screenview

import "image"

// cursorFromBlackWhite rebuilds a pointer image with straight alpha from two
// renderings of it, one over black and one over white (BGRA rows, width x
// height). This covers alpha, masked and monochrome cursors alike: a pixel
// that shows the background through it lets white through, and one that
// inverts the background turns black into white and white into black.
//
// Inverting pixels cannot be reproduced by an overlay drawn over the
// picture; they become dark gray, which stays visible on light backgrounds,
// and the light parts of an inverting cursor are drawn on dark ones.
func cursorFromBlackWhite(black, white []byte, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i+3 < len(black) && i+3 < len(white) && i+3 < len(out.Pix); i += 4 {
		var alphaSum, inverted int
		for channel := 0; channel < 3; channel++ {
			b, w := int(black[i+channel]), int(white[i+channel])
			if w < b {
				inverted++
				continue
			}
			alphaSum += 255 - (w - b)
		}
		if inverted >= 2 {
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = 32, 32, 32, 255
			continue
		}
		alpha := alphaSum / max(1, 3-inverted)
		if alpha <= 0 {
			continue
		}
		for channel := 0; channel < 3; channel++ {
			// Over black the pixel shows color*alpha; undo the
			// premultiplication. RGBA order is the reverse of BGRA.
			value := int(black[i+channel]) * 255 / alpha
			out.Pix[i+2-channel] = byte(min(255, value))
		}
		out.Pix[i+3] = byte(alpha)
	}
	return out
}
