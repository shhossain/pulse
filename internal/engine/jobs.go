package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"pulse/internal/alert"
	"pulse/internal/audit"
	"pulse/internal/config"
	"pulse/internal/model"
	"pulse/internal/paths"
	"pulse/internal/watch"
)

// JobConfig holds per-job settings.
type JobConfig struct {
	Alerts      []alert.Rule      `json:"alerts,omitempty"`   // custom rules (CLI/UI and pulse.yaml)
	Disabled    []string          `json:"disabled,omitempty"` // built-in rule ids turned off
	MutedUntil  *time.Time        `json:"muted_until,omitempty"`
	Display     config.Display    `json:"display,omitempty"`
	Link        string            `json:"link,omitempty"`
	Actions     []config.Action   `json:"actions,omitempty"`
	Source      string            `json:"source,omitempty"`       // pulse.yaml path, if any
	TrustedHash string            `json:"trusted_hash,omitempty"` // shell actions the user trusted
	Watch       *watch.Spec       `json:"watch,omitempty"`        // polled source, if any
	Labels      map[string]string `json:"labels,omitempty"`       // display names set with pulse label
	Units       map[string]string `json:"units,omitempty"`        // units set with pulse label
}

// SaveWatch stores (or with nil, clears) a job's watcher. Implements watch.Store.
func (e *Engine) SaveWatch(job string, s *watch.Spec) error {
	job = CleanJobName(job)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.job(job).Watch = s
	return e.saveJob(job)
}

// Watches returns every stored watcher. Implements watch.Store.
func (e *Engine) Watches() []watch.Spec {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []watch.Spec
	for _, c := range e.jobs {
		if c.Watch != nil {
			out = append(out, *c.Watch)
		}
	}
	return out
}

// AlertsView is a job's rules (built-in first) and mute state.
type AlertsView struct {
	Rules      []alert.Rule `json:"rules"`
	MutedUntil *time.Time   `json:"muted_until,omitempty"`
}

func (e *Engine) loadJobs() error {
	raw, err := e.st.Jobs()
	if err != nil {
		return err
	}
	for name, b := range raw {
		var c JobConfig
		if json.Unmarshal(b, &c) == nil {
			e.jobs[name] = &c
		}
	}
	return nil
}

// job returns (creating if needed) a job's config. Caller holds e.mu.
func (e *Engine) job(name string) *JobConfig {
	c := e.jobs[name]
	if c == nil {
		c = &JobConfig{}
		e.jobs[name] = c
	}
	return c
}

func (e *Engine) saveJob(name string) error {
	b, err := json.Marshal(e.job(name))
	if err != nil {
		return fmt.Errorf("save job %s: %w", name, err)
	}
	return e.st.SaveJob(name, b)
}

// rules returns the active rule list for a job. Caller holds e.mu.
func (e *Engine) rules(job string) []alert.Rule {
	c := e.jobs[job]
	out := alert.Defaults()
	if c == nil {
		return out
	}
	for i := range out {
		for _, d := range c.Disabled {
			if out[i].ID == d {
				out[i].Enabled = false
			}
		}
	}
	return append(out, c.Alerts...)
}

func (e *Engine) muted(job string, now time.Time) bool {
	c := e.jobs[job]
	return c != nil && c.MutedUntil != nil && now.Before(*c.MutedUntil)
}

// Alerts returns a job's rules and mute state.
func (e *Engine) Alerts(job string) AlertsView {
	job = CleanJobName(job)
	e.mu.Lock()
	defer e.mu.Unlock()
	v := AlertsView{Rules: e.rules(job)}
	if e.muted(job, e.now()) {
		v.MutedUntil = e.jobs[job].MutedUntil
	}
	return v
}

