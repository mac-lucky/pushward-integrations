// This file is an encode-only port of ThumbHash by Evan Wallace
// (https://github.com/evanw/thumbhash), used under the MIT License:
//
// Copyright (c) 2023 Evan Wallace
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// Only the encoder is ported: the iOS client is the only decoder, and the
// relay never needs to read a hash back. The port is deliberately literal so a
// hash produced here matches the reference byte for byte.

package poster

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"math"

	// Decoders for the formats providers actually serve. A format that is not
	// registered (WebP, AVIF) fails to decode, which the caller treats as "no
	// thumbhash" - no new module dependency is worth adding for it.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

const (
	// maxThumbhashDim is the largest edge ThumbHash accepts. Anything bigger is
	// slower with no effect on the 4-bit output.
	maxThumbhashDim = 100
	// maxDecodePixels caps the decoded pixel count. maxBytes bounds the encoded
	// size, which a decompression bomb can still expand by orders of magnitude,
	// so the pixel count is checked from the header before decoding. It is the
	// backstop behind maxDecodeBytes, which is the tighter of the two.
	maxDecodePixels = 8_000_000
	// maxDecodeBytes caps the frame buffer a decode is allowed to allocate.
	// Pixels alone are the wrong unit: a 16-bit-per-channel image decodes to
	// image.NRGBA64 at 8 bytes per pixel, so a 64 KB PNG can sit just under the
	// pixel cap and still ask for 122 MB - four of those at once OOM a relay pod
	// with a 64Mi limit. The estimate is deliberately generous enough for real
	// artwork: a 2000x3000 8-bit TMDB "original" poster needs 24 MB.
	maxDecodeBytes = 24 << 20 // 24 MiB
)

// imageThumbhash decodes an encoded image and returns its padded base64
// ThumbHash. Every failure path returns "": a card without a placeholder is a
// degraded card, not a failed webhook.
func imageThumbhash(data []byte) string {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return ""
	}
	if decodeTooBig(cfg) {
		return ""
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}

	small := downscale(img, maxThumbhashDim)
	if small == nil {
		return ""
	}
	w, h := small.Rect.Dx(), small.Rect.Dy()
	return base64.StdEncoding.EncodeToString(rgbaToThumbHash(w, h, small.Pix))
}

// decodeTooBig reports whether decoding an image with this header would cost
// more than the frame-buffer budget. Both halves are judged from the header,
// because by the time Decode returns the memory is already spent.
func decodeTooBig(cfg image.Config) bool {
	pixels := int64(cfg.Width) * int64(cfg.Height)
	return pixels > maxDecodePixels || pixels*bytesPerPixel(cfg.ColorModel) > maxDecodeBytes
}

// bytesPerPixel is how much one pixel costs in the buffer the standard library
// will allocate for an image of this color model. An unrecognised model is
// charged the worst case rather than the average: the number only exists to
// refuse a decode, and under-charging is the failure that matters.
func bytesPerPixel(m color.Model) int64 {
	// A paletted image reports its palette as the model, which is a slice type
	// and so cannot be compared in the switch below.
	if _, ok := m.(color.Palette); ok {
		return 1 // image.Paletted
	}
	switch m {
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.YCbCrModel:
		return 3 // 4:4:4; the subsampled variants are cheaper
	case color.RGBAModel, color.NRGBAModel, color.CMYKModel, color.NYCbCrAModel:
		return 4
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	return 8
}

// downscale box-averages img down so neither edge exceeds maxDim, returning a
// tightly packed non-premultiplied RGBA image. Sampling goes through
// image.Image rather than a full-size intermediate buffer, so peak memory
// tracks the decoded image alone. Averaging happens in premultiplied space and
// is divided back out, which is what keeps transparent edges from bleeding
// black into their neighbours.
func downscale(img image.Image, maxDim int) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}

	nw, nh := w, h
	if w > maxDim || h > maxDim {
		if w >= h {
			nw = maxDim
			nh = max(1, int(math.Round(float64(h)*float64(maxDim)/float64(w))))
		} else {
			nh = maxDim
			nw = max(1, int(math.Round(float64(w)*float64(maxDim)/float64(h))))
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		y0, y1 := y*h/nh, (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		y1 = min(y1, h)
		for x := range nw {
			x0, x1 := x*w/nw, (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			x1 = min(x1, w)

			var sr, sg, sb, sa float64
			n := 0
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, al := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					sr += float64(r)
					sg += float64(g)
					sb += float64(bl)
					sa += float64(al)
					n++
				}
			}

			o := dst.PixOffset(x, y)
			if n == 0 || sa == 0 {
				dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = 0, 0, 0, 0
				continue
			}
			dst.Pix[o] = clamp8(sr / sa * 255)
			dst.Pix[o+1] = clamp8(sg / sa * 255)
			dst.Pix[o+2] = clamp8(sb / sa * 255)
			dst.Pix[o+3] = clamp8(sa / float64(n) / 65535 * 255)
		}
	}
	return dst
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}

