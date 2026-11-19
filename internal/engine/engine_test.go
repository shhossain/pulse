package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"pulse/internal/alert"
	"pulse/internal/config"
	"pulse/internal/format"
	"pulse/internal/logs"
	"pulse/internal/model"
	"pulse/internal/store"
)

type fakeNotifier struct {
	mu    sync.Mutex
	notes []model.Notification
}

func (f *fakeNotifier) Notify(n model.Notification) {
	f.mu.Lock()
	f.notes = append(f.notes, n)
	f.mu.Unlock()
}

func (f *fakeNotifier) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, n := range f.notes {
		out = append(out, n.Title)
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newEngine(t *testing.T) (*Engine, *fakeNotifier, *clock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pulse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := &fakeNotifier{}
	e, err := New(st, n)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	e.now = c.now
	e.lastTick = c.t
	return e, n, c
}

func runMeta() model.RunMeta {
	return model.RunMeta{Source: "run", Cmd: "python train.py", Cwd: "/a", Heartbeat: true}
}

func TestRunLifecycleDone(t *testing.T) {
	e, n, c := newEngine(t)
	if _, err := e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(),
		Events: []map[string]any{{"epoch": "1/10", "loss": "0.9"}}}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	resp, _ := e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(),
		Events: []map[string]any{{"epoch": "10/10", "loss": "0.2"}, {"status": "done", "exit_code": 0}}})
	if resp.Job != "train" {
		t.Fatalf("job = %q", resp.Job)
	}
	r, _ := e.Run("r_1")
	if r.Status != model.Done || r.Seq != 1 || r.Metrics[0].Value != 0.2 {
		t.Fatalf("run = %+v", r)
	}
	if got := n.titles(); len(got) != 1 || got[0] != "train finished" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestShortRunFinishesSilentlyButFailureAlwaysNotifies(t *testing.T) {
	e, n, _ := newEngine(t)
	e.ApplyBatch("r_a", model.Batch{Job: "ls", Meta: runMeta(), Events: []map[string]any{{"status": "done"}}})
	e.ApplyBatch("r_b", model.Batch{Job: "make", Meta: model.RunMeta{Source: "run", Cwd: "/b", Heartbeat: true},
		Events: []map[string]any{{"status": "failed", "exit_code": 2, "message": "boom"}}})
	if got := n.titles(); len(got) != 1 || got[0] != "make failed" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestLostAndRecovered(t *testing.T) {
	e, n, c := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta()})
	for range 8 { // 40s of ticks without heartbeat
		c.advance(5 * time.Second)
		e.Tick()
	}
	if r, _ := e.Run("r_1"); r.Status != model.Lost {
		t.Fatalf("status = %s", r.Status)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta()}) // heartbeat returns
	if r, _ := e.Run("r_1"); r.Status != model.Running {
		t.Fatalf("status after beat = %s", r.Status)
	}
	if got := n.titles(); len(got) != 1 || got[0] != "train lost contact" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestSleepDoesNotMarkLost(t *testing.T) {
	e, n, c := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta()})
	c.advance(2 * time.Hour) // laptop slept
	e.Tick()
	if r, _ := e.Run("r_1"); r.Status != model.Running {
		t.Fatalf("status = %s", r.Status)
	}
	if len(n.titles()) != 0 {
		t.Fatalf("unexpected notifications %v", n.titles())
	}
}

