// Package engine owns live run state: it applies events, detects lifecycle
// transitions (done, failed, lost, idle) and decides what to notify.
package engine

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pulse/internal/alert"
	"pulse/internal/format"
	"pulse/internal/logs"
	"pulse/internal/model"
	"pulse/internal/normalize"
	"pulse/internal/series"
	"pulse/internal/store"
)

// Notifier shows notifications to the user.
type Notifier interface{ Notify(model.Notification) }

// Engine is safe for concurrent use.
type Engine struct {
	// Tunables (set before Start).
	LostAfter     time.Duration // heartbeat runs with no beat for this long → lost
	IdleAfter     time.Duration // push runs with no update for this long → idle
	MinDoneNotify time.Duration // `pulse run` jobs shorter than this finish silently
	KeepFor       time.Duration // finished runs kept in memory this long
	Retention     time.Duration // finished runs deleted after this long (0 = keep forever)
	Logs          *logs.Store   // run output; nil disables log storage

	mu        sync.Mutex
	st        *store.Store
	notifier  Notifier
	runs      map[string]*model.Run
	latest    map[string]string // job → latest run id
	listeners []func()
	now       func() time.Time
	lastTick  time.Time
	lastWake  time.Time
	lastPrune time.Time
	rates     map[string]map[string]*rateTracker // run → progress name → tracker
	jobs      map[string]*JobConfig
	alertSt   map[string]*alert.State // run → alert memory
	cmds      map[string][]*pendingCmd
	cmdWake   map[string]chan struct{}
	tokens    map[string]*Token // hash → job token
	asks      map[string]*model.Ask
	askWake   map[string]chan struct{}
}

func New(st *store.Store, n Notifier) (*Engine, error) {
	e := &Engine{
		LostAfter:     35 * time.Second,
		IdleAfter:     10 * time.Minute,
		MinDoneNotify: 10 * time.Second,
		KeepFor:       48 * time.Hour,
		Retention:     30 * 24 * time.Hour,
		st:            st,
		notifier:      n,
		runs:          map[string]*model.Run{},
		latest:        map[string]string{},
		now:           time.Now,
		rates:         map[string]map[string]*rateTracker{},
		jobs:          map[string]*JobConfig{},
		alertSt:       map[string]*alert.State{},
		cmds:          map[string][]*pendingCmd{},
		cmdWake:       map[string]chan struct{}{},
		tokens:        map[string]*Token{},
		asks:          map[string]*model.Ask{},
		askWake:       map[string]chan struct{}{},
	}
	if err := e.loadJobs(); err != nil {
		return nil, err
	}
	if err := e.loadTokens(); err != nil {
		return nil, err
	}
	if err := e.loadAsks(); err != nil {
		return nil, err
	}
	now := e.now()
	runs, err := st.Recent(now.Add(-e.KeepFor))
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		// The app may have been down; give live senders a fresh grace period.
		r.LastBeat = now
		if r.StopRequested != nil && r.Status == model.Running {
			e.enqueue(r.ID, "stop") // the pending command was in memory; queue it again
		}
		e.runs[r.ID] = r
		if cur := e.runs[e.latest[r.Job]]; cur == nil || r.Seq > cur.Seq {
			e.latest[r.Job] = r.ID
		}
	}
	e.lastTick = now
	return e, nil
}

// OnChange registers a callback invoked (synchronously, must not block)
// after any state change.
func (e *Engine) OnChange(f func()) {
	e.mu.Lock()
	e.listeners = append(e.listeners, f)
	e.mu.Unlock()
}

// Start runs the background ticker until stop is closed.
func (e *Engine) Start(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				e.Tick()
			}
		}
	}()
}

var (
	runIDRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	jobNameRe = regexp.MustCompile(`[^\p{L}\p{N} _.:#/@+-]+`)
)

// ValidRunID reports whether id is an acceptable run id.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// NewRunID returns a fresh random run id.
func NewRunID() string { return model.NewRunID() }

