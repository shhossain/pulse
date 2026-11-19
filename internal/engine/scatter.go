package engine

import (
	"fmt"
	"math"
	"sort"

	"pulse/internal/model"
	"pulse/internal/store"
)

// Scatter pairs each sample of the y metrics with the value x had at that
// moment, for plotting y against x. Samples are matched on step when both
// metrics log steps that only go up, otherwise on time; either way a y
// sample takes the latest x at or before it (x is often logged less often,
// like a learning rate set once per epoch). Points keep their order in the
// run, so they can be joined into a path; T is the step or seconds since
// the start (SeriesSet.XKind says which).
func (e *Engine) Scatter(runID, xKey string, keys []string, points int) (model.SeriesSet, error) {
	set := model.SeriesSet{Run: runID, XKind: "step", XKey: xKey, Series: []model.Series{}}
	run, ok := e.Run(runID)
	if !ok {
		r, err := e.st.Run(runID)
		if err != nil || r == nil {
			return set, fmt.Errorf("no such run")
		}
		run = *r
	}
	xs, err := e.st.Series(runID, xKey)
	if err != nil {
		return set, err
	}
	xs = finite(xs)
	ys := make([][]store.SeriesPoint, len(keys))
	steps := increasingSteps(xs)
	for i, k := range keys {
		if ys[i], err = e.st.Series(runID, k); err != nil {
			return set, err
		}
		ys[i] = finite(ys[i])
		steps = steps && increasingSteps(ys[i])
	}
	if !steps {
		set.XKind = "time"
	}
	at := func(p store.SeriesPoint) float64 {
		if steps {
			return p.X
		}
		return float64(p.TS)
	}
	start := run.StartedAt.UnixMilli()
	for i, k := range keys {
		s := model.Series{Key: k, X: []float64{}, Y: []float64{}, T: []float64{}}
		for _, p := range ys[i] {
			// Latest x at or before this sample.
			j := sort.Search(len(xs), func(j int) bool { return at(xs[j]) > at(p) }) - 1
			if j < 0 {
				continue
			}
			t := p.X
			if !steps {
				t = float64(p.TS-start) / 1000
			}
			s.X, s.Y, s.T = append(s.X, xs[j].V), append(s.Y, p.V), append(s.T, t)
		}
		s.X, s.Y, s.T = thin(s.X, s.Y, s.T, points)
		set.Series = append(set.Series, s)
	}
	return set, nil
}

func finite(pts []store.SeriesPoint) []store.SeriesPoint {
	out := pts[:0:0]
	for _, p := range pts {
		if !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
			out = append(out, p)
		}
	}
	return out
}

// increasingSteps reports whether every sample has a step and steps only go
// up (a counter that restarts each epoch can't be matched on).
func increasingSteps(pts []store.SeriesPoint) bool {
	prev := math.Inf(-1)
	for _, p := range pts {
		if math.IsNaN(p.X) || p.X <= prev {
			return false
		}
		prev = p.X
	}
	return true
}

// thin keeps at most n evenly spaced points, always including the last.
// (LTTB needs x to go up, which a scatter's x doesn't.)
func thin(x, y, t []float64, n int) ([]float64, []float64, []float64) {
	if n < 2 || len(x) <= n {
		return x, y, t
	}
	ox, oy, ot := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
	step := float64(len(x)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		k := int(math.Round(float64(i) * step))
		ox, oy, ot = append(ox, x[k]), append(oy, y[k]), append(ot, t[k])
	}
	return ox, oy, ot
}