func TestPushRunRules(t *testing.T) {
	e, n, c := newEngine(t)
	a, _ := e.Push("deploy", map[string]any{"status": "running"}, false)
	b, _ := e.Push("deploy", map[string]any{"step": "5"}, false)
	if a.Run != b.Run {
		t.Fatal("second push should continue the run")
	}
	c.advance(time.Second)
	e.Push("deploy", map[string]any{"status": "done"}, false)
	d, _ := e.Push("deploy", map[string]any{"status": "running"}, false)
	if d.Run == a.Run {
		t.Fatal("push after done should start a new run")
	}
	e.Push("deploy", map[string]any{"step": "10"}, false)
	f, _ := e.Push("deploy", map[string]any{"step": "1"}, false)
	if f.Run == d.Run {
		t.Fatal("step going backwards should start a new run")
	}
	g, _ := e.Push("deploy", map[string]any{}, true)
	if g.Run == f.Run {
		t.Fatal("forceNew should start a new run")
	}
	if r, _ := e.Run(g.Run); r.Seq != 4 {
		t.Fatalf("seq = %d", r.Seq)
	}
	// A one-shot push of done notifies even though it took 0s.
	if got := n.titles(); len(got) != 1 || got[0] != "deploy finished" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestPushRunGoesIdleNotLost(t *testing.T) {
	e, n, c := newEngine(t)
	r, _ := e.Push("sync", map[string]any{"files": "3/10"}, false)
	for range 130 { // ~11 min
		c.advance(5 * time.Second)
		e.Tick()
	}
	if got, _ := e.Run(r.Run); got.Status != model.Idle {
		t.Fatalf("status = %s", got.Status)
	}
	if len(n.titles()) != 0 {
		t.Fatalf("idle must not notify: %v", n.titles())
	}
}

func TestNameCollision(t *testing.T) {
	e, _, _ := newEngine(t)
	meta := func(cmd, cwd string, inferred bool) model.RunMeta {
		return model.RunMeta{Source: "run", Cmd: cmd, Cwd: cwd, Inferred: inferred}
	}
	a, _ := e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: meta("python train.py", "/repo-a", true)})
	b, _ := e.ApplyBatch("r_2", model.Batch{Job: "train", Meta: meta("python train.py", "/repo-b", true)})
	c, _ := e.ApplyBatch("r_3", model.Batch{Job: "train", Meta: meta("python train.py --lr 1e-3", "/repo-a", true)})
	d, _ := e.ApplyBatch("r_4", model.Batch{Job: "train", Meta: meta("python other.py", "/repo-c", false)})
	if a.Job != "train" || b.Job != "train#2" || c.Job != "train" || d.Job != "train" {
		t.Fatalf("jobs = %s, %s, %s, %s (want train, train#2, train, train)", a.Job, b.Job, c.Job, d.Job)
	}
}

func TestPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "pulse.db"))
	e, _ := New(st, nil)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"loss": "nan", "epoch": "2/5"}}})
	st.Close()

	st2, _ := store.Open(filepath.Join(dir, "pulse.db"))
	defer st2.Close()
	e2, err := New(st2, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := e2.Snapshot()
	if len(snap) != 1 || snap[0].Job != "train" || snap[0].Progress[0].Cur != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestETAFromRecentRate(t *testing.T) {
	e, _, c := newEngine(t)
	for i := 0; i <= 10; i++ { // 1 step per second
		e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(),
			Events: []map[string]any{{"step": fmt.Sprintf("%d/100", i), "_ts": float64(c.t.Unix())}}})
		c.advance(time.Second)
	}
	r, _ := e.Run("r_1")
	p := r.Progress[0]
	if p.ETA == nil {
		t.Fatal("expected an ETA")
	}
	// 90 steps left at 1 step/s → ~90s after the last sample.
	if left := p.ETA.Sub(c.t.Add(-time.Second)); left < 85*time.Second || left > 95*time.Second {
		t.Fatalf("eta in %v", left)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "done"}}})
	if r, _ := e.Run("r_1"); r.Progress[0].ETA != nil {
		t.Fatal("finished run should have no ETA")
	}
}

func TestNeedsAttentionAndArchive(t *testing.T) {
	e, _, _ := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "failed"}}})
	e.ApplyBatch("r_2", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "running"}}})
	if e.NeedsAttention() != 1 {
		t.Fatalf("needs attention = %d", e.NeedsAttention())
	}
	// The failed run stays visible even though a newer run of the job exists.
	if snap := e.Snapshot(); len(snap) != 2 {
		t.Fatalf("snapshot has %d runs", len(snap))
	}
	e.MarkSeen("r_1")
	if e.NeedsAttention() != 0 || len(e.Snapshot()) != 1 {
		t.Fatalf("after seen: attention=%d snapshot=%d", e.NeedsAttention(), len(e.Snapshot()))
	}
	e.ApplyBatch("r_2", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "done"}}})
	e.Archive("r_2")
	if len(e.Snapshot()) != 0 {
		t.Fatalf("archived run still shown: %+v", e.Snapshot())
	}
}

