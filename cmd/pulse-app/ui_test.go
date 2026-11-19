package main

import (
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
	cases := []struct {
		runs  []model.Run
		label string
		frac  float64
		badge bool
	}{
		{nil, "", -1, false},
		{[]model.Run{run(model.Done, true)}, "", -1, false},
		{[]model.Run{run(model.Running, true, epoch(3)), run(model.Running, true, epoch(6))}, "60%", 0.6, false},
		{[]model.Run{run(model.Running, true), run(model.Running, true)}, "2▶", 0, false},
		{[]model.Run{run(model.Failed, false)}, "", -1, true},
		{[]model.Run{run(model.Failed, false), run(model.Running, true, epoch(5))}, "50%", 0.5, true},
		{[]model.Run{run(model.Failed, true)}, "", -1, false},
	}
	for i, c := range cases {
		label, frac, badge := traySummary(c.runs)
		if label != c.label || frac != c.frac || badge != c.badge {
			t.Errorf("case %d: got (%q, %v, %v) want (%q, %v, %v)", i, label, frac, badge, c.label, c.frac, c.badge)
		}
	}
}
