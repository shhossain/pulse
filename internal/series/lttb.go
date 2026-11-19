// Package series downsamples metric series for display.
package series

import "math"

// LTTB downsamples (xs, ys) to at most n points with the Largest-Triangle-
// Three-Buckets algorithm, which keeps the visual shape (peaks, dips) far
// better than taking every k-th point. xs must be sorted ascending.
func LTTB(xs, ys []float64, n int) ([]float64, []float64) {
	if n >= len(xs) || n < 3 {
		return xs, ys
	}
	outX := make([]float64, 0, n)
	outY := make([]float64, 0, n)
	outX, outY = append(outX, xs[0]), append(outY, ys[0])

	bucket := float64(len(xs)-2) / float64(n-2)
	a := 0
	for i := 0; i < n-2; i++ {
		// Average of the next bucket (the third triangle point).
		start := int(math.Floor(float64(i+1)*bucket)) + 1
		end := min(int(math.Floor(float64(i+2)*bucket))+1, len(xs))
		var avgX, avgY float64
		for j := start; j < end; j++ {
			avgX += xs[j]
			avgY += ys[j]
		}
		cnt := float64(end - start)
		avgX /= cnt
		avgY /= cnt

		// Pick the point in this bucket forming the largest triangle.
		lo := int(math.Floor(float64(i)*bucket)) + 1
		hi := int(math.Floor(float64(i+1)*bucket)) + 1
		best, bestArea := lo, -1.0
		for j := lo; j < hi; j++ {
			area := math.Abs((xs[a]-avgX)*(ys[j]-ys[a]) - (xs[a]-xs[j])*(avgY-ys[a]))
			if area > bestArea {
				best, bestArea = j, area
			}
		}
		outX, outY = append(outX, xs[best]), append(outY, ys[best])
		a = best
	}
	return append(outX, xs[len(xs)-1]), append(outY, ys[len(ys)-1])
}