func TestChartsAndSeries(t *testing.T) {
	e, _, c := newEngine(t)
	var evs []map[string]any
	for i := 1; i <= 50; i++ {
		ev := map[string]any{"step": i, "loss": 1.0 / float64(i), "lr": "0.001"}
		if i%10 == 0 {
			ev["val_loss"] = 1.2 / float64(i)
		}
		if i == 25 {
			ev["loss"] = "nan"
		}
		evs = append(evs, ev)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: evs})
	r, _ := e.Run("r_1")
	if len(r.Charts) != 2 || r.Charts[0].Name != "loss" || len(r.Charts[0].Keys) != 2 || r.Charts[1].Name != "lr" {
		t.Fatalf("charts = %+v", r.Charts)
	}
	set, err := e.Series("r_1", []string{"loss", "val_loss"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if set.XKind != "step" || len(set.Series) != 2 {
		t.Fatalf("set = %+v", set)
	}
	loss := set.Series[0]
	if loss.NonFinite != 1 || len(loss.X) != 20 || loss.X[0] != 1 || loss.X[19] != 50 {
		t.Fatalf("loss: n=%d nonfinite=%d first=%v last=%v", len(loss.X), loss.NonFinite, loss.X[0], loss.X[len(loss.X)-1])
	}
	if vl := set.Series[1]; len(vl.X) != 5 || vl.X[0] != 10 {
		t.Fatalf("val_loss = %+v", vl)
	}
	_ = c
}

func TestSeriesWithoutStepUsesTime(t *testing.T) {
	e, _, c := newEngine(t)
	for i := 0; i < 3; i++ {
		e.Push("cpu", map[string]any{"util": i * 10}, false)
		c.advance(2 * time.Second)
	}
	r, _ := e.LatestRun("cpu")
	set, _ := e.Series(r, nil, 100)
	if set.XKind != "time" || len(set.Series) != 1 || set.Series[0].X[2] != 4 {
		t.Fatalf("set = %+v", set)
	}
}

func TestAlertsThroughEngine(t *testing.T) {
	e, n, c := newEngine(t)
	r, err := alert.Parse("val_loss < 0.5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddAlert("train", r); err != nil {
		t.Fatal(err)
	}
	stopRule, _ := alert.Parse("loss is nan")
	stopRule.Action = "stop"
	e.AddAlert("train", stopRule)

	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{
		{"step": 1, "val_loss": 0.9}, {"step": 2, "val_loss": 0.4},
	}})
	c.advance(time.Second)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"step": 3, "loss": "nan"}}})

	got := strings.Join(n.titles(), " | ")
	// Built-in NaN and the custom NaN rule match the same event: one
	// notification, and the custom rule's stop action runs.
	if got != "train: val_loss < 0.5 | train: loss is NaN" {
		t.Fatalf("notifications: %s", got)
	}
	run, _ := e.Run("r_1")
	if run.StopRequested == nil {
		t.Fatal("stop should have been requested by the alert")
	}
	cmds := e.WaitCommands(context.Background(), "r_1", 0)
	if len(cmds) != 1 || cmds[0].Name != "stop" {
		t.Fatalf("commands: %+v", cmds)
	}
}

func TestMuteSilencesNotifications(t *testing.T) {
	e, n, _ := newEngine(t)
	e.Mute("deploy", time.Hour)
	e.Push("deploy", map[string]any{"status": "failed"}, false)
	if len(n.titles()) != 0 {
		t.Fatalf("muted job notified: %v", n.titles())
	}
	e.Mute("deploy", 0)
	e.Push("deploy", map[string]any{"status": "running"}, false)
	e.Push("deploy", map[string]any{"status": "failed"}, false)
	if len(n.titles()) != 1 {
		t.Fatalf("unmuted job didn't notify: %v", n.titles())
	}
}

func TestStopRunAndCommandDelivery(t *testing.T) {
	e, n, _ := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "running"}}})

	got := make(chan []model.Command, 1)
	go func() { got <- e.WaitCommands(context.Background(), "r_1", 5*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	if res, err := e.Stop("r_1"); err != nil || res != "stopping" {
		t.Fatalf("stop: %v %v", res, err)
	}
	cmds := <-got // long-poll wakes up immediately
	if len(cmds) != 1 || cmds[0].Name != "stop" {
		t.Fatalf("cmds = %+v", cmds)
	}
	if again := e.WaitCommands(context.Background(), "r_1", 0); len(again) != 0 {
		t.Fatal("delivered command re-sent before the retry window")
	}
	if !e.AckCommand("r_1", cmds[0].ID) {
		t.Fatal("ack failed")
	}
	// The runner reports the outcome: stopped, no notification.
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "stopped"}}})
	if r, _ := e.Run("r_1"); r.Status != model.Stopped || r.StopRequested != nil || !r.Seen {
		t.Fatalf("run = %+v", r)
	}
	if len(n.titles()) != 0 {
		t.Fatalf("stopping notified: %v", n.titles())
	}
	if _, err := e.Stop("r_1"); err == nil {
		t.Fatal("stopping a finished run should fail")
	}
}