// CleanJobName makes a job name safe for display and URLs.
func CleanJobName(s string) string {
	s = strings.TrimSpace(jobNameRe.ReplaceAllString(s, "-"))
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	if s == "" {
		s = "job"
	}
	return s
}

// ApplyBatch applies events (or a heartbeat, if empty) to run id.
func (e *Engine) ApplyBatch(id string, b model.Batch) (model.BatchResponse, error) {
	if !ValidRunID(id) {
		return model.BatchResponse{}, fmt.Errorf("invalid run id")
	}
	e.mu.Lock()
	now := e.now()
	r, created, err := e.getOrCreate(id, b.Job, b.Meta, now)
	if err != nil {
		e.mu.Unlock()
		return model.BatchResponse{}, err
	}
	r.LastBeat = now
	var notes []model.Notification
	changed := created || len(b.Events) > 0
	if !created && b.Meta.Source == "run" && !b.Meta.Helper && r.Cmd == "" {
		// The helper's first report beat the runner's: take the runner's
		// details (heartbeats, command, folder) now.
		r.Source, r.Cmd, r.Cwd, r.Host, r.Heartbeat = "run", b.Meta.Cmd, b.Meta.Cwd, b.Meta.Host, b.Meta.Heartbeat
		changed = true
	}
	for _, a := range b.Meta.Accepts { // senders announce what they can handle
		if !contains(r.Accepts, a) {
			r.Accepts = append(r.Accepts, a)
			changed = true
		}
	}
	if b.Meta.Helper && !r.Helper {
		// The helper reports metrics itself: tell `pulse run` to stop
		// parsing them from output, so nothing is counted twice.
		r.Helper = true
		changed = true
		if r.Source == "run" {
			e.enqueue(r.ID, "helper_attached")
		}
	}
	if r.Status == model.Lost || r.Status == model.Idle {
		notes = append(notes, e.transition(r, model.Running, now)...)
		changed = true
	}
	var points []model.Point
	for _, raw := range b.Events {
		// A retried batch (the sender timed out after we applied it) must not
		// be applied twice: skip events at or below the last sequence seen.
		if seq, ok := raw[normalize.KeySeq].(float64); ok {
			if r.LastSeqs == nil {
				r.LastSeqs = map[string]int64{}
			}
			if int64(seq) <= r.LastSeqs[b.Sender] {
				continue
			}
			r.LastSeqs[b.Sender] = int64(seq)
		}
		notes = append(notes, e.apply(r, normalize.Normalize(raw, now), now, &points)...)
	}
	if changed {
		e.save(r, points)
	}
	// Log lines are numbered per sender (LogFrom): a retried batch only
	// appends the lines not stored yet.
	logLines := b.Log
	if b.LogFrom > 0 || len(b.Log) > 0 {
		if r.LogNext == nil {
			r.LogNext = map[string]int64{}
		}
		if next := r.LogNext[b.Sender]; b.LogFrom < next {
			skip := min(int(next-b.LogFrom), len(logLines))
			logLines = logLines[skip:]
		}
		if end := b.LogFrom + int64(len(b.Log)); end > r.LogNext[b.Sender] {
			r.LogNext[b.Sender] = end
		}
	}
	if len(logLines) > 0 {
		// Output alone (no parsed values) still needs to reach open pages;
		// the counter is kept in memory and saved with the next real change.
		r.LogLines += len(logLines)
		changed = true
	}
	resp := model.BatchResponse{Job: r.Job, Run: r.ID}
	e.mu.Unlock()

	if e.Logs != nil && len(logLines) > 0 {
		if err := e.Logs.Append(id, logLines); err != nil {
			log.Printf("logs: %v", err)
		}
	}
	e.emit(notes, changed)
	return resp, nil
}

