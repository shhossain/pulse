// Package trayicon renders the menubar progress ring as a macOS template
// image: monochrome (only alpha matters) so macOS tints it for light/dark
// menubars and the highlighted state, like built-in status items.
package trayicon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Size is the rendered pixel size (22pt @2x).
const Size = 44

const (
	cx, cy     = Size / 2.0, Size / 2.0
	outer      = 17.0 // ring outer radius
	inner      = 12.5 // ring inner radius
	trackAlpha = 0.28 // unfilled part of the ring

	// Attention badge: a dot at the top-right, separated from the ring by a
	// transparent gap (the SF Symbols ".badge" look).
	badgeX, badgeY = 36.5, 7.5
	badgeR         = 5.5
	badgeGap       = 2.0

	samples = 4 // supersampling per axis for anti-aliasing
)

// Ring renders a progress ring for fraction p in [0,1]. A negative p renders
// an idle ring (track only). badge adds the attention dot.
func Ring(p float64, badge bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, Size, Size))
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			var sum float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples
					py := float64(y) + (float64(sy)+0.5)/samples
					sum += coverage(px, py, p, badge)
				}
			}
			if a := sum / (samples * samples); a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{A: uint8(math.Round(a * 255))})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// coverage returns the alpha (0..1) at sub-pixel (px, py).
func coverage(px, py, p float64, badge bool) float64 {
	if badge {
		d := math.Hypot(px-badgeX, py-badgeY)
		if d <= badgeR {
			return 1
		}
		if d <= badgeR+badgeGap {
			return 0
		}
	}
	fx, fy := px-cx, py-cy
	r := math.Hypot(fx, fy)
	if r < inner || r > outer {
		return 0
	}
	// Angle clockwise from 12 o'clock, in [0,1).
	a := math.Atan2(fx, -fy) / (2 * math.Pi)
	if a < 0 {
		a++
	}
	if p >= 0 && a <= p {
		return 1
	}
	return trackAlpha
}