func TestStopPushRunMarksStopped(t *testing.T) {
	e, _, _ := newEngine(t)
	resp, _ := e.Push("deploy", map[string]any{"status": "running"}, false)
	if res, err := e.Stop(resp.Run); err != nil || res != "stopped" {
		t.Fatalf("stop push run: %v %v", res, err)
	}
	if r, _ := e.Run(resp.Run); r.Status != model.Stopped {
		t.Fatalf("status = %s", r.Status)
	}
}

func TestAlertConfigPersists(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "pulse.db"))
	e, _ := New(st, nil)
	r, _ := alert.Parse("val_loss new min")
	e.AddAlert("train", r)
	e.SetAlertEnabled("train", "stale", false)
	if err := e.RemoveAlert("train", "nan"); err == nil {
		t.Fatal("built-in rules can't be removed")
	}
	st.Close()

	st2, _ := store.Open(filepath.Join(dir, "pulse.db"))
	defer st2.Close()
	e2, _ := New(st2, nil)
	v := e2.Alerts("train")
	if len(v.Rules) != 4 || v.Rules[0].Enabled || v.Rules[3].Kind != alert.NewBest {
		t.Fatalf("rules = %+v", v.Rules)
	}
}

func TestPushFeedbackFixes(t *testing.T) {
	e, _, _ := newEngine(t)
	// Explicit progress= wins over other bars; text values become fields.
	a, _ := e.Push("upload", map[string]any{"step": "5/147", "video": "80%", "progress": "6/147", "current": "clip_042.mp4"}, false)
	r, _ := e.Run(a.Run)
	if r.Progress[0].Name != "main" || r.Progress[0].Cur != 6 {
		t.Fatalf("primary = %+v", r.Progress)
	}
	if len(r.Fields) != 1 || r.Fields[0].Value != "clip_042.mp4" {
		t.Fatalf("fields = %+v", r.Fields)
	}
	// --new closes the replaced run instead of leaving it running.
	b, _ := e.Push("upload", map[string]any{"progress": "1/10"}, true)
	old, _ := e.Run(a.Run)
	if old.Status != model.Stopped || old.Message != "Replaced by run #2" {
		t.Fatalf("old run = %s %q", old.Status, old.Message)
	}
	if n, err := e.DeleteJob("upload"); err != nil || n != 2 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, ok := e.Run(b.Run); ok {
		t.Fatal("run survived delete")
	}
	if runs, _ := e.JobRuns("upload", 10); len(runs) != 0 {
		t.Fatalf("store still has %d runs", len(runs))
	}
}

