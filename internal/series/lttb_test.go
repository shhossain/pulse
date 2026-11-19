package series

import (
	"math"
	"testing"
)

func TestLTTBKeepsEndsAndPeaks(t *testing.T) {
	var xs, ys []float64
	for i := 0; i < 10000; i++ {
		xs = append(xs, float64(i))
		y := math.Sin(float64(i) / 500)
		if i == 4321 {
			y = 50 // a spike must survive
		}
		ys = append(ys, y)
	}
	ox, oy := LTTB(xs, ys, 200)
	if len(ox) != 200 || ox[0] != 0 || ox[199] != 9999 {
		t.Fatalf("len=%d first=%v last=%v", len(ox), ox[0], ox[len(ox)-1])
	}
	spike := false
	for _, v := range oy {
		if v == 50 {
			spike = true
		}
	}
	if !spike {
		t.Fatal("spike was dropped")
	}
	if x, _ := LTTB(xs[:10], ys[:10], 200); len(x) != 10 {
		t.Fatal("short series should be returned as is")
	}
}
