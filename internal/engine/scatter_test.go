package engine

import (
	"testing"
	"time"

	"pulse/internal/config"
	"pulse/internal/model"
)

func TestScatterPairsOnStep(t *testing.T) {
	e, _, _ := newEngine(t)
	var evs []map[string]any
	for i := 1; i <= 10; i++ {
		ev := map[string]any{"step": i, "loss": float64(20 - i)}
		if i%5 == 2 { // lr is set at steps 2 and 7 only
			ev["lr"] = float64(i) / 1000
		}
		evs = append(evs, ev)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: evs})
	set, err := e.Scatter("r_1", "lr", []string{"loss"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	s := set.Series[0]
	// Step 1 comes before any lr, so it's left out.
	if set.XKey != "lr" || set.XKind != "step" || len(s.X) != 9 || s.T[0] != 2 {
		t.Fatalf("set = %+v", set)
	}
	if s.X[0] != 0.002 || s.X[4] != 0.002 || s.X[5] != 0.007 || s.Y[5] != 13 {
		t.Fatalf("pairs: x=%v y=%v", s.X, s.Y)
	}
	if thin3, _, _ := thin(s.X, s.Y, s.T, 3); len(thin3) != 3 || thin3[2] != s.X[8] {
		t.Fatalf("thin = %v", thin3)
	}
}

func TestScatterFallsBackToTime(t *testing.T) {
	e, _, c := newEngine(t)
	for i := 0; i < 4; i++ {
		e.Push("bench", map[string]any{"batch": 8 << i, "latency": 10 + i}, false)
		c.advance(time.Second)
	}
	r, _ := e.LatestRun("bench")
	set, _ := e.Scatter(r, "batch", []string{"latency"}, 100)
	s := set.Series[0]
	if set.XKind != "time" || len(s.X) != 4 || s.X[3] != 64 || s.Y[3] != 13 || s.T[3] != 3 {
		t.Fatalf("set = %+v", set)
	}
}

func TestScatterChartFromConfig(t *testing.T) {
	e, _, _ := newEngine(t)
	if _, err := e.ApplyConfig(config.Job{Name: "train", Widgets: config.Display{Charts: []config.ChartSpec{
		{X: "lr", Y: []string{"loss"}, Style: "both"},
		{Y: []string{"acc"}, Style: "points"},
	}}}, "test", false); err != nil {
		t.Fatal(err)
	}
	var evs []map[string]any
	for i := 1; i <= 10; i++ {
		evs = append(evs, map[string]any{"step": i, "loss": 1.0 / float64(i), "lr": 0.1, "acc": 0.5})
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: evs})
	r, _ := e.Run("r_1")
	names := map[string]model.Chart{}
	for _, c := range r.Charts {
		names[c.Name] = c
	}
	// The scatter is extra: loss keeps its own chart over steps.
	if c := names["loss vs lr"]; c.X != "lr" || c.Style != "both" || names["loss"].Keys == nil || names["acc"].Style != "points" {
		t.Fatalf("charts = %+v", r.Charts)
	}
	if _, err := e.ApplyConfig(config.Job{Name: "train", Widgets: config.Display{Charts: []config.ChartSpec{{Y: []string{"a"}, Style: "dots"}}}}, "test", false); err == nil {
		t.Fatal("bad style accepted")
	}
}