func TestReviewFixesM5(t *testing.T) {
	e, n, c := newEngine(t)

	// Job names are cleaned the same way on every entry point.
	resp, _ := e.Push("train (v2)", map[string]any{"status": "running"}, false)
	if err := e.AddAction("train (v2)", config.Action{Name: "gpu", Open: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.Run(resp.Run); len(r.Actions) != 1 {
		t.Fatalf("action not visible on the run: %+v", r.Actions)
	}

	// A file action replaces a CLI action of the same name.
	e.AddAction("deploy", config.Action{Name: "gpu", Shell: "echo cli"})
	e.ApplyConfig(config.Job{Name: "deploy", ActionList: []config.Action{{Name: "gpu", Open: "https://example.com"}}}, "/tmp/pulse.yaml", false)
	if acts := e.Actions("deploy"); len(acts) != 1 || acts[0].Source != "file" {
		t.Fatalf("actions = %+v", acts)
	}

	// Cooldowns accept the same forms as rules; negatives are refused.
	if _, err := e.ApplyConfig(config.Job{Name: "x", Alerts: []config.Alert{{Expr: "loss < 1", Cooldown: "10min"}}}, "/tmp/p.yaml", false); err != nil {
		t.Fatalf("10min: %v", err)
	}
	if _, err := e.ApplyConfig(config.Job{Name: "x", Alerts: []config.Alert{{Expr: "loss < 1", Cooldown: "-5m"}}}, "/tmp/p.yaml", false); err == nil {
		t.Fatal("negative cooldown accepted")
	}

	// send: only to jobs that listen for that command.
	e.AddAction("train", config.Action{Name: "ckpt", Send: "save_checkpoint"})
	meta := runMeta()
	meta.Accepts = []string{"stop"}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: meta, Events: []map[string]any{{"status": "running"}}})
	if _, err := e.RunAction(context.Background(), "r_1", "ckpt", "test"); err == nil || !strings.Contains(err.Error(), "isn't listening") {
		t.Fatalf("send to a non-listening job: %v", err)
	}
	if r, _ := e.Run("r_1"); r.Actions[0].Blocked == "" {
		t.Fatal("blocked send should be marked in the view")
	}

	// A shell action never runs in some other folder.
	e.AddAction("train", config.Action{Name: "clean", Shell: "ls"})
	gone := runMeta()
	gone.Cwd = "/definitely/not/here"
	e.ApplyBatch("r_2", model.Batch{Job: "train", Meta: gone})
	if _, err := e.RunAction(context.Background(), "r_2", "clean", "test"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("missing folder: %v", err)
	}

	// Views don't share mutable slices with the live run.
	e.Push("fields", map[string]any{"current": "a.mp4"}, false)
	id, _ := e.LatestRun("fields")
	v, _ := e.Run(id)
	v.Fields[0].Value = "changed"
	if again, _ := e.Run(id); again.Fields[0].Value != "a.mp4" {
		t.Fatal("view mutation leaked into the live run")
	}

	// The done notification honours ignore/stats ordering.
	e.ApplyConfig(config.Job{Name: "sum", Widgets: config.Display{Ignore: []string{"seed"}, Stats: []string{"acc"}}}, "/tmp/s.yaml", false)
	e.Push("sum", map[string]any{"seed": 1, "loss": 0.5, "acc": 0.9, "status": "running"}, false)
	c.advance(time.Minute)
	e.Push("sum", map[string]any{"status": "done"}, false)
	last := n.notes[len(n.notes)-1]
	if last.Body != "Accuracy 0.9 · Loss 0.5" {
		t.Fatalf("summary = %q", last.Body)
	}
}

func TestEmptyValueRemovesKey(t *testing.T) {
	e, _, _ := newEngine(t)
	resp, _ := e.Push("upload", map[string]any{"videos": "6/147", "gb": 7.6, "current": "a.mp4"}, false)
	e.Push("upload", map[string]any{"videos": "", "gb": "", "current": "", "progress": "7/147"}, false)
	r, _ := e.Run(resp.Run)
	if len(r.Progress) != 1 || r.Progress[0].Name != "main" || len(r.Metrics) != 0 || len(r.Fields) != 0 {
		t.Fatalf("progress=%+v metrics=%+v fields=%+v", r.Progress, r.Metrics, r.Fields)
	}
}

func TestRetriedBatchIsAppliedOnce(t *testing.T) {
	e, _, _ := newEngine(t)
	batch := model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{
		{"_seq": 1.0, "step": 1, "loss": 0.9}, {"_seq": 2.0, "step": 2, "loss": 0.8},
	}}
	e.ApplyBatch("r_1", batch)
	e.ApplyBatch("r_1", batch) // the sender timed out and resent it
	r, _ := e.Run("r_1")
	set, _ := e.Series("r_1", []string{"loss"}, 100)
	if r.Updates != 2 || len(set.Series[0].X) != 2 {
		t.Fatalf("updates=%d points=%d", r.Updates, len(set.Series[0].X))
	}
}

func TestRepeatedFinalStatusIsNotANewRun(t *testing.T) {
	e, n, c := newEngine(t)
	a, _ := e.Push("deploy", map[string]any{"status": "running"}, false)
	c.advance(time.Second)
	e.Push("deploy", map[string]any{"status": "done"}, false)
	b, _ := e.Push("deploy", map[string]any{"status": "done"}, false) // a retry
	if a.Run != b.Run || len(n.titles()) != 1 {
		t.Fatalf("runs %s/%s, notifications %v", a.Run, b.Run, n.titles())
	}
}

func TestWakeGraceForPushRuns(t *testing.T) {
	e, _, c := newEngine(t)
	resp, _ := e.Push("sync", map[string]any{"progress": "3/10"}, false)
	c.advance(2 * time.Hour) // asleep
	e.Tick()
	c.advance(5 * time.Second)
	e.Tick()
	if r, _ := e.Run(resp.Run); r.Status != model.Running {
		t.Fatalf("idle right after wake: %s", r.Status)
	}
}

