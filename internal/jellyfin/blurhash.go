package jellyfin

import (
	"image"
	"io"
	"math"
	"strings"

	// the kinds of image a cover may be, which image.Decode tells apart
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// A blurhash is the few dozen characters that a client draws a blurred
// stand-in of an image from while the image loads. Jellyfin computes one for
// every image, and Finamp keeps the images it downloads by it: an item whose
// image has none is downloaded with an image of its own, and Finamp warns
// that the server is set up wrong. The encoding is the one of
// github.com/woltapp/blurhash.

// decodeCover reads an image of a kind a cover may be.
func decodeCover(r io.Reader) (image.Image, error) {
	img, _, err := image.Decode(r)
	return img, err
}

// blurCells is the most cells a side of an image is shrunk to before it is
// encoded, and blurSamples how many pixels a side of a cell is averaged
// from: a blurhash has no more detail than that, and the cosines of every
// pixel of a large cover take long.
const (
	blurCells   = 32
	blurSamples = 4
)

// blurHash is the blurhash of an image: four components across and four
// down, or three along the short side of an image clearly wider than it is
// tall or taller than it is wide.
func blurHash(img image.Image) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return ""
	}
	xComp, yComp := 4, 4
	switch {
	case 2*w > 3*h:
		yComp = 3
	case 2*h > 3*w:
		xComp = 3
	}
	cw, ch := min(w, blurCells), min(h, blurCells)
	if w > h {
		ch = max(1, min(h, cw*h/w))
	} else if h > w {
		cw = max(1, min(w, ch*w/h))
	}
	return encodeBlurHash(xComp, yComp, cw, ch, shrink(img, cw, ch))
}

// shrink averages an image into cw by ch cells of linear light, row by row,
// each of red, green and blue.
func shrink(img image.Image, cw, ch int) [][3]float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	sx, sy := min(blurSamples, max(1, w/cw)), min(blurSamples, max(1, h/ch))
	cells := make([][3]float64, cw*ch)
	for cy := range ch {
		for cx := range cw {
			var sum [3]float64
			for j := range sy {
				y := b.Min.Y + (cy*sy+j)*h/(ch*sy)
				for i := range sx {
					x := b.Min.X + (cx*sx+i)*w/(cw*sx)
					r, g, bl, _ := img.At(x, y).RGBA()
					sum[0] += srgbToLinear[r>>8]
					sum[1] += srgbToLinear[g>>8]
					sum[2] += srgbToLinear[bl>>8]
				}
			}
			n := float64(sx * sy)
			cells[cy*cw+cx] = [3]float64{sum[0] / n, sum[1] / n, sum[2] / n}
		}
	}
	return cells
}

// encodeBlurHash encodes the pixels of linear light of a w by h image, row
// by row, with xComp by yComp components, as the reference implementation
// does.
func encodeBlurHash(xComp, yComp, w, h int, pixels [][3]float64) string {
	factors := make([][3]float64, 0, xComp*yComp)
	for y := range yComp {
		for x := range xComp {
			norm := 2.0
			if x == 0 && y == 0 {
				norm = 1
			}
			var f [3]float64
			for j := range h {
				cy := math.Cos(math.Pi * float64(y) * float64(j) / float64(h))
				for i := range w {
					basis := norm * math.Cos(math.Pi*float64(x)*float64(i)/float64(w)) * cy
					p := pixels[j*w+i]
					f[0] += basis * p[0]
					f[1] += basis * p[1]
					f[2] += basis * p[2]
				}
			}
			scale := 1 / float64(w*h)
			factors = append(factors, [3]float64{f[0] * scale, f[1] * scale, f[2] * scale})
		}
	}

	var hash strings.Builder
	encode83(&hash, (xComp-1)+(yComp-1)*9, 1)
	dc, ac := factors[0], factors[1:]
	maxValue := 1.0
	if len(ac) > 0 {
		actualMax := 0.0
		for _, f := range ac {
			actualMax = max(actualMax, math.Abs(f[0]), math.Abs(f[1]), math.Abs(f[2]))
		}
		quantised := int(max(0, min(82, math.Floor(actualMax*166-0.5))))
		maxValue = float64(quantised+1) / 166
		encode83(&hash, quantised, 1)
	} else {
		encode83(&hash, 0, 1)
	}
	encode83(&hash, linearToSRGB(dc[0])<<16+linearToSRGB(dc[1])<<8+linearToSRGB(dc[2]), 4)
	for _, f := range ac {
		q := func(v float64) int {
			return int(max(0, min(18, math.Floor(signPow(v/maxValue, 0.5)*9+9.5))))
		}
		encode83(&hash, q(f[0])*19*19+q(f[1])*19+q(f[2]), 2)
	}
	return hash.String()
}

const base83 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~"

func encode83(b *strings.Builder, n, length int) {
	for i := 1; i <= length; i++ {
		digit := n / int(math.Pow(83, float64(length-i))) % 83
		b.WriteByte(base83[digit])
	}
}

// srgbToLinear is the linear light of each value of an sRGB channel.
var srgbToLinear = func() (t [256]float64) {
	for i := range t {
		v := float64(i) / 255
		if v <= 0.04045 {
			t[i] = v / 12.92
		} else {
			t[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return t
}()

func linearToSRGB(v float64) int {
	v = max(0, min(1, v))
	if v <= 0.0031308 {
		return int(v*12.92*255 + 0.5)
	}
	return int((1.055*math.Pow(v, 1/2.4)-0.055)*255 + 0.5)
}

func signPow(v, exp float64) float64 {
	return math.Copysign(math.Pow(math.Abs(v), exp), v)
}
