package normalize

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pulse/internal/model"
)

func TestNormalize(t *testing.T) {
	now := time.Unix(1000, 0)
	ev := Normalize(map[string]any{
		"epoch":          "3/10",
		"loss":           "0.431",
		"lr":             0.0003,
		"status":         "success",
		"step":           "1200",
		"pid":            1234,
		"date":           "11/19",
		"progress.batch": []any{120.0, 500.0},
		"progress":       0.62,
		"train":          map[string]any{"acc": 0.9},
		"bad":            "nan",
		"note":           "hello",
		"_ts":            1700000000.5,
	}, now)

	if ev.Status != model.Done {
		t.Errorf("status = %q", ev.Status)
	}
	if ev.Step == nil || *ev.Step != 1200 {
		t.Errorf("step = %v", ev.Step)
	}
	if p := ev.Progress["epoch"]; p.Cur != 3 || p.Total != 10 {
		t.Errorf("epoch = %+v", p)
	}
	if p := ev.Progress["batch"]; p.Cur != 120 || p.Total != 500 {
		t.Errorf("batch = %+v", p)
	}
	if p := ev.Progress["main"]; math.Abs(p.Frac()-0.62) > 1e-9 {
		t.Errorf("main = %+v", p)
	}
	if ev.Metrics["loss"] != 0.431 || ev.Metrics["lr"] != 0.0003 || ev.Metrics["train.acc"] != 0.9 {
		t.Errorf("metrics = %v", ev.Metrics)
	}
	if !math.IsNaN(ev.Metrics["bad"]) {
		t.Errorf("bad should be NaN, got %v", ev.Metrics["bad"])
	}
	if ev.Text["note"] != "hello" || ev.Text["date"] != "11/19" {
		t.Errorf("text = %v", ev.Text)
	}
	for _, k := range []string{"pid", "date", "note"} {
		if _, ok := ev.Metrics[k]; ok {
			t.Errorf("%s should be ignored", k)
		}
		if _, ok := ev.Progress[k]; ok {
			t.Errorf("%s should not be progress", k)
		}
	}
	if ev.TS.Unix() != 1700000000 {
		t.Errorf("ts = %v", ev.TS)
	}
}

func TestStepRatio(t *testing.T) {
	ev := Normalize(map[string]any{"step": "2/5"}, time.Now())
	if ev.Step == nil || *ev.Step != 2 || ev.Progress["step"].Total != 5 {
		t.Fatalf("got step=%v progress=%v", ev.Step, ev.Progress)
	}
}

func TestParseStatus(t *testing.T) {
	for in, want := range map[string]model.Status{
		"OK": model.Done, "error": model.Failed, "in_progress": model.Running, "weird": "",
	} {
		if got := ParseStatus(in); got != want {
			t.Errorf("ParseStatus(%q) = %q want %q", in, got, want)
		}
	}
}

func TestTextFieldsAreSafe(t *testing.T) {
	long := strings.Repeat("a", 299) + "éé"
	ev := Normalize(map[string]any{"api_key": "sk-123", "AUTH_TOKEN": "x", "db.password": "p", "current": long,
		"author": "ann", "session_name": "warmup"}, time.Now())
	if ev.Text["author"] != "ann" || ev.Text["session_name"] != "warmup" {
		t.Errorf("ordinary keys dropped as secrets: %v", ev.Text)
	}
	if _, ok := ev.Text["db.password"]; ok {
		t.Error("password stored")
	}
	if _, ok := ev.Text["api_key"]; ok {
		t.Error("secret-looking key stored")
	}
	if _, ok := ev.Text["AUTH_TOKEN"]; ok {
		t.Error("secret-looking key stored")
	}
	if !utf8.ValidString(ev.Text["current"]) {
		t.Error("truncation split a character")
	}
}

func TestBareProgressFromText(t *testing.T) {
	for in, want := range map[string]float64{"1": 1, "2": 2, "100": 100, "0.5": 50, "1.0": 100, "45%": 45} {
		ev := Normalize(map[string]any{"progress": in}, time.Now())
		if got := ev.Progress["main"].Frac() * 100; math.Abs(got-want) > 1e-9 {
			t.Errorf("progress=%s → %v%%, want %v%%", in, got, want)
		}
	}
}
