package main

import (
	"slices"
	"testing"

	"pulse/internal/model"
)

func TestTraySummary(t *testing.T) {
	run := func(status model.Status, seen bool, p ...model.ProgressItem) model.Run {
		return model.Run{Status: status, Seen: seen, Progress: p}
	}
	epoch := func(cur float64) model.ProgressItem {
		return model.ProgressItem{Name: "epoch", Progress: model.Progress{Cur: cur, Total: 10}}
	}
	running := func(cur float64) model.Run { return run(model.Running, true, epoch(cur)) }
	bare := run(model.Running, true)
	cases := []struct {
		name  string
		runs  []model.Run
		label string
		fracs []float64
		badge bool
	}{
		{"nothing", nil, "", nil, false},
		{"only finished", []model.Run{run(model.Done, true)}, "", nil, false},
		{"one with progress", []model.Run{running(6)}, "60%", []float64{0.6}, false},
		{"label doesn't lose a percent to float error", []model.Run{running(2.9)}, "29%", []float64{0.29}, false},
		{"two: ring each, label is the one behind", []model.Run{running(3), running(6)}, "30%·2", []float64{0.6, 0.3}, false},
		{"order follows progress, not input", []model.Run{running(6), running(3)}, "30%·2", []float64{0.6, 0.3}, false},
		{"four: the three furthest behind", []model.Run{running(9), running(2), running(5), running(7)}, "20%·4", []float64{0.7, 0.5, 0.2}, false},
		{"one without progress counts but has no ring", []model.Run{running(5), bare}, "50%·2", []float64{0.5}, false},
		{"one without progress", []model.Run{bare}, "", []float64{0}, false},
		{"several without progress", []model.Run{bare, bare}, "2▶", []float64{0}, false},
		{"failed unseen badges", []model.Run{run(model.Failed, false)}, "", nil, true},
		{"badge with progress", []model.Run{run(model.Failed, false), running(5)}, "50%", []float64{0.5}, true},
		{"failed seen", []model.Run{run(model.Failed, true)}, "", nil, false},
	}
	for _, c := range cases {
		label, fracs, badge := traySummary(c.runs)
		if label != c.label || !slices.Equal(fracs, c.fracs) || badge != c.badge {
			t.Errorf("%s: got (%q, %v, %v) want (%q, %v, %v)", c.name, label, fracs, badge, c.label, c.fracs, c.badge)
		}
	}
}

func TestRingsMoved(t *testing.T) {
	cases := []struct {
		a, b []float64
		want bool
	}{
		{nil, nil, false},
		{nil, []float64{-2}, true},
		{[]float64{0.5}, []float64{0.5, 0.2}, true},
		{[]float64{0.5, 0.2}, []float64{0.501, 0.2}, false},
		{[]float64{0.5, 0.2}, []float64{0.5, 0.21}, true},
	}
	for i, c := range cases {
		if got := ringsMoved(c.a, c.b); got != c.want {
			t.Errorf("case %d: ringsMoved(%v, %v) = %v", i, c.a, c.b, got)
		}
	}
}