// AddAlert adds a custom rule to a job.
func (e *Engine) AddAlert(job string, r alert.Rule) (alert.Rule, error) {
	job = CleanJobName(job)
	if err := r.Validate(); err != nil {
		return r, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.job(job)
	have := map[string]bool{}
	for _, a := range c.Actions {
		have[a.Name] = true
	}
	if err := answerActionsExist(r, have); err != nil {
		return r, err
	}
	r.ID = "a_" + paths.RandomHex(4)
	r.Builtin = false
	c.Alerts = append(c.Alerts, r)
	return r, e.saveJob(job)
}

// SetAlertEnabled turns a built-in or custom rule on or off.
func (e *Engine) SetAlertEnabled(job, id string, on bool) error {
	job = CleanJobName(job)
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.job(job)
	for i := range c.Alerts {
		if c.Alerts[i].ID == id {
			c.Alerts[i].Enabled = on
			return e.saveJob(job)
		}
	}
	for _, d := range alert.Defaults() {
		if d.ID != id {
			continue
		}
		var keep []string
		for _, x := range c.Disabled {
			if x != id {
				keep = append(keep, x)
			}
		}
		if !on {
			keep = append(keep, id)
		}
		c.Disabled = keep
		return e.saveJob(job)
	}
	return fmt.Errorf("no alert %q on job %q", id, job)
}

// RemoveAlert deletes a custom rule (built-in rules can only be disabled).
func (e *Engine) RemoveAlert(job, id string) error {
	job = CleanJobName(job)
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.job(job)
	for i := range c.Alerts {
		if c.Alerts[i].ID == id {
			c.Alerts = append(c.Alerts[:i], c.Alerts[i+1:]...)
			return e.saveJob(job)
		}
	}
	for _, d := range alert.Defaults() {
		if d.ID == id {
			return fmt.Errorf("%q is built in; turn it off instead", id)
		}
	}
	return fmt.Errorf("no alert %q on job %q", id, job)
}

// Mute silences a job's notifications for d (d <= 0 unmutes).
func (e *Engine) Mute(job string, d time.Duration) error {
	job = CleanJobName(job)
	e.mu.Lock()
	c := e.job(job)
	if d <= 0 {
		c.MutedUntil = nil
	} else {
		t := e.now().Add(d)
		c.MutedUntil = &t
	}
	err := e.saveJob(job)
	e.mu.Unlock()
	e.emit(nil, true)
	return err
}

// --- alerts evaluation (caller holds e.mu) ---

func (e *Engine) alertState(runID string) *alert.State {
	st := e.alertSt[runID]
	if st == nil {
		st = alert.NewState()
		e.alertSt[runID] = st
	}
	return st
}

func (e *Engine) fire(r *model.Run, fired []alert.Fired, now time.Time) []model.Notification {
	// Two rules can describe the same event (built-in "any metric is NaN" and
	// a custom "loss is nan --run stop"): notify once, keeping the one that
	// does most: a question, then an action, then the job's own rule over a
	// built-in one.
	weight := func(r alert.Rule) int {
		w := 0
		if r.Ask != "" {
			w += 4
		}
		if r.Action != "" {
			w += 2
		}
		if !r.Builtin {
			w++
		}
		return w
	}
	byTitle := map[string]int{}
	var uniq []alert.Fired
	for _, f := range fired {
		if i, ok := byTitle[f.Title]; ok {
			if weight(f.Rule) > weight(uniq[i].Rule) {
				uniq[i] = f
			}
			continue
		}
		byTitle[f.Title] = len(uniq)
		uniq = append(uniq, f)
	}
	var notes []model.Notification
	for _, f := range uniq {
		if f.Rule.Ask != "" && (r.Status == model.Running || r.Status == model.Idle) {
			// This alert asks instead of just telling: one open question per
			// rule and run, answered from the notification or the popover.
			if e.openRuleAsk(r.ID, f.Rule.ID) {
				continue
			}
			ask := model.Ask{Question: f.Rule.Ask, Context: f.Title + " · " + f.Subtitle, Rule: f.Rule.ID, Then: map[string]string{}}
			for _, ans := range f.Rule.Answers {
				ask.Options = append(ask.Options, ans.Label)
				if ans.Do != "" {
					ask.Then[ans.Label] = ans.Do
				}
			}
			if _, n, err := e.askLocked(r, ask, 0); err == nil {
				notes = append(notes, n)
			} else {
				log.Printf("alert question: %v", err)
			}
			continue
		}
		n := model.Notification{
			ID: fmt.Sprintf("%s-%s", r.ID, f.Rule.ID), Level: f.Level, Title: f.Title, Subtitle: f.Subtitle,
			Job: r.Job, RunID: r.ID, CanStop: r.Status == model.Running,
		}
		if f.Rule.Action == "stop" && r.Status == model.Running {
			switch {
			case e.requestStop(r, now):
				n.Body = "Stopping it, as this alert asks."
				n.CanStop = false
			case !r.Heartbeat:
				n.Body = "Couldn't stop it: this job reports with pulse push, so Pulse can't signal it."
				n.CanStop = false
			}
		}
		log.Printf("alert %s on %s (%s): %s — %s", f.Rule.ID, r.Job, r.ID, f.Title, f.Subtitle)
		notes = append(notes, n)
	}
	return notes
}

// --- stop & commands ---

// Stop stops a running run. Runs started with `pulse run` (or the helper)
// get a stop command; other runs can't be signalled and are just marked
// stopped. It returns "stopping" or "stopped".
func (e *Engine) Stop(id string) (string, error) {
	e.mu.Lock()
	r := e.runs[id]
	if r == nil {
		e.mu.Unlock()
		return "", fmt.Errorf("no such run")
	}
	if r.Status != model.Running && r.Status != model.Idle {
		e.mu.Unlock()
		return "", fmt.Errorf("%s #%d isn't running (%s)", r.Job, r.Seq, r.Status)
	}
	now := e.now()
	result := "stopping"
	if r.Heartbeat {
		e.requestStop(r, now)
	} else {
		e.transition(r, model.Stopped, now)
		result = "stopped"
	}
	e.save(r, nil)
	job, seq := r.Job, r.Seq
	e.mu.Unlock()
	audit.Log("run.stop", map[string]any{"job": job, "seq": seq, "run": id, "result": result})
	e.emit(nil, true)
	return result, nil
}

// requestStop queues a stop command once (again after a minute, in case it
// was lost, e.g. across an app restart). Caller holds e.mu.
func (e *Engine) requestStop(r *model.Run, now time.Time) bool {
	if !r.Heartbeat || (r.StopRequested != nil && now.Sub(*r.StopRequested) < time.Minute) {
		return false
	}
	t := now
	r.StopRequested = &t
	e.enqueue(r.ID, "stop")
	return true
}

type pendingCmd struct {
	model.Command
	delivered time.Time
}

func (e *Engine) enqueue(runID, name string) {
	e.cmds[runID] = append(e.cmds[runID], &pendingCmd{Command: model.Command{ID: "c_" + paths.RandomHex(4), Name: name}})
	if ch := e.cmdWake[runID]; ch != nil {
		close(ch)
		delete(e.cmdWake, runID)
	}
}

// WaitCommands returns pending commands for a run, waiting up to wait for
// one to arrive. Unacknowledged commands are re-delivered after 20s.
// Only commands named in accepts are delivered (nil: all), so the runner and
// the helper polling the same run each get their own.
func (e *Engine) WaitCommands(ctx context.Context, runID string, wait time.Duration, accepts ...string) []model.Command {
	deadline := time.Now().Add(wait)
	for {
		e.mu.Lock()
		now := e.now()
		var out []model.Command
		for _, c := range e.cmds[runID] {
			if len(accepts) > 0 && !contains(accepts, c.Name) {
				continue
			}
			if c.delivered.IsZero() || now.Sub(c.delivered) > 20*time.Second {
				c.delivered = now
				out = append(out, c.Command)
			}
		}
		if len(out) > 0 {
			e.mu.Unlock()
			return out
		}
		ch := e.cmdWake[runID]
		if ch == nil {
			ch = make(chan struct{})
			e.cmdWake[runID] = ch
		}
		e.mu.Unlock()

		left := time.Until(deadline)
		if left <= 0 {
			return []model.Command{}
		}
		timer := time.NewTimer(left)
		select {
		case <-ch:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return []model.Command{}
		case <-timer.C:
			return []model.Command{}
		}
	}
}

// AckCommand marks a command as handled.
func (e *Engine) AckCommand(runID, cmdID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs := e.cmds[runID]
	for i, c := range cs {
		if c.ID == cmdID {
			e.cmds[runID] = append(cs[:i], cs[i+1:]...)
			return true
		}
	}
	return false
}

// DeleteJob removes a job and everything about it. Running `pulse run`
// processes keep running; if they report again, the job comes back.
func (e *Engine) DeleteJob(job string) (int, error) {
	job = CleanJobName(job)
	e.mu.Lock()
	ids, err := e.st.DeleteJob(job)
	if err != nil {
		e.mu.Unlock()
		return 0, err
	}
	for id, r := range e.runs {
		if r.Job == job {
			ids = append(ids, id)
			delete(e.runs, id)
			delete(e.rates, id)
			delete(e.alertSt, id)
			delete(e.cmds, id)
		}
	}
	delete(e.latest, job)
	delete(e.jobs, job)
	e.mu.Unlock()
	audit.Log("job.deleted", map[string]any{"job": job, "runs": len(ids)})
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] && e.Logs != nil {
			e.Logs.Remove(id)
		}
		seen[id] = true
	}
	e.emit(nil, true)
	return len(seen), nil
}
