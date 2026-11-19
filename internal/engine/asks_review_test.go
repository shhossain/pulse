package engine

import (
	"strings"
	"testing"
	"time"

	"pulse/internal/alert"
	"pulse/internal/config"
	"pulse/internal/format"
	"pulse/internal/model"
)

func askRule(t *testing.T, expr, question string, answers ...string) alert.Rule {
	t.Helper()
	r, err := alert.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	r.Ask = question
	for _, s := range answers {
		a, err := alert.ParseAnswer(s)
		if err != nil {
			t.Fatal(err)
		}
		r.Answers = append(r.Answers, a)
	}
	return r
}

// A job's own "loss is nan" question must win over the built-in NaN notice
// for the same event.
func TestNaNQuestionBeatsBuiltin(t *testing.T) {
	e, n, _ := newEngine(t)
	if _, err := e.AddAlert("train", askRule(t, "loss is nan", "Loss is NaN. Stop?", "Stop=stop", "Keep going")); err != nil {
		t.Fatal(err)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "running"}, {"loss": 0.5}, {"loss": "nan"}}})
	if asks := e.OpenAsks(); len(asks) != 1 {
		t.Fatalf("asks = %+v, notifications = %v", asks, n.titles())
	}
}

func TestAskRuleChecks(t *testing.T) {
	stop := askRule(t, "loss > 5", "Stop?", "Stop=stop")
	stop.Action = "stop"
	if stop.Validate() == nil {
		t.Error("ask plus run: stop accepted (the stop would never happen)")
	}
	orphan := askRule(t, "loss > 5", "", "Stop=stop")
	if orphan.Validate() == nil {
		t.Error("answers without a question accepted")
	}
	e, _, _ := newEngine(t)
	if _, err := e.AddAlert("train", askRule(t, "loss > 5", "Save?", "Save=action:checkpoint")); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Errorf("unknown action accepted: %v", err)
	}
	_, err := e.ApplyConfig(config.Job{Name: "train", Alerts: []config.Alert{{Expr: "loss > 5", Ask: "  Save?\n", Answers: []string{"Save=action:checkpoint"}}},
		ActionList: []config.Action{{Name: "checkpoint", Send: "save", Source: "file"}}}, "/tmp/p.yaml", false)
	if err != nil {
		t.Fatalf("action from the same file: %v", err)
	}
	if v := e.Alerts("train"); v.Rules[len(v.Rules)-1].Ask != "Save?" {
		t.Errorf("question not trimmed: %q", v.Rules[len(v.Rules)-1].Ask)
	}
}

// An answer whose follow-up can't happen tells the user instead of failing quietly.
func TestFailedAnswerNotifies(t *testing.T) {
	e, n, _ := newEngine(t)
	r := askRule(t, "loss > 5", "Spiking. Save?", "Save=send:save_checkpoint", "Ignore")
	r.CooldownS = 0
	e.AddAlert("train", r)
	// runMeta's run doesn't listen for save_checkpoint.
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "running"}, {"loss": 1}, {"loss": 9}}})
	asks := e.OpenAsks()
	if len(asks) != 1 {
		t.Fatalf("asks = %+v", asks)
	}
	if _, err := e.Answer(asks[0].ID, "Save", "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, title := range n.titles() {
			if strings.Contains(title, "didn't go through") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no failure notice: %v", n.titles())
}

func TestLabelsAreAllOrNothing(t *testing.T) {
	e, _, _ := newEngine(t)
	resp, _ := e.Push("upload", map[string]any{"a": 1, "b": 2}, false)
	err := e.SetLabels("upload", map[string]string{"a": "Fine"}, map[string]string{"b": strings.Repeat("x", 61)})
	if err == nil {
		t.Fatal("over-long unit accepted")
	}
	if r, _ := e.Run(resp.Run); r.Labels["a"] != "" {
		t.Fatalf("half applied: %v", r.Labels)
	}
	if _, err := e.ApplyConfig(config.Job{Name: "upload", Widgets: config.Display{Units: map[string]string{"b": strings.Repeat("é", 61)}}}, "/tmp/p.yaml", false); err == nil {
		t.Fatal("over-long file unit accepted")
	}
	// An empty file label doesn't blank a CLI one.
	e.SetLabels("upload", map[string]string{"a": "Fine"}, nil)
	if r, _ := e.Run(resp.Run); format.LabelFor(&r, "a") != "Fine" {
		t.Fatal("CLI label lost")
	}
	r, _ := e.Run(resp.Run)
	if got := format.KeySummary(&r); got != "a 1 · b 2" {
		t.Fatalf("CLI summary should keep raw keys: %q", got)
	}
}
