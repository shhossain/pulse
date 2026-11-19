package engine

import (
	"time"

	"pulse/internal/model"
)

const (
	etaFastWindow = 15 * time.Minute    // a bar that moves often: rate over the last 15 minutes
	etaSlowPoints = 12                  // a bar that moves rarely: rate over its last 12 moves
	etaKeepFor    = 48 * time.Hour      // forget changes older than this
	etaMaxPoints  = 64                  // and keep at most this many
	etaMinSpan    = 3 * time.Second     // need at least this much history for an estimate
	etaMax        = 30 * 24 * time.Hour // estimates beyond this are noise
)

type sample struct {
	t   time.Time
	cur float64
}

// rateTracker estimates when a progress bar will finish. It remembers when
// the value changed (not every report), so a bar that moves once every
// 30 minutes still gets an estimate after its second move.
type rateTracker struct{ points []sample }

// add records a progress value at time t and returns the estimated finish
// time, or nil if there isn't enough history yet (or no forward progress).
func (tr *rateTracker) add(t time.Time, p model.Progress) *time.Time {
	n := len(tr.points)
	switch {
	case n > 0 && p.Cur < tr.points[n-1].cur:
		tr.points = append(tr.points[:0], sample{t, p.Cur}) // bar restarted (next epoch's batches)
	case n == 0 || p.Cur != tr.points[n-1].cur:
		tr.points = append(tr.points, sample{t, p.Cur})
	}
	// Forget old changes, but keep at least two.
	cut := 0
	for cut < len(tr.points)-2 && (t.Sub(tr.points[cut].t) > etaKeepFor || len(tr.points)-cut > etaMaxPoints) {
		cut++
	}
	tr.points = tr.points[cut:]

	if p.Total <= 0 || p.Cur >= p.Total || len(tr.points) < 2 {
		return nil
	}
	last := tr.points[len(tr.points)-1]
	first := tr.points[0]
	recent := 0
	for i := len(tr.points) - 1; i >= 0 && last.t.Sub(tr.points[i].t) <= etaFastWindow; i-- {
		recent++
	}
	if recent >= 3 {
		first = tr.points[len(tr.points)-recent]
	} else if len(tr.points) > etaSlowPoints {
		first = tr.points[len(tr.points)-etaSlowPoints]
	}
	span := last.t.Sub(first.t)
	done := last.cur - first.cur
	if span < etaMinSpan || done <= 0 {
		return nil
	}
	// Compare in float seconds: converting a huge value to Duration first
	// overflows (and on amd64 wraps to a date in the past).
	secs := (p.Total - p.Cur) / done * span.Seconds()
	if secs > etaMax.Seconds() || secs != secs {
		return nil
	}
	remaining := time.Duration(secs * float64(time.Second))
	eta := last.t.Add(remaining)
	return &eta
}