func TestUnicodeJobNamesAndSafeTruncation(t *testing.T) {
	if CleanJobName("データ学習") != "データ学習" || CleanJobName("café run") != "café run" {
		t.Fatalf("got %q %q", CleanJobName("データ学習"), CleanJobName("café run"))
	}
	if got := truncate(strings.Repeat("損", 300), 240); !utf8.ValidString(got) {
		t.Fatal("truncate split a character")
	}
}

func TestETAForSlowBars(t *testing.T) {
	e, _, c := newEngine(t)
	resp, _ := e.Push("slow", map[string]any{"progress": "8/147", "status": "running"}, false)
	for i := 0; i < 21; i++ { // a minute-by-minute watcher re-reporting the same value
		c.advance(time.Minute)
		e.Push("slow", map[string]any{"progress": "8/147", "gb": float64(i)}, false)
	}
	if r, _ := e.Run(resp.Run); r.Progress[0].ETA != nil {
		t.Fatal("no estimate is possible before the bar moves")
	}
	c.advance(9 * time.Minute) // the next video lands 30 minutes after the first report
	e.Push("slow", map[string]any{"progress": "9/147"}, false)
	r, _ := e.Run(resp.Run)
	if r.Progress[0].ETA == nil {
		t.Fatal("a slow bar should get an ETA after its second value")
	}
	// 138 left at 1 per 30 minutes ≈ 69h → beyond nothing; just check the order of magnitude.
	if left := r.Progress[0].ETA.Sub(c.t); left < 60*time.Hour || left > 80*time.Hour {
		t.Fatalf("eta in %v", left)
	}
	if r.Source != "push" {
		t.Fatalf("source = %s", r.Source)
	}
	e.PushFrom("slow", map[string]any{"progress": "10/147"}, false, "watch")
	if r, _ := e.Run(resp.Run); r.Source != "watch" {
		t.Fatalf("source after watcher = %s", r.Source)
	}
}

func TestHiddenRunReturnsWhenItFails(t *testing.T) {
	e, _, c := newEngine(t)
	resp, _ := e.Push("sync", map[string]any{"progress": "1/10"}, false)
	c.advance(11 * time.Minute)
	e.Tick() // → idle
	e.Archive(resp.Run)
	e.Push("sync", map[string]any{"status": "failed"}, false)
	if e.NeedsAttention() != 1 {
		t.Fatal("a hidden run that fails must show up in Needs you")
	}
	e.MarkSeen(resp.Run)
	e.Archive(resp.Run)
	if len(e.Snapshot()) != 0 || len(e.Snapshot(true)) != 1 {
		t.Fatalf("hidden: %d, with hidden: %d", len(e.Snapshot()), len(e.Snapshot(true)))
	}
}

func TestETAHugeTotalHasNoEstimate(t *testing.T) {
	var tr rateTracker
	t0 := time.Unix(0, 0)
	tr.add(t0, model.Progress{Cur: 0, Total: 1e11})
	if eta := tr.add(t0.Add(4*time.Second), model.Progress{Cur: 20, Total: 1e11}); eta != nil {
		t.Fatalf("eta = %v", eta)
	}
}

func TestNoETAOnFinishedOrStalledRuns(t *testing.T) {
	e, _, c := newEngine(t)
	for i := 1; i <= 5; i++ {
		e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"step": fmt.Sprintf("%d/100", i), "_ts": float64(c.t.Unix())}}})
		c.advance(time.Second)
	}
	if r, _ := e.Run("r_1"); r.Progress[0].ETA == nil {
		t.Fatal("expected an estimate while running")
	}
	c.advance(10 * time.Minute) // the bar stalls; heartbeats keep the run alive
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta()})
	e.Tick()
	if r, _ := e.Run("r_1"); r.Progress[0].ETA != nil {
		t.Fatal("a passed estimate should be dropped")
	}
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"status": "done"}}})
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"step": "6/100"}, {"step": "7/100"}}})
	if r, _ := e.Run("r_1"); r.Progress[0].ETA != nil || e.rates["r_1"] != nil {
		t.Fatal("finished runs get no estimate and no tracker")
	}
}

func TestSeriesAxisFallsBackToTimeForRestartingSteps(t *testing.T) {
	e, _, c := newEngine(t)
	var evs []map[string]any
	for epoch := 0; epoch < 2; epoch++ {
		for step := 1; step <= 5; step++ {
			evs = append(evs, map[string]any{"step": step, "loss": float64(step), "_ts": float64(c.t.Unix())})
			c.advance(time.Second)
		}
	}
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: evs})
	if set, _ := e.Series("r_1", []string{"loss"}, 100); set.XKind != "time" || len(set.Series[0].X) != 10 {
		t.Fatalf("set = %+v", set)
	}
}

