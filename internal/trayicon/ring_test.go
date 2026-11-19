package trayicon

import (
	"bytes"
	"image/png"
	"math"
	"testing"
)

// alphaAt decodes the icon and returns the alpha (0..1) of pixel (x, y).
func alphaAt(t *testing.T, icon []byte, xy [2]int) float64 {
	x, y := xy[0], xy[1]
	t.Helper()
	img, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := img.At(x, y).RGBA()
	return float64(a) / 0xffff
}

// ringPixel is a pixel at the middle of ring i (of n), at angle frac
// clockwise from 12 o'clock.
func ringPixel(n, i int, frac float64) [2]int {
	b := band[n]
	r := outer - float64(i)*(b.width+b.gap) - b.width/2
	ang := frac * 2 * math.Pi
	return [2]int{int(cx + r*math.Sin(ang)), int(cy - r*math.Cos(ang))}
}

func TestRingsFill(t *testing.T) {
	for n := 1; n <= MaxRings; n++ {
		fracs := make([]float64, n)
		for i := range fracs {
			fracs[i] = 0.5 // each ring half full
		}
		icon := Rings(fracs, false)
		for i := 0; i < n; i++ {
			if a := alphaAt(t, icon, ringPixel(n, i, 0.25)); a < 0.95 {
				t.Errorf("%d rings, ring %d: filled part alpha %.2f", n, i, a)
			}
			if a := alphaAt(t, icon, ringPixel(n, i, 0.75)); math.Abs(a-trackAlpha) > 0.05 {
				t.Errorf("%d rings, ring %d: track alpha %.2f, want %.2f", n, i, a, trackAlpha)
			}
		}
		if a := alphaAt(t, icon, [2]int{int(cx), int(cy)}); a != 0 {
			t.Errorf("%d rings: centre not empty (alpha %.2f)", n, a)
		}
	}
}

func TestRingsGapBetweenRings(t *testing.T) {
	icon := Rings([]float64{1, 1}, false)
	b := band[2]
	r := outer - b.width - b.gap/2 // middle of the gap at 3 o'clock
	if a := alphaAt(t, icon, [2]int{int(cx + r), int(cy)}); a > 0.3 {
		t.Errorf("gap between rings has alpha %.2f", a)
	}
}

func TestRingsClampAndIdle(t *testing.T) {
	if !bytes.Equal(Rings(nil, false), Ring(-1, false)) {
		t.Error("no fractions should render the idle ring")
	}
	if !bytes.Equal(Rings([]float64{.9, .8, .7, .6}, true), Rings([]float64{.9, .8, .7}, true)) {
		t.Error("rings beyond MaxRings should be dropped")
	}
}
