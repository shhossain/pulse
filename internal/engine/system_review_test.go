package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"pulse/internal/alert"
	"pulse/internal/model"
)

// System samples keep arriving while a job hangs; they must not hide it.
func TestSystemSamplesDontHideAHungJob(t *testing.T) {
	e, n, c := newEngine(t)
	r, _ := alert.Parse("no update for 2m")
	e.AddAlert("train", r)
	meta := runMeta()
	for i := 0; i < 8; i++ {
		e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: meta, Events: []map[string]any{{"loss": 1.0 / float64(i+1)}}})
		c.advance(10 * time.Second)
	}
	for i := 0; i < 60; i++ { // 5 minutes of samples, nothing from the job
		e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: meta, Events: []map[string]any{{"sys.cpu": 100.0, "sys.mem": 2.0}}})
		c.advance(5 * time.Second)
		e.Tick()
	}
	got := strings.Join(n.titles(), " | ")
	if !strings.Contains(got, "hasn't reported") {
		t.Fatalf("hung job not reported: %q", got)
	}
	if run, _ := e.Run("r_1"); run.Updates != 8 || run.SysUpdates != 60 {
		t.Fatalf("updates=%d sys=%d", run.Updates, run.SysUpdates)
	}
}

func TestSystemKeysAreExact(t *testing.T) {
	e, _, _ := newEngine(t)
	var evs []map[string]any
	for i := 1; i <= 6; i++ {
		evs = append(evs, map[string]any{"step": i, "sys.throughput": float64(i)})
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: evs})
	r, _ := e.Run("r_1")
	for _, c := range r.Charts {
		if c.Group == "system" {
			t.Fatalf("a job's own sys.* metric was taken for a system one: %+v", r.Charts)
		}
	}
	// Room for system metrics even when the job uses every slot.
	big := map[string]any{}
	for i := 0; i < 70; i++ {
		big[fmt.Sprintf("m%02d", i)] = 1.0
	}
	e.ApplyBatch("r_2", model.Batch{Job: "wide", Meta: runMeta(), Events: []map[string]any{big, {"sys.cpu": 50.0, "sys.mem": 1.0}}})
	r2, _ := e.Run("r_2")
	have := 0
	for _, m := range r2.Metrics {
		if model.IsSystemKey(m.Name) {
			have++
		}
	}
	if have != 2 {
		t.Fatalf("system metrics kept: %d of 2", have)
	}
}

func TestPercentRuleOnSystemPercent(t *testing.T) {
	r, _ := alert.Parse("sys.gpu < 10%")
	st := alert.NewState()
	run := &model.Run{Job: "train"}
	ev := func(v float64) model.Event { return model.Event{Metrics: map[string]float64{"sys.gpu": v}} }
	st.OnEvent([]alert.Rule{r}, run, ev(50), time.Unix(1, 0), true)
	if f := st.OnEvent([]alert.Rule{r}, run, ev(0.5), time.Unix(2, 0), true); len(f) != 1 {
		t.Fatal("a GPU at 0.5% is below 10%")
	}
}