func TestLogOnlyBatchNotifiesListeners(t *testing.T) {
	e, _, _ := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta()})
	changes := 0
	e.OnChange(func() { changes++ })
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Log: []string{"compiling foo.c"}})
	if r, _ := e.Run("r_1"); changes != 1 || r.LogLines != 1 {
		t.Fatalf("changes=%d log_lines=%d", changes, r.LogLines)
	}
}

func TestRunFallsBackToStore(t *testing.T) {
	e, _, _ := newEngine(t)
	e.ApplyBatch("r_old", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"status": "done"}}})
	e.mu.Lock()
	delete(e.runs, "r_old") // aged out of memory
	delete(e.latest, "t")
	e.mu.Unlock()
	if _, ok := e.Run("r_old"); !ok {
		t.Fatal("old run should load from the store")
	}
	if id, ok := e.LatestRun("t"); !ok || id != "r_old" {
		t.Fatalf("latest = %q %v", id, ok)
	}
	if _, ok := e.Run("../secret"); ok {
		t.Fatal("invalid id accepted")
	}
}

func TestKilledIsAFailureAndPushStopIsExplained(t *testing.T) {
	e, n, _ := newEngine(t)
	e.Push("oom", map[string]any{"status": "running"}, false)
	e.Push("oom", map[string]any{"status": "killed"}, false)
	if got := n.titles(); len(got) != 1 || got[0] != "oom failed" {
		t.Fatalf("killed: %v", got)
	}
	r, _ := alert.Parse("loss is nan")
	r.Action = "stop"
	e.AddAlert("pushy", r)
	e.Push("pushy", map[string]any{"status": "running", "loss": "nan"}, false)
	last := n.notes[len(n.notes)-1]
	if !strings.Contains(last.Body, "can't signal it") {
		t.Fatalf("body = %q", last.Body)
	}
}

func TestNoStuckAlertAfterSleep(t *testing.T) {
	e, n, c := newEngine(t)
	for i := 0; i < 6; i++ { // a job that prints every 30s
		e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"loss": float64(i), "_ts": float64(c.t.Unix())}}})
		c.advance(30 * time.Second)
	}
	c.advance(time.Hour) // the Mac sleeps
	e.Tick()             // first tick after wake
	c.advance(5 * time.Second)
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta()}) // heartbeat resumes, no output yet
	e.Tick()
	for _, title := range n.titles() {
		if strings.Contains(title, "stuck") {
			t.Fatalf("sleep counted as quiet time: %v", n.titles())
		}
	}
}

func TestPendingStopSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "pulse.db"))
	e, _ := New(st, nil)
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"status": "running"}}})
	e.Stop("r_1")
	st.Close() // the app restarts before the runner picked up the command

	st2, _ := store.Open(filepath.Join(dir, "pulse.db"))
	defer st2.Close()
	e2, _ := New(st2, nil)
	cmds := e2.WaitCommands(context.Background(), "r_1", 0, "stop")
	if len(cmds) != 1 || cmds[0].Name != "stop" {
		t.Fatalf("stop lost across restart: %+v", cmds)
	}
}

func TestCronOutcomesAreNewRuns(t *testing.T) {
	e, n, c := newEngine(t)
	a, _ := e.Push("backup", map[string]any{"status": "failed"}, false)
	b, _ := e.Push("backup", map[string]any{"status": "failed"}, false) // immediate retry
	c.advance(time.Hour)
	d, _ := e.Push("backup", map[string]any{"status": "failed"}, false) // next cron run
	if a.Run != b.Run || d.Run == a.Run || len(n.titles()) != 2 {
		t.Fatalf("runs %s %s %s, notifications %v", a.Run, b.Run, d.Run, n.titles())
	}
}

func TestRetriedLogsAreStoredOnce(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Logs = logs.New(t.TempDir())
	batch := model.Batch{Job: "t", Sender: "s1", Meta: runMeta(), Log: []string{"a", "b"}}
	e.ApplyBatch("r_1", batch)
	e.ApplyBatch("r_1", batch) // retried after a timeout
	e.ApplyBatch("r_1", model.Batch{Job: "t", Sender: "s1", Meta: runMeta(), Log: []string{"c"}, LogFrom: 2})
	lines, _ := e.Logs.Tail("r_1", 10)
	if strings.Join(lines, ",") != "a,b,c" {
		t.Fatalf("log = %v", lines)
	}
}

func TestQuestionsGetThroughMuteAndSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "pulse.db"))
	n := &fakeNotifier{}
	e, _ := New(st, n)
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: runMeta(), Events: []map[string]any{{"status": "running"}}})
	e.Mute("t", time.Hour)
	a, err := e.Ask("r_1", "Ship?", []string{" yes ", "no"}, "", 0)
	if err != nil || a.Options[0] != "yes" {
		t.Fatalf("ask: %+v %v", a, err)
	}
	if len(n.titles()) != 1 {
		t.Fatalf("a question must get through mute: %v", n.titles())
	}
	if _, err := e.Answer(a.ID, "yes", "test"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st2, _ := store.Open(filepath.Join(dir, "pulse.db"))
	defer st2.Close()
	e2, _ := New(st2, nil)
	if got, ok := e2.GetAsk(a.ID); !ok || got.Answer != "yes" {
		t.Fatalf("answered question lost across restart: %+v %v", got, ok)
	}
}

func TestHelperFirstRunTakesRunnerDetails(t *testing.T) {
	e, _, _ := newEngine(t)
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: model.RunMeta{Source: "run", Helper: true}})
	e.ApplyBatch("r_1", model.Batch{Job: "t", Meta: model.RunMeta{Source: "run", Cmd: "python t.py", Heartbeat: true}})
	if r, _ := e.Run("r_1"); !r.Heartbeat || r.Cmd != "python t.py" {
		t.Fatalf("run = %+v", r)
	}
}

func TestAlertAsksAndAnswerActs(t *testing.T) {
	e, n, _ := newEngine(t)
	r, _ := alert.Parse("val_loss < 0.3")
	r.CooldownS = 0
	r.Ask = "Good enough. Stop early?"
	for _, s := range []string{"Stop now=stop", "Keep going"} {
		a, _ := alert.ParseAnswer(s)
		r.Answers = append(r.Answers, a)
	}
	if _, err := e.AddAlert("train", r); err != nil {
		t.Fatal(err)
	}
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"status": "running"}, {"val_loss": 0.5}, {"val_loss": 0.25}}})
	asks := e.OpenAsks()
	if len(asks) != 1 || asks[0].Question != "Good enough. Stop early?" || asks[0].Then["Stop now"] != "stop" {
		t.Fatalf("asks = %+v", asks)
	}
	if got := n.titles(); len(got) != 1 || got[0] != "train needs you" {
		t.Fatalf("notifications: %v", got)
	}
	// Crossing again while the question is open doesn't ask twice.
	e.ApplyBatch("r_1", model.Batch{Job: "train", Meta: runMeta(), Events: []map[string]any{{"val_loss": 0.5}, {"val_loss": 0.2}}})
	if len(e.OpenAsks()) != 1 {
		t.Fatal("asked twice")
	}
	e.Answer(asks[0].ID, "Stop now", "test")
	time.Sleep(50 * time.Millisecond) // the answer acts in the background
	if run, _ := e.Run("r_1"); run.StopRequested == nil {
		t.Fatal("answering 'Stop now' should stop the run")
	}
}

func TestLabelsAndUnits(t *testing.T) {
	e, _, _ := newEngine(t)
	resp, _ := e.Push("upload", map[string]any{"gb": 8.4, "clip_pct": 30, "gpu_util": 0.9}, false)
	e.SetLabels("upload", map[string]string{"gb": "Uploaded"}, map[string]string{"gb": "GB"})
	e.ApplyConfig(config.Job{Name: "upload", Widgets: config.Display{Labels: map[string]string{"gb": "Sent to S3"}}}, "/tmp/p.yaml", false)
	r, _ := e.Run(resp.Run)
	if format.LabelFor(&r, "gb") != "Sent to S3" || format.WithUnit(&r, "gb", 8.4) != "8.4 GB" {
		t.Fatalf("labels=%v units=%v", r.Labels, r.Units)
	}
	if format.LabelFor(&r, "gpu_util") != "GPU util" || format.LabelFor(&r, "clip_pct") != "Clip %" {
		t.Fatalf("humanised: %q %q", format.LabelFor(&r, "gpu_util"), format.LabelFor(&r, "clip_pct"))
	}
	e.SetLabels("upload", nil, map[string]string{"gb": ""})
	if r, _ := e.Run(resp.Run); r.Units["gb"] != "" {
		t.Fatal("empty value should remove the unit")
	}
}