// Series returns downsampled samples (≤ points each) of the given metric
// keys of a run. With no keys, every metric of the run is returned.
func (e *Engine) Series(runID string, keys []string, points int) (model.SeriesSet, error) {
	set := model.SeriesSet{Run: runID, XKind: "step", Series: []model.Series{}}
	run, ok := e.Run(runID)
	if !ok {
		r, err := e.st.Run(runID)
		if err != nil || r == nil {
			return set, fmt.Errorf("no such run")
		}
		run = *r
	}
	if len(keys) == 0 {
		for _, m := range run.Metrics {
			if !model.IsSystemKey(m.Name) { // they'd put the job's steps on a time axis
				keys = append(keys, m.Name)
			}
		}
	}
	type raw struct {
		key string
		pts []store.SeriesPoint
	}
	var all []raw
	for _, k := range keys {
		pts, err := e.st.Series(runID, k)
		if err != nil {
			return set, err
		}
		// The step axis needs steps that only go up: a sample without a step,
		// or a counter that restarts each epoch (or repeats), would overlap
		// on a step axis. Plot against time then.
		prev := math.Inf(-1)
		for _, p := range pts {
			if p.X != p.X || p.X <= prev {
				set.XKind = "time"
			}
			prev = p.X
		}
		all = append(all, raw{k, pts})
	}
	start := run.StartedAt.UnixMilli()
	for _, r := range all {
		s := model.Series{Key: r.key, X: []float64{}, Y: []float64{}}
		for _, p := range r.pts {
			if p.V != p.V || p.V > maxFloat || p.V < -maxFloat {
				s.NonFinite++
				continue
			}
			x := p.X
			if set.XKind == "time" {
				x = float64(p.TS-start) / 1000
			}
			s.X = append(s.X, x)
			s.Y = append(s.Y, p.V)
		}
		s.X, s.Y = series.LTTB(s.X, s.Y, points)
		set.Series = append(set.Series, s)
	}
	return set, nil
}

const maxFloat = 1.7976931348623157e308

// JobRuns lists a job's runs, newest first (live state where available).
func (e *Engine) JobRuns(job string, limit int) ([]model.Run, error) {
	job = CleanJobName(job)
	runs, err := e.st.JobRuns(job, limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.Run, 0, len(runs))
	for _, r := range runs {
		if live, ok := e.Run(r.ID); ok {
			out = append(out, live)
		} else {
			e.mu.Lock()
			out = append(out, e.view(r))
			e.mu.Unlock()
		}
	}
	return out, nil
}

