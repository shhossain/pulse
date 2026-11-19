package alert

import (
	"math"
	"strings"
	"testing"
	"time"

	"pulse/internal/model"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Rule
	}{
		{"val_loss < 0.3", Rule{Kind: Threshold, Metric: "val_loss", Op: "<", Value: 0.3}},
		{"gpu_util<=0.2", Rule{Kind: Threshold, Metric: "gpu_util", Op: "<=", Value: 0.2}},
		{"progress >= 90%", Rule{Kind: Progress, Metric: "progress", Op: ">=", Value: 90}},
		{"progress >= 0.9", Rule{Kind: Progress, Metric: "progress", Op: ">=", Value: 90}},
		{"val_loss new min", Rule{Kind: NewBest, Metric: "val_loss", Best: "min"}},
		{"accuracy new best", Rule{Kind: NewBest, Metric: "accuracy", Best: "max"}},
		{"Loss is NaN", Rule{Kind: NaN, Metric: "Loss"}},
		{"Val_Loss < 0.3", Rule{Kind: Threshold, Metric: "Val_Loss", Op: "<", Value: 0.3}},
		{"NO UPDATE FOR 2 days", Rule{Kind: NoUpdate, DurS: 172800}},
		{"any is nan", Rule{Kind: NaN, Metric: "*"}},
		{"eta < 5m", Rule{Kind: ETA, DurS: 300}},
		{"no update for 15m", Rule{Kind: NoUpdate, DurS: 900}},
		{"no updates for 30s", Rule{Kind: NoUpdate, DurS: 30}},
		{"no progress for 2 hours", Rule{Kind: NoProgress, DurS: 7200}},
		{"no update for 10", Rule{Kind: NoUpdate, DurS: 600}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got.Kind != c.want.Kind || got.Metric != c.want.Metric || got.Op != c.want.Op ||
			got.Value != c.want.Value || got.DurS != c.want.DurS || got.Best != c.want.Best {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "loss", "loss ~ 3", "eta < soon", "no update for -5m", "loss > 1e999"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func run() *model.Run {
	return &model.Run{Job: "train", Status: model.Running, Heartbeat: true}
}

func ev(metrics map[string]float64) model.Event {
	return model.Event{Metrics: metrics, Progress: map[string]model.Progress{}}
}

func titles(f []Fired) string {
	var s []string
	for _, x := range f {
		s = append(s, x.Title)
	}
	return strings.Join(s, " | ")
}

func TestThresholdFiresOncePerCrossing(t *testing.T) {
	r, _ := Parse("val_loss < 0.3")
	if r.Cooldown() != 10*time.Minute {
		t.Fatalf("default threshold cooldown = %v", r.Cooldown())
	}
	r.ID, r.CooldownS = "a", 0
	st, rn, now := NewState(), run(), time.Unix(0, 0)
	var fired []string
	for _, v := range []float64{0.5, 0.29, 0.28, 0.31, 0.25} {
		now = now.Add(time.Second)
		if f := st.OnEvent([]Rule{r}, rn, ev(map[string]float64{"val_loss": v}), now, false); len(f) > 0 {
			fired = append(fired, f[0].Subtitle)
		}
	}
	if strings.Join(fired, ",") != "val_loss is 0.29,val_loss is 0.25" {
		t.Fatalf("fired: %q", fired)
	}
	// With the default cooldown, a noisy metric re-crossing soon after is quiet.
	r.CooldownS = 600
	st, fired = NewState(), nil
	for i, v := range []float64{0.5, 0.29, 0.31, 0.28} {
		if f := st.OnEvent([]Rule{r}, rn, ev(map[string]float64{"val_loss": v}), now.Add(time.Duration(i)*time.Minute), false); len(f) > 0 {
			fired = append(fired, f[0].Subtitle)
		}
	}
	if len(fired) != 1 {
		t.Fatalf("with cooldown fired %d times", len(fired))
	}
}

func TestNewBestSkipsWarmupAndRespectsCooldown(t *testing.T) {
	r, _ := Parse("val_loss new min")
	r.ID = "b"
	st, rn, now := NewState(), run(), time.Unix(0, 0)
	count := 0
	for i, v := range []float64{1.0, 0.9, 0.8, 0.7, 0.6} {
		now = now.Add(time.Duration(i) * time.Minute)
		count += len(st.OnEvent([]Rule{r}, rn, ev(map[string]float64{"val_loss": v}), now, false))
	}
	// 0.8 is the first eligible best (3rd value); 0.7 and 0.6 fall inside the 10m cooldown.
	if count != 1 {
		t.Fatalf("fired %d times", count)
	}
	now = now.Add(11 * time.Minute)
	if f := st.OnEvent([]Rule{r}, rn, ev(map[string]float64{"val_loss": 0.5}), now, false); len(f) != 1 || f[0].Subtitle != "0.5 (was 0.6)" {
		t.Fatalf("after cooldown: %+v", f)
	}
}

func TestNaNOncePerMetric(t *testing.T) {
	rules := Defaults()
	st, rn := NewState(), run()
	f := st.OnEvent(rules, rn, ev(map[string]float64{"loss": math.NaN(), "lr": 1}), time.Unix(1, 0), false)
	f = append(f, st.OnEvent(rules, rn, ev(map[string]float64{"loss": math.NaN(), "grad": math.Inf(1)}), time.Unix(2, 0), false)...)
	if got := titles(f); got != "train: loss is NaN | train: grad is ∞" {
		t.Fatalf("got %q", got)
	}
}

func TestStaleUsesRhythm(t *testing.T) {
	rules := Defaults()
	st, rn := NewState(), run()
	now := time.Unix(0, 0)
	for i := 0; i < 6; i++ { // updates every 30s
		now = now.Add(30 * time.Second)
		rn.UpdatedAt = now
		st.OnEvent(rules, rn, ev(nil), now, false)
	}
	if f := st.OnTick(rules, rn, now.Add(4*time.Minute)); len(f) != 0 {
		t.Fatalf("4m quiet with 30s rhythm should not be stale yet: %v", titles(f))
	}
	f := st.OnTick(rules, rn, now.Add(6*time.Minute))
	if len(f) != 1 || f[0].Title != "train looks stuck" {
		t.Fatalf("got %v", titles(f))
	}
	if f := st.OnTick(rules, rn, now.Add(20*time.Minute)); len(f) != 0 {
		t.Fatal("stale must fire once per quiet spell")
	}
	// Push runs without an explicit status never go stale.
	st2, push := NewState(), &model.Run{Job: "sync", Status: model.Running}
	for i := 0; i < 6; i++ {
		st2.OnEvent(rules, push, ev(nil), time.Unix(int64(i), 0), false)
	}
	if f := st2.OnTick(rules, push, time.Unix(3600, 0)); len(f) != 0 {
		t.Fatal("push run without status went stale")
	}
}

func TestETAAndProgress(t *testing.T) {
	eta, _ := Parse("eta < 5m")
	eta.ID = "e"
	prog, _ := Parse("progress >= 50%")
	prog.ID = "p"
	rules := []Rule{eta, prog}
	st, rn, now := NewState(), run(), time.Unix(0, 0)
	in := now.Add(10 * time.Minute)
	rn.Progress = []model.ProgressItem{{Name: "epoch", ETA: &in, Progress: model.Progress{Cur: 4, Total: 10}}}
	e := model.Event{Progress: map[string]model.Progress{"epoch": {Cur: 4, Total: 10}}}
	if f := st.OnEvent(rules, rn, e, now, false); len(f) != 0 {
		t.Fatalf("40%%: %v", titles(f))
	}
	rn.Progress[0].Cur = 6
	if f := st.OnEvent(rules, rn, e, now, false); titles(f) != "train is 60% done" {
		t.Fatalf("60%%: %v", titles(f))
	}
	if f := st.OnTick(rules, rn, now.Add(6*time.Minute)); titles(f) != "train is almost done" {
		t.Fatalf("eta: %v", titles(f))
	}
	if f := st.OnTick(rules, rn, now.Add(7*time.Minute)); len(f) != 0 {
		t.Fatal("eta fired twice")
	}
}

func TestRulesMatchMetricCaseAndPercent(t *testing.T) {
	r, _ := Parse("Val_Loss new min")
	r.ID, r.CooldownS = "b", 0
	st, rn := NewState(), run()
	for i, v := range []float64{0.9, 0.8, 0.7} {
		key := "Val_Loss"
		if i == 2 {
			key = "val_loss" // a different spelling still matches
		}
		if f := st.OnEvent([]Rule{r}, rn, ev(map[string]float64{key: v}), time.Unix(int64(i), 0), false); i == 2 && len(f) != 1 {
			t.Fatalf("mixed-case new min didn't fire: %v", f)
		}
	}
	pct, _ := Parse("acc >= 90%")
	pct.ID = "p"
	if f := NewState().OnEvent([]Rule{pct}, rn, ev(map[string]float64{"acc": 0.93}), time.Unix(1, 0), false); len(f) != 1 {
		t.Fatal("acc >= 90% should fire for acc=0.93")
	}
}

func TestStaleWithBurstyBatches(t *testing.T) {
	rules := Defaults()
	st, rn := NewState(), run()
	base := time.Unix(0, 0)
	for epoch := 0; epoch < 6; epoch++ { // 6 events per batch, batches 30 minutes apart
		now := base.Add(time.Duration(epoch) * 30 * time.Minute)
		for i := 0; i < 6; i++ {
			e := ev(nil)
			e.TS = now.Add(time.Duration(i) * time.Second)
			st.OnEvent(rules, rn, e, now, false)
		}
		rn.UpdatedAt = now
	}
	quiet := rn.UpdatedAt.Add(20 * time.Minute)
	if f := st.OnTick(rules, rn, quiet); len(f) != 0 {
		t.Fatalf("20 minutes quiet in a 30-minute rhythm isn't stuck: %v", titles(f))
	}
}

func TestIdleRunsStillGetTimeRules(t *testing.T) {
	r, _ := Parse("no update for 15m")
	r.ID = "n"
	rn := &model.Run{Job: "deploy", Status: model.Idle, UpdatedAt: time.Unix(0, 0)}
	if f := NewState().OnTick([]Rule{r}, rn, time.Unix(16*60, 0)); len(f) != 1 {
		t.Fatal("an idle push job should get its no-update alert")
	}
}

func TestAskAnswers(t *testing.T) {
	a, err := ParseAnswer("Save=send:save_checkpoint")
	if err != nil || a.Label != "Save" || a.Do != "send:save_checkpoint" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, _ := ParseAnswer("Keep going=none"); a.Do != "" {
		t.Fatal("none should do nothing")
	}
	for _, bad := range []string{"=stop", "X=reboot", "X=send:rm -rf"} {
		if _, err := ParseAnswer(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	r, _ := Parse("loss < 1")
	r.Ask = "Stop?"
	r.Answers = []Answer{{Label: "Yes", Do: "stop"}, {Label: "Yes"}}
	if r.Validate() == nil {
		t.Fatal("duplicate answers accepted")
	}
}
