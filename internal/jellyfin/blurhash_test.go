package jellyfin

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/audio"
)

func solid(w, h int, c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

// The hashes of a pixel follow from the encoding by hand: every cosine is 1,
// so the DC component is the pixel and each of the 15 AC components twice
// it. 4 by 4 components are size flag 3+3*9 = 30, "U".
func TestBlurHashOfAPixel(t *testing.T) {
	// black: no AC at all, "0"; DC 0, "0000"; an AC of 0 quantises to
	// 9*19*19+9*19+9 = 3429 = 41*83+26, "fQ"
	if got, want := blurHash(solid(1, 1, color.Black)), "U00000"+strings.Repeat("fQ", 15); got != want {
		t.Errorf("black: %q, want %q", got, want)
	}
	// white: the largest AC is 2, quantised to the top, 82, "~", for a
	// maximum of 0.5; DC 0xFFFFFF = 29*83³+28*83²+30*83+10, "TSUA"; an AC of 2
	// is sqrt(4)*9+9.5, clamped to 18: 18*361+18*19+18 = 6858 = 82*83+52, "~q"
	if got, want := blurHash(solid(1, 1, color.White)), "U~TSUA"+strings.Repeat("~q", 15); got != want {
		t.Errorf("white: %q, want %q", got, want)
	}
}

// decode83 and blurPixel decode a hash as the reference decoder does, to see
// that the components are encoded in their order.
func decode83(s string) int {
	n := 0
	for _, c := range []byte(s) {
		n = n*83 + strings.IndexByte(base83, c)
	}
	return n
}

func blurPixel(hash string, fx, fy float64) [3]float64 {
	size := decode83(hash[:1])
	xComp, yComp := size%9+1, size/9+1
	maxValue := float64(decode83(hash[1:2])+1) / 166
	dc := decode83(hash[2:6])
	colours := [][3]float64{{srgbToLinear[dc>>16], srgbToLinear[dc>>8&255], srgbToLinear[dc&255]}}
	for i := 1; i < xComp*yComp; i++ {
		v := decode83(hash[4+2*i : 6+2*i])
		ac := func(q int) float64 { return signPow(float64(q-9)/9, 2) * maxValue }
		colours = append(colours, [3]float64{ac(v / 361), ac(v / 19 % 19), ac(v % 19)})
	}
	var p [3]float64
	for j := range yComp {
		for i := range xComp {
			basis := math.Cos(math.Pi*fx*float64(i)) * math.Cos(math.Pi*fy*float64(j))
			c := colours[j*xComp+i]
			p[0] += c[0] * basis
			p[1] += c[1] * basis
			p[2] += c[2] * basis
		}
	}
	return p
}

func TestBlurHashKeepsTheLayout(t *testing.T) {
	// red grows to the right and blue downwards; a large image is shrunk
	img := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	for y := range 300 {
		for x := range 300 {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / 299), 64, uint8(y * 255 / 299), 255})
		}
	}
	hash := blurHash(img)
	if len(hash) != 36 || hash[0] != 'U' {
		t.Fatalf("hash %q", hash)
	}
	topLeft, topRight := blurPixel(hash, 0.05, 0.05), blurPixel(hash, 0.95, 0.05)
	bottomLeft := blurPixel(hash, 0.05, 0.95)
	if topRight[0] < topLeft[0]+0.3 || bottomLeft[2] < topLeft[2]+0.3 {
		t.Errorf("hash %q: top left %v, top right %v, bottom left %v", hash, topLeft, topRight, bottomLeft)
	}
	if green := blurPixel(hash, 0.5, 0.5)[1]; math.Abs(green-srgbToLinear[64]) > 0.03 {
		t.Errorf("the green is %v, not %v", green, srgbToLinear[64])
	}
}

func TestBlurHashOfWideAndTallImages(t *testing.T) {
	// size flags 3+2*9 = 21, "L", and 2+3*9 = 29, "T", each with 12 components
	for _, c := range []struct {
		w, h int
		flag byte
	}{{600, 200, 'L'}, {200, 600, 'T'}, {200, 210, 'U'}} {
		hash := blurHash(solid(c.w, c.h, color.Gray{128}))
		size := decode83(hash[:1])
		if components := (size%9 + 1) * (size/9 + 1); hash[0] != c.flag || len(hash) != 4+2*components {
			t.Errorf("%dx%d: %q", c.w, c.h, hash)
		}
	}
}

func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The items whose image is one cover all carry its blurhash, by the tag of
// the cover, which Finamp keeps one download of the image by.
func TestDescribesTheBlurHashes(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"Band/First", "Band/Broken"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cover := solid(40, 40, color.NRGBA{200, 30, 30, 255})
	if err := os.WriteFile(filepath.Join(dir, "Band", "First", "cover.png"), pngOf(t, cover), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Band", "Broken", "cover.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []Source{
		source("Band/First/01.flac", audio.Track{Title: "One", Artist: "The Band", AlbumArtist: "The Band",
			Album: "First"}, 1),
		source("Band/First/02.flac", audio.Track{Title: "Two", Artist: "The Band", AlbumArtist: "The Band",
			Album: "First"}, 2),
		source("Band/Broken/01.flac", audio.Track{Title: "Gone", Artist: "Other", Album: "Broken"}, 3),
	}
	l := Build("music", "", "/music", dir, sources,
		Covers(dir, Folders([]string{"Band/First/01.flac", "Band/Broken/01.flac"})), day)
	d := Describer{ServerID: "server"}

	first := l.Item(newID("music", "album", "artist\x00the band\x00first")).(*Album)
	want := blurHash(cover)
	for _, item := range []Item{first, first.Tracks[0], first.Tracks[1], first.Artist} {
		b := d.Item(item)
		tag := b.ImageTags["Primary"]
		if tag == "" || b.ImageBlurHashes["Primary"][tag] != want || len(b.ImageBlurHashes["Primary"]) != 1 {
			t.Errorf("%s %s: tag %q, blurhashes %v, want %q", item.ItemType(), item.ItemName(), tag,
				b.ImageBlurHashes, want)
		}
	}
	broken := d.Item(l.Item(newID("music", "album", "folder\x00Band/Broken\x00broken")))
	if broken.ImageTags["Primary"] == "" || broken.ImageBlurHashes != nil {
		t.Errorf("a cover that cannot be decoded: %v %v", broken.ImageTags, broken.ImageBlurHashes)
	}

	// the part of the library an account sees takes the hash from the cache
	view := l.For("first only", func(v string) bool { return strings.HasPrefix(v, "/music/Band/First/") })
	if got := view.Albums[0].blurHash; got != want {
		t.Errorf("the view's blurhash %q, want %q", got, want)
	}
}