// rgbaToThumbHash is the reference rgbaToThumbHash. rgba is w*h*4 bytes of
// non-premultiplied RGBA, row by row; w and h must both be <= 100.
func rgbaToThumbHash(w, h int, rgba []byte) []byte {
	// Determine the average color.
	var avgR, avgG, avgB, avgA float64
	for i, j := 0, 0; i < w*h; i, j = i+1, j+4 {
		alpha := float64(rgba[j+3]) / 255
		avgR += alpha / 255 * float64(rgba[j])
		avgG += alpha / 255 * float64(rgba[j+1])
		avgB += alpha / 255 * float64(rgba[j+2])
		avgA += alpha
	}
	if avgA > 0 {
		avgR /= avgA
		avgG /= avgA
		avgB /= avgA
	}

	hasAlpha := avgA < float64(w*h)
	// Fewer luminance bits when alpha has to be encoded as well.
	lLimit := 7.0
	if hasAlpha {
		lLimit = 5.0
	}
	maxDim := float64(max(w, h))
	lx := max(1, int(math.Round(lLimit*float64(w)/maxDim)))
	ly := max(1, int(math.Round(lLimit*float64(h)/maxDim)))

	// Convert from RGBA to LPQA, compositing atop the average color.
	l := make([]float64, w*h) // luminance
	p := make([]float64, w*h) // yellow - blue
	q := make([]float64, w*h) // red - green
	a := make([]float64, w*h) // alpha
	for i, j := 0, 0; i < w*h; i, j = i+1, j+4 {
		alpha := float64(rgba[j+3]) / 255
		r := avgR*(1-alpha) + alpha/255*float64(rgba[j])
		g := avgG*(1-alpha) + alpha/255*float64(rgba[j+1])
		b := avgB*(1-alpha) + alpha/255*float64(rgba[j+2])
		l[i] = (r + g + b) / 3
		p[i] = (r+g)/2 - b
		q[i] = r - g
		a[i] = alpha
	}

	lDC, lAC, lScale := encodeChannel(l, max(3, lx), max(3, ly), w, h)
	pDC, pAC, pScale := encodeChannel(p, 3, 3, w, h)
	qDC, qAC, qScale := encodeChannel(q, 3, 3, w, h)
	var aDC, aScale float64
	var aAC []float64
	if hasAlpha {
		aDC, aAC, aScale = encodeChannel(a, 5, 5, w, h)
	}

	isLandscape := 0
	if w > h {
		isLandscape = 1
	}
	alphaBit := 0
	if hasAlpha {
		alphaBit = 1
	}

	// The two headers are packed bit fields. Every term is a rounded product of
	// a value already clamped to [0,1] (or [-1,1] for the two DC chroma terms,
	// biased by 31.5), so each lands inside its field width by construction.
	header24 := int(math.Round(63*lDC)) |
		int(math.Round(31.5+31.5*pDC))<<6 |
		int(math.Round(31.5+31.5*qDC))<<12 |
		int(math.Round(31*lScale))<<18 |
		alphaBit<<23
	lDim := lx
	if isLandscape == 1 {
		lDim = ly
	}
	header16 := lDim |
		int(math.Round(63*pScale))<<3 |
		int(math.Round(63*qScale))<<9 |
		isLandscape<<15

	acStart := 5
	if hasAlpha {
		acStart = 6
	}
	acCount := len(lAC) + len(pAC) + len(qAC) + len(aAC)

	hash := make([]byte, acStart+(acCount+1)/2)
	hash[0] = byte(header24 & 0xff)
	hash[1] = byte((header24 >> 8) & 0xff)
	hash[2] = byte((header24 >> 16) & 0xff)
	hash[3] = byte(header16 & 0xff)
	hash[4] = byte((header16 >> 8) & 0xff)
	if hasAlpha {
		hash[5] = byte((int(math.Round(15*aDC)) | int(math.Round(15*aScale))<<4) & 0xff)
	}

	channels := [][]float64{lAC, pAC, qAC}
	if hasAlpha {
		channels = append(channels, aAC)
	}
	acIndex := 0
	for _, ac := range channels {
		for _, f := range ac {
			hash[acStart+acIndex/2] |= byte(math.Round(15*f)) << ((acIndex & 1) * 4)
			acIndex++
		}
	}
	return hash
}

// encodeChannel runs the DCT over one channel, returning its DC term, the AC
// terms normalized into [0,1], and the scale they were normalized by.
func encodeChannel(channel []float64, nx, ny, w, h int) (dc float64, ac []float64, scale float64) {
	fx := make([]float64, w)
	for cy := range ny {
		for cx := 0; cx*ny < nx*(ny-cy); cx++ {
			f := 0.0
			for x := range w {
				fx[x] = math.Cos(math.Pi / float64(w) * float64(cx) * (float64(x) + 0.5))
			}
			for y := range h {
				fy := math.Cos(math.Pi / float64(h) * float64(cy) * (float64(y) + 0.5))
				for x := range w {
					f += channel[x+y*w] * fx[x] * fy
				}
			}
			f /= float64(w * h)
			if cx > 0 || cy > 0 {
				ac = append(ac, f)
				scale = math.Max(scale, math.Abs(f))
			} else {
				dc = f
			}
		}
	}
	if scale > 0 {
		for i := range ac {
			ac[i] = 0.5 + 0.5/scale*ac[i]
		}
	}
	return dc, ac, scale
}
