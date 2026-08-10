package poster

import (
	"bytes"
	"embed"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// Fixtures are embedded rather than read off disk so the tests do not depend on
// the working directory, and so a fixture cannot be swapped out from under a
// golden assertion.
//
//go:embed testdata
var fixtures embed.FS

// The golden hashes below were produced by an independent transcription of the
// reference rgbaToThumbHash (evanw/thumbhash) run over the same fixtures, not
// by this package. They are what pins the port: the DCT coefficient order, the
// 4-bit packing and the padded standard-alphabet base64 all have to agree with
// the reference or the iOS decoder renders noise.
var goldens = map[string]string{
	// 32x48 portrait, fully opaque: exercises lx != ly and the no-alpha layout.
	"gradient.png": "WfcJhRqPdTeXeIhXiXiYd3BmB/eH",
	// 40x40 with a transparent border: exercises the hasAlpha layout, which
	// shifts the AC terms by one byte and appends a fourth channel.
	"alpha.png": "GoeGJQg4gIyPdLc3eISACIiIB1eHiIB4WA==",
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestImageThumbhashGolden(t *testing.T) {
	for name, want := range goldens {
		t.Run(name, func(t *testing.T) {
			got := imageThumbhash(readFixture(t, name))
			if got != want {
				t.Errorf("thumbhash mismatch\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// The server accepts base64.StdEncoding only, so the encoding has to be padded
// and use the standard alphabet. alpha.png is the case that proves it: its hash
// needs padding, which RawStdEncoding rejects.
func TestImageThumbhashUsesPaddedStandardAlphabet(t *testing.T) {
	got := imageThumbhash(readFixture(t, "alpha.png"))
	if _, err := base64.RawStdEncoding.DecodeString(got); err == nil {
		t.Error("expected the padded encoding to be rejected as raw base64")
	}
	if _, err := base64.StdEncoding.DecodeString(got); err != nil {
		t.Errorf("StdEncoding decode failed: %v", err)
	}
}

func TestImageThumbhashRejectsUndecodable(t *testing.T) {
	tests := map[string][]byte{
		"empty":         {},
		"not an image":  []byte("<html><body>404 not found</body></html>"),
		"truncated png": readFixture(t, "gradient.png")[:40],
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if got := imageThumbhash(data); got != "" {
				t.Errorf("expected empty hash, got %q", got)
			}
		})
	}
}

// A pixel count past the cap is refused from the header, before any decode
// allocates the frame buffer.
func TestImageThumbhashRejectsPixelBomb(t *testing.T) {
	// 4000x4000 = 16M pixels, twice the cap. A solid image compresses to a few
	// hundred bytes, which is exactly why the byte cap alone is not enough.
	img := image.NewGray(image.Rect(0, 0, 4000, 4000))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding bomb fixture: %v", err)
	}
	if buf.Len() > maxBytes {
		t.Fatalf("fixture is %d bytes, the byte cap would have caught it first", buf.Len())
	}
	if got := imageThumbhash(buf.Bytes()); got != "" {
		t.Errorf("expected empty hash for a %d-pixel image, got %q", 4000*4000, got)
	}
}

// Pixels are the wrong unit on their own: 16 bits per channel decodes to 8
// bytes per pixel, so an image comfortably inside the pixel cap can still ask
// for more memory than the whole relay pod is allowed. This fixture is well
// under both the byte cap and the pixel cap and must still be refused.
func TestImageThumbhashRejectsSixteenBitBomb(t *testing.T) {
	const w, h = 1800, 1800 // 3.24M pixels, 8 bytes each: ~24.7 MiB decoded
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA64(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encoding bomb fixture: %v", err)
	}
	if buf.Len() > maxBytes {
		t.Fatalf("fixture is %d bytes, the byte cap would have caught it first", buf.Len())
	}
	if w*h > maxDecodePixels {
		t.Fatalf("fixture is %d pixels, the pixel cap would have caught it first", w*h)
	}
	if got := imageThumbhash(buf.Bytes()); got != "" {
		t.Errorf("expected a 16-bit decode bomb to be refused, got %q", got)
	}
}

// The budget has to leave real artwork alone: the largest thing a provider
// actually serves is a TMDB "original" poster, 2000x3000 at 8 bits.
func TestDecodeTooBig(t *testing.T) {
	tests := []struct {
		name string
		cfg  image.Config
		want bool
	}{
		{"tmdb original poster, 8-bit rgba", image.Config{Width: 2000, Height: 3000, ColorModel: color.NRGBAModel}, false},
		{"tmdb original poster, jpeg", image.Config{Width: 2000, Height: 3000, ColorModel: color.YCbCrModel}, false},
		{"1080p episode still", image.Config{Width: 1920, Height: 1080, ColorModel: color.RGBAModel}, false},
		// 8-bit RGBA runs out of budget at ~6.3M pixels, which is past every
		// artwork size a provider serves and short of any bomb worth sending.
		{"8-bit rgba just inside the budget", image.Config{Width: 2500, Height: 2500, ColorModel: color.RGBAModel}, false},
		{"8-bit rgba just outside it", image.Config{Width: 2600, Height: 2600, ColorModel: color.RGBAModel}, true},
		{"greyscale at the pixel cap", image.Config{Width: 4000, Height: 2000, ColorModel: color.GrayModel}, false},
		// A palette is a slice, so it also proves the model lookup does not
		// compare something uncomparable.
		{"paletted gif", image.Config{Width: 4000, Height: 2000, ColorModel: color.Palette{}}, false},
		{"16-bit poster", image.Config{Width: 1800, Height: 1800, ColorModel: color.NRGBA64Model}, true},
		{"16-bit just under the pixel cap", image.Config{Width: 3999, Height: 2000, ColorModel: color.NRGBA64Model}, true},
		{"past the pixel cap", image.Config{Width: 4000, Height: 4000, ColorModel: color.GrayModel}, true},
		{"unknown model is charged the worst case", image.Config{Width: 2000, Height: 2000, ColorModel: nil}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeTooBig(tc.cfg); got != tc.want {
				t.Errorf("decodeTooBig(%dx%d) = %v, want %v", tc.cfg.Width, tc.cfg.Height, got, tc.want)
			}
		})
	}
}

func TestDownscaleAspect(t *testing.T) {
	tests := []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"landscape", 400, 200, 100, 50},
		{"portrait", 200, 400, 50, 100},
		{"square", 300, 300, 100, 100},
		{"already small", 64, 48, 64, 48},
		{"exactly at the cap", 100, 100, 100, 100},
		{"extreme aspect floors at one row", 1000, 3, 100, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := downscale(solidImage(tc.w, tc.h, color.NRGBA{R: 10, G: 20, B: 30, A: 255}), maxThumbhashDim)
			if got == nil {
				t.Fatal("expected an image")
			}
			if got.Rect.Dx() != tc.wantW || got.Rect.Dy() != tc.wantH {
				t.Errorf("got %dx%d, want %dx%d", got.Rect.Dx(), got.Rect.Dy(), tc.wantW, tc.wantH)
			}
			if got.Stride != 4*got.Rect.Dx() {
				t.Errorf("stride %d is not tightly packed for width %d", got.Stride, got.Rect.Dx())
			}
		})
	}
}

// Box averaging a solid image has to reproduce that exact color, which is what
// proves the premultiply/un-premultiply round trip is not shifting values.
func TestDownscalePreservesSolidColor(t *testing.T) {
	want := color.NRGBA{R: 200, G: 100, B: 50, A: 255}
	got := downscale(solidImage(320, 240, want), maxThumbhashDim)
	for i := 0; i < len(got.Pix); i += 4 {
		if got.Pix[i] != want.R || got.Pix[i+1] != want.G || got.Pix[i+2] != want.B || got.Pix[i+3] != want.A {
			t.Fatalf("pixel %d is (%d,%d,%d,%d), want (%d,%d,%d,%d)", i/4,
				got.Pix[i], got.Pix[i+1], got.Pix[i+2], got.Pix[i+3], want.R, want.G, want.B, want.A)
		}
	}
}

func TestDownscaleEmptyImage(t *testing.T) {
	if got := downscale(image.NewNRGBA(image.Rect(0, 0, 0, 0)), maxThumbhashDim); got != nil {
		t.Errorf("expected nil for an empty image, got %v", got.Rect)
	}
}

func solidImage(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}
