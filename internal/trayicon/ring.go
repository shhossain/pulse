// Package trayicon renders the menubar progress rings as a macOS template
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

// MaxRings is how many concentric rings fit in the icon and stay legible.
const MaxRings = 3

const (
	cx, cy     = Size / 2.0, Size / 2.0
	outer      = 17.0 // outermost ring's outer radius
	trackAlpha = 0.28 // unfilled part of a ring

	// Attention badge: a dot at the top-right, separated from the ring by a
	// transparent gap (the SF Symbols ".badge" look).
	badgeX, badgeY = 36.5, 7.5
	badgeR         = 5.5
	badgeGap       = 2.0

	samples = 4 // supersampling per axis for anti-aliasing
)

// band is one ring's thickness and the gap to the next ring inward, by ring
// count: rings get thinner as more share the icon (in pixels, 2px = 1pt).
var band = [MaxRings + 1]struct{ width, gap float64 }{
	1: {4.5, 0},
	2: {3.5, 1.5},
	3: {2.75, 1.25},
}

// Ring renders a single progress ring for fraction p in [0,1]. A negative p
// renders an idle ring (track only). badge adds the attention dot.
func Ring(p float64, badge bool) []byte {
	return Rings([]float64{p}, badge)
}

// Rings renders one concentric ring per fraction, the first outermost. Each
// fraction is in [0,1]; a negative one draws that ring's track only. No
// fractions renders the idle ring; more than MaxRings keeps the first ones.
// badge adds the attention dot.
func Rings(fracs []float64, badge bool) []byte {
	switch {
	case len(fracs) == 0:
		fracs = []float64{-1}
	case len(fracs) > MaxRings:
		fracs = fracs[:MaxRings]
	}
	img := image.NewNRGBA(image.Rect(0, 0, Size, Size))
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			var sum float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples
					py := float64(y) + (float64(sy)+0.5)/samples
					sum += coverage(px, py, fracs, badge)
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
func coverage(px, py float64, fracs []float64, badge bool) float64 {
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
	b := band[len(fracs)]
	// Which ring (0 = outermost) does radius r fall in, if any?
	depth := outer - r
	if depth < 0 {
		return 0
	}
	i := int(depth / (b.width + b.gap))
	if i >= len(fracs) || depth-float64(i)*(b.width+b.gap) > b.width {
		return 0
	}
	// Angle clockwise from 12 o'clock, in [0,1).
	a := math.Atan2(fx, -fy) / (2 * math.Pi)
	if a < 0 {
		a++
	}
	if p := fracs[i]; p >= 0 && a <= p {
		return 1
	}
	return trackAlpha
}