// JobRun returns one run of a job by its number.
func (e *Engine) JobRun(job string, seq int) (model.Run, bool) {
	job = CleanJobName(job)
	r, err := e.st.RunBySeq(job, seq)
	if err != nil || r == nil {
		return model.Run{}, false
	}
	if live, ok := e.Run(r.ID); ok {
		return live, true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.view(r), true
}

// CountRuns returns how many runs a job has.
func (e *Engine) CountRuns(job string) int {
	n, _ := e.st.CountRuns(CleanJobName(job))
	return n
}

// LatestRun returns the id of a job's most recent run.
func (e *Engine) LatestRun(job string) (string, bool) {
	job = CleanJobName(job)
	e.mu.Lock()
	id, ok := e.latest[job]
	e.mu.Unlock()
	if ok {
		return id, true
	}
	runs, err := e.st.JobRuns(job, 1) // idle for longer than the memory window
	if err != nil || len(runs) == 0 {
		return "", false
	}
	return runs[0].ID, true
}

// Push applies one event to a job's current run, starting a new run when
// needed (no run yet, last one finished/lost, forceNew, or step went backwards).
func (e *Engine) Push(job string, raw map[string]any, forceNew bool) (model.BatchResponse, error) {
	return e.PushFrom(job, raw, forceNew, "push")
}

// PushFrom is Push with the run's source named ("push" or "watch").
func (e *Engine) PushFrom(job string, raw map[string]any, forceNew bool, source string) (model.BatchResponse, error) {
	e.mu.Lock()
	now := e.now()
	ev := normalize.Normalize(raw, now)
	name := CleanJobName(job)
	r := e.runs[e.latest[name]]
	var notes []model.Notification
	if r != nil && !forceNew && r.Status.Terminal() && repeatsEnd(ev, r.Status) &&
		r.EndedAt != nil && now.Sub(*r.EndedAt) < 2*time.Minute {
		// "status=done" sent again right away (a retry): the run is already
		// done; don't start a new one or notify twice. Later it's a new run
		// (a cron job that reports only its outcome).
		resp := model.BatchResponse{Job: r.Job, Run: r.ID}
		e.mu.Unlock()
		return resp, nil
	}
	if r == nil || forceNew || r.Status.Terminal() || r.Status == model.Lost ||
		(ev.Step != nil && r.Step != nil && *ev.Step < *r.Step) {
		prev := r
		r = e.newRun(NewRunID(), name, model.RunMeta{Source: source}, now)
		// A push job has one current run: close the one being replaced so
		// it doesn't sit at "running" forever.
		if prev != nil && !prev.Heartbeat && (prev.Status == model.Running || prev.Status == model.Idle) {
			e.transition(prev, model.Stopped, now)
			prev.Message = fmt.Sprintf("Replaced by run #%d", r.Seq)
			e.save(prev, nil)
		}
	}
	if r.Status == model.Idle {
		notes = append(notes, e.transition(r, model.Running, now)...)
	}
	if !r.Heartbeat && r.Source != source {
		r.Source = source // e.g. a pushed job now fed by a watcher
	}
	var points []model.Point
	notes = append(notes, e.apply(r, ev, now, &points)...)
	e.save(r, points)
	resp := model.BatchResponse{Job: r.Job, Run: r.ID}
	e.mu.Unlock()

	e.emit(notes, true)
	return resp, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// repeatsEnd reports whether ev only restates a run's final status.
func repeatsEnd(ev model.Event, final model.Status) bool {
	return ev.Status == final && len(ev.Progress) == 0 && len(ev.Metrics) == 0 && len(ev.Text) == 0 &&
		len(ev.Unset) == 0 && ev.Step == nil && ev.Message == "" && ev.ExitCode == nil
}

// Tick handles time-based transitions (lost, idle) and memory pruning.
func (e *Engine) Tick() {
	e.mu.Lock()
	now := e.now()
	// A long gap between ticks means the Mac was asleep: don't blame
	// senders for missing heartbeats while we weren't looking.
	woke := now.Round(0).Sub(e.lastTick.Round(0)) > 30*time.Second
	e.lastTick = now
	if woke {
		e.lastWake = now
	}

	var notes []model.Notification
	changed := e.expireAsks(now)
	for id, r := range e.runs {
		// Time asleep doesn't count as silence: measure quiet from the wake.
		quietSince := r.UpdatedAt
		if e.lastWake.After(quietSince) {
			quietSince = e.lastWake
		}
		if (r.Status == model.Running || r.Status == model.Idle) && woke {
			if st := e.alertSt[id]; st != nil {
				st.Woke(now)
			}
		}
		if (r.Status == model.Running || r.Status == model.Idle) && !woke {
			// alertState creates the memory if the app restarted: a job that
			// was already stuck still gets "no update for …".
			if st := e.alertState(id); st != nil {
				rv := *r
				rv.UpdatedAt = quietSince
				if fired := e.fire(r, st.OnTick(e.rules(r.Job), &rv, now), now); len(fired) > 0 {
					notes = append(notes, fired...)
					e.save(r, nil)
					changed = true
				}
			}
		}
		if r.Status == model.Running {
			// An estimate that has passed while the bar stood still is no
			// longer an estimate: drop it rather than show "almost done".
			for i := range r.Progress {
				if eta := r.Progress[i].ETA; eta != nil && now.Sub(*eta) > time.Minute {
					r.Progress[i].ETA = nil
					changed = true
				}
			}
			switch {
			case woke:
				r.LastBeat = now
			case r.Heartbeat && now.Sub(r.LastBeat) > e.LostAfter:
				notes = append(notes, e.transition(r, model.Lost, now)...)
				e.save(r, nil)
				changed = true
			case !r.Heartbeat && now.Sub(quietSince) > e.IdleAfter:
				notes = append(notes, e.transition(r, model.Idle, now)...)
				e.save(r, nil)
				changed = true
			}
		}
		if r.Status != model.Running && now.Sub(r.UpdatedAt) > e.KeepFor && e.latest[r.Job] != id && !r.NeedsAttention() {
			delete(e.runs, id)
			delete(e.rates, id)
		}
	}
	if e.lastPrune.IsZero() {
		e.lastPrune = now.Add(-50 * time.Minute) // first prune ~10 minutes after start
	}
	prune := e.Retention > 0 && now.Sub(e.lastPrune) > time.Hour
	if prune {
		e.lastPrune = now
	}
	e.mu.Unlock()
	if prune {
		// Off the ticker: a long delete or VACUUM must not stall lost/idle checks.
		go e.Prune(now.Add(-e.Retention))
	}
	e.emit(notes, changed)
}

// Prune deletes finished runs last updated before cutoff (runs, series,
// questions and logs). The latest run of each job in memory is kept.
func (e *Engine) Prune(cutoff time.Time) int {
	ids, err := e.st.Prune(cutoff)
	if err != nil {
		log.Printf("retention: %v", err)
		return 0
	}
	e.mu.Lock()
	for _, id := range ids {
		if r := e.runs[id]; r != nil && e.latest[r.Job] == id {
			continue
		}
		delete(e.runs, id)
		delete(e.rates, id)
		delete(e.alertSt, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		if e.Logs != nil {
			e.Logs.Remove(id)
		}
	}
	if len(ids) > 0 {
		log.Printf("retention: deleted %d run(s) finished before %s", len(ids), cutoff.Format(time.DateOnly))
		if len(ids) > 100 {
			_ = e.st.Vacuum()
		}
	}
	return len(ids)
}

// JobNames lists every known job (for completion and listings).
func (e *Engine) JobNames() []string {
	names, err := e.st.JobNames()
	if err != nil {
		log.Printf("store: %v", err)
	}
	return names
}

// Snapshot returns copies of the runs worth showing: every running run plus
// the latest run of each job (hidden ones too with includeHidden). Running
// first, then most recently updated.
func (e *Engine) Snapshot(includeHidden ...bool) []model.Run {
	all := len(includeHidden) > 0 && includeHidden[0]
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []model.Run
	for id, r := range e.runs {
		show := r.Status == model.Running || r.NeedsAttention() || (e.latest[r.Job] == id && (all || !r.Archived))
		if show {
			out = append(out, e.view(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i].Status == model.Running, out[j].Status == model.Running
		if ri != rj {
			return ri
		}
		if ri {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// Run returns a copy of one run (from memory, else the store).
func (e *Engine) Run(id string) (model.Run, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.runs[id]
	if !ok {
		if !ValidRunID(id) {
			return model.Run{}, false
		}
		stored, err := e.st.Run(id)
		if err != nil || stored == nil {
			return model.Run{}, false
		}
		r = stored
	}
	return e.view(r), true
}

// MarkSeen acknowledges a failed/lost run (removes it from "Needs you").
func (e *Engine) MarkSeen(id string) bool { return e.update(id, func(r *model.Run) { r.Seen = true }) }

// Archive hides a finished run from the popover.
func (e *Engine) Archive(id string) bool {
	return e.update(id, func(r *model.Run) { r.Seen, r.Archived = true, true })
}

// NeedsAttention counts failed/lost runs the user hasn't acknowledged.
func (e *Engine) NeedsAttention() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range e.runs {
		if r.NeedsAttention() {
			n++
		}
	}
	return n
}

func (e *Engine) update(id string, f func(*model.Run)) bool {
	e.mu.Lock()
	r, ok := e.runs[id]
	if ok {
		f(r)
		e.save(r, nil)
	}
	e.mu.Unlock()
	if ok {
		e.emit(nil, true)
	}
	return ok
}

// --- internals (caller holds e.mu) ---

func (e *Engine) tracker(runID, name string) *rateTracker {
	m := e.rates[runID]
	if m == nil {
		m = map[string]*rateTracker{}
		e.rates[runID] = m
	}
	t := m[name]
	if t == nil {
		t = &rateTracker{}
		m[name] = t
	}
	return t
}

func (e *Engine) getOrCreate(id, job string, meta model.RunMeta, now time.Time) (*model.Run, bool, error) {
	if r := e.runs[id]; r != nil {
		return r, false, nil
	}
	r, err := e.st.Run(id)
	if err != nil {
		return nil, false, err
	}
	if r != nil {
		e.runs[id] = r
		return r, false, nil
	}
	name := e.resolveCollision(CleanJobName(job), meta)
	return e.newRun(id, name, meta, now), true, nil
}

// resolveCollision keeps two unrelated projects apart: when the job name was
// guessed (e.g. both are "train.py") and a run from another folder or machine
// is already running under it, the new one becomes name#2, #3… Explicit
// names (-n) and runs from the same folder (e.g. a sweep) share the job.
func (e *Engine) resolveCollision(name string, meta model.RunMeta) string {
	if !meta.Inferred {
		return name
	}
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s#%d", name, i)
		}
		lr := e.runs[e.latest[cand]]
		if lr == nil || lr.Status != model.Running || (lr.Cwd == meta.Cwd && lr.Host == meta.Host) {
			return cand
		}
	}
}

func (e *Engine) newRun(id, job string, meta model.RunMeta, now time.Time) *model.Run {
	seq := 0
	for _, r := range e.runs {
		if r.Job == job && r.Seq > seq {
			seq = r.Seq
		}
	}
	if s, err := e.st.MaxSeq(job); err == nil && s > seq {
		seq = s
	}
	if meta.Source == "" {
		meta.Source = "push"
	}
	r := &model.Run{
		ID: id, Job: job, Seq: seq + 1, Status: model.Running,
		Source: meta.Source, Cmd: meta.Cmd, Cwd: meta.Cwd, Host: meta.Host, Heartbeat: meta.Heartbeat,
		Accepts:   append([]string(nil), meta.Accepts...),
		StartedAt: now, UpdatedAt: now, LastBeat: now,
		Progress: []model.ProgressItem{}, Metrics: []model.MetricItem{}, Seen: true,
	}
	e.runs[id] = r
	e.latest[job] = id
	return r
}

func (e *Engine) apply(r *model.Run, ev model.Event, now time.Time, points *[]model.Point) []model.Notification {
	if r.Updates == 0 && ev.TS.Before(r.StartedAt) {
		r.StartedAt = ev.TS
	}
	for _, name := range ev.Unset {
		r.Remove(name)
		if m := e.rates[r.ID]; m != nil {
			delete(m, name)
		}
	}
	for _, name := range sortedKeys(ev.Progress) {
		p := ev.Progress[name]
		r.SetProgress(name, p)
		// Estimate only for running runs, and only for bars the run kept
		// (SetProgress caps them), so trackers stay bounded.
		if !r.Status.Terminal() && r.HasProgress(name) {
			r.SetETA(name, e.tracker(r.ID, name).add(ev.TS, p))
		}
	}
	if ev.Step != nil {
		s := *ev.Step
		r.Step = &s
	}
	for _, k := range sortedKeys(ev.Metrics) {
		v := ev.Metrics[k]
		r.SetMetric(k, v)
		var x *float64
		if r.Step != nil && !model.IsSystemKey(k) { // system samples run on their own clock
			s := *r.Step
			x = &s
		}
		*points = append(*points, model.Point{Run: r.ID, Key: k, X: x, TS: ev.TS, V: v})
	}
	for _, k := range sortedKeys(ev.Text) {
		r.SetField(k, ev.Text[k])
	}
	if ev.Message != "" {
		r.Message = ev.Message
	}
	if ev.ExitCode != nil {
		c := *ev.ExitCode
		r.ExitCode = &c
	}
	background := ev.SystemOnly()
	if background {
		// A sample of CPU and memory says the process exists, not that the
		// job is getting anywhere: "stuck" and "no update" must still fire.
		r.SysUpdates++
	} else {
		r.Updates++
		r.UpdatedAt = now
	}
	if ev.Status == model.Running {
		r.ExplicitStatus = true
	}
	notes := e.fire(r, e.alertState(r.ID).OnEvent(e.rules(r.Job), r, ev, now, background), now)
	if ev.Status != "" && ev.Status != r.Status {
		at := ev.TS
		if at.After(now) {
			at = now
		}
		notes = append(notes, e.transition(r, ev.Status, at)...)
	}
	return notes
}

func (e *Engine) transition(r *model.Run, to model.Status, at time.Time) []model.Notification {
	from := r.Status
	r.Status = to
	r.Seen = to != model.Failed && to != model.Lost
	if to == model.Failed || to == model.Lost || to == model.Running {
		r.Archived = false // something new happened: show it again
	}
	if to.Terminal() {
		t := at
		r.EndedAt = &t
	} else {
		r.EndedAt = nil
	}
	if to != model.Running {
		for i := range r.Progress {
			r.Progress[i].ETA = nil
		}
		delete(e.rates, r.ID)
	}
	if to.Terminal() || to == model.Lost {
		r.StopRequested = nil
		delete(e.cmds, r.ID)
		delete(e.alertSt, r.ID)
	}
	n := model.Notification{ID: fmt.Sprintf("%s-%s", r.ID, to), Job: r.Job, RunID: r.ID}
	switch to {
	case model.Done:
		if r.Source == "run" && r.Duration(at) < e.MinDoneNotify {
			return nil
		}
		n.Level = "success"
		n.Title = r.Job + " finished"
		n.Subtitle = "Done in " + format.Duration(r.Duration(at))
		v := e.view(r) // so pinned/charted metrics lead and ignored ones stay out
		n.Body = format.Summary(&v)
	case model.Failed:
		n.Level = "error"
		n.Title = r.Job + " failed"
		if r.ExitCode != nil {
			n.Subtitle = fmt.Sprintf("Exit code %d after %s", *r.ExitCode, format.Duration(r.Duration(at)))
		} else {
			n.Subtitle = "Failed after " + format.Duration(r.Duration(at))
		}
		n.Body = truncate(lastLines(r.Message, 2), 240) // the error is usually last
	case model.Lost:
		n.Level = "warn"
		n.Title = r.Job + " lost contact"
		n.Subtitle = fmt.Sprintf("No heartbeat for %s", format.Duration(e.LostAfter))
		n.Body = "The terminal may have closed, or the machine running it went to sleep."
	default:
		if from == model.Lost && to == model.Running {
			log.Printf("run %s (%s) recovered", r.ID, r.Job)
		}
		return nil
	}
	return []model.Notification{n}
}

func (e *Engine) save(r *model.Run, points []model.Point) {
	if err := e.st.Save(r, points); err != nil {
		log.Printf("store: save run %s: %v", r.ID, err)
	}
}

func (e *Engine) emit(notes []model.Notification, changed bool) {
	if e.notifier != nil && len(notes) > 0 {
		e.mu.Lock()
		now := e.now()
		var keep []model.Notification
		for _, n := range notes {
			// A question blocks the job until answered: it gets through mute.
			if !e.muted(n.Job, now) || n.AskID != "" {
				keep = append(keep, n)
			}
		}
		e.mu.Unlock()
		for _, n := range keep {
			e.notifier.Notify(n)
		}
	}
	if changed || len(notes) > 0 {
		e.mu.Lock()
		ls := append([]func(){}, e.listeners...)
		e.mu.Unlock()
		for _, f := range ls {
			f()
		}
	}
}

// copyRun deep-copies everything the engine mutates in place, so views can
// be encoded after the lock is released.
func copyRun(r *model.Run) model.Run {
	c := *r
	c.Progress = append([]model.ProgressItem{}, r.Progress...)
	c.Metrics = append([]model.MetricItem{}, r.Metrics...)
	c.Charts = append([]model.Chart(nil), r.Charts...)
	c.Fields = append([]model.TextItem(nil), r.Fields...)
	c.Accepts = append([]string(nil), r.Accepts...)
	c.LastSeqs, c.LogNext = nil, nil // internal bookkeeping
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
