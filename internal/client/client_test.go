package client

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestResolveNeverSendsLocalTokensElsewhere(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	t.Setenv("PULSE_TOKEN", "")
	t.Setenv("PULSE_URL", "http://gpu-box.example:7777")
	if _, err := Resolve(false); err == nil || !strings.Contains(err.Error(), "PULSE_TOKEN") {
		t.Fatalf("remote URL without token: %v", err)
	}
	t.Setenv("PULSE_URL", "http://127.0.0.1:7777")
	if _, err := Resolve(false); err != nil {
		t.Fatalf("local URL: %v", err)
	}
}

func TestBatchesFitTheBudget(t *testing.T) {
	var events []map[string]any
	for i := 0; i < 500; i++ {
		events = append(events, map[string]any{"blob": strings.Repeat("x", 9000)})
	}
	n, _ := fitBudget(events, nil)
	b, _ := json.Marshal(events[:n])
	if n == 0 || n == 500 || len(b) > maxBatchBytes {
		t.Fatalf("n=%d bytes=%d", n, len(b))
	}
	if n, nl := fitBudget(events[:1], []string{strings.Repeat("y", 5<<20)}); n != 1 || nl != 0 {
		t.Fatalf("huge line should wait for its own batch: n=%d nl=%d", n, nl)
	}
	if n, nl := fitBudget(nil, []string{strings.Repeat("y", 5<<20)}); n != 0 || nl != 1 {
		t.Fatalf("a lone huge line still goes out: n=%d nl=%d", n, nl)
	}
}

func TestCoalesceKeepsOtherBars(t *testing.T) {
	s := NewRunSender("r_1", "j", modelMeta())
	s.Add(map[string]any{"progress.epoch": []any{3.0, 10.0}, "_coalesce": true})
	s.Add(map[string]any{"progress.batch": []any{1.0, 100.0}, "_coalesce": true})
	s.Add(map[string]any{"progress.batch": []any{2.0, 100.0}, "_coalesce": true})
	if len(s.pending) != 2 {
		t.Fatalf("pending = %v", s.pending)
	}
}

func TestUnsendableEventIsDropped(t *testing.T) {
	_, err := json.Marshal(map[string]any{"x": math.Inf(1)})
	var u *json.UnsupportedValueError
	if err == nil || !errorsAs(err, &u) {
		t.Fatalf("expected UnsupportedValueError, got %v", err)
	}
}

func TestLogTrimKeepsInFlightLines(t *testing.T) {
	s := NewRunSender("r_1", "j", modelMeta())
	for i := 0; i < maxLogPending; i++ {
		s.AddLog("old")
	}
	s.logsOut = 2000 // a send of the first 2000 lines is in flight
	s.AddLog("new1")
	s.AddLog("new2")
	if len(s.logs) != maxLogPending {
		t.Fatalf("len = %d", len(s.logs))
	}
	s.logs = dropFront(s.logs, 2000) // the send succeeded
	if got := s.logs[len(s.logs)-2:]; got[0] != "new1" || got[1] != "new2" {
		t.Fatalf("unsent lines lost: %v", got)
	}
	if len(s.logs) != maxLogPending-2000 {
		t.Fatalf("wrong lines removed: %d left", len(s.logs))
	}
}
