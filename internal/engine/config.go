package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pulse/internal/actions"
	"pulse/internal/alert"
	"pulse/internal/audit"
	"pulse/internal/config"
	"pulse/internal/layout"
	"pulse/internal/model"
)

// ApplyResult reports what applying a config did.
type ApplyResult struct {
	Job       string          `json:"job"`
	Alerts    int             `json:"alerts"`
	Actions   int             `json:"actions"`
	Untrusted []config.Action `json:"untrusted,omitempty"` // shell actions waiting for trust
}

const maxChartSpecs = 40

// ApplyConfig loads a job's config from a file (one-way: the file is the
// source of truth). Shell actions are only stored once their exact set has
// been trusted; pass trust=true after the user agreed.
func (e *Engine) ApplyConfig(j config.Job, source string, trust bool) (ApplyResult, error) {
	name := CleanJobName(j.Name)
	if j.Name == "" {
		return ApplyResult{}, fmt.Errorf("the config needs a job name")
	}
	for _, m := range []map[string]string{j.Widgets.Labels, j.Widgets.Units} {
		if err := config.CheckLabels(m, false); err != nil {
			return ApplyResult{}, err
		}
	}
	if len(j.Widgets.Charts) > maxChartSpecs {
		return ApplyResult{}, fmt.Errorf("at most %d charts", maxChartSpecs)
	}
	for _, c := range j.Widgets.Charts {
		if err := c.Validate(); err != nil {
			return ApplyResult{}, err
		}
	}
	var rules []alert.Rule
	for _, a := range j.Alerts {
		r, err := alert.Parse(a.Expr)
		if err != nil {
			return ApplyResult{}, err
		}
		if a.Level != "" {
			r.Level = a.Level
		}
		r.Action, r.Once, r.Ask = a.Run, a.Once, strings.TrimSpace(a.Ask)
		for _, s := range a.Answers {
			ans, err := alert.ParseAnswer(s)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("alert %q: %w", a.Expr, err)
			}
			r.Answers = append(r.Answers, ans)
		}
		if r.Ask != "" && len(r.Answers) == 0 {
			r.Answers = alert.DefaultAnswers()
		}
		if a.Cooldown != "" {
			d, err := alert.ParseCooldown(a.Cooldown)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("alert %q: %w", a.Expr, err)
			}
			r.CooldownS = d.Seconds()
		}
		if err := r.Validate(); err != nil {
			return ApplyResult{}, fmt.Errorf("alert %q: %w", a.Expr, err)
		}
		id := a.Expr + "\x00" + a.Level + "\x00" + a.Run + "\x00" + a.Cooldown + "\x00" + strconv.FormatBool(a.Once)
		if r.Ask != "" { // rules without a question keep the IDs they had
			id += "\x00" + r.Ask + "\x00" + strings.Join(a.Answers, "\x00")
		}
		sum := sha256.Sum256([]byte(id))
		r.ID, r.FromFile = "f_"+hex.EncodeToString(sum[:4]), true
		rules = append(rules, r)
	}
	for _, a := range j.ActionList {
		if err := a.Validate(); err != nil {
			return ApplyResult{}, err
		}
	}

	e.mu.Lock()
	c := e.job(name)
	have := map[string]bool{}
	for _, a := range j.ActionList {
		have[a.Name] = true
	}
	for _, a := range c.Actions {
		if a.Source != "file" {
			have[a.Name] = true
		}
	}
	for _, r := range rules {
		if err := answerActionsExist(r, have); err != nil {
			e.mu.Unlock()
			return ApplyResult{}, err
		}
	}
	res := ApplyResult{Job: name, Alerts: len(rules)}

	// Alerts: replace the file's rules, keep ones added from the CLI/UI, and
	// keep the on/off state of file rules that didn't change.
	enabled := map[string]bool{}
	var kept []alert.Rule
	for _, r := range c.Alerts {
		if r.FromFile {
			enabled[r.ID] = r.Enabled
		} else {
			kept = append(kept, r)
		}
	}
	for i := range rules {
		if on, ok := enabled[rules[i].ID]; ok {
			rules[i].Enabled = on
		}
	}
	c.Alerts = append(rules, kept...)

	// Actions: shell commands need trust for this exact set (and folder).
	dir := filepath.Dir(source)
	hash := config.ShellHash(dir, j.ActionList)
	if trust && config.HasShell(j.ActionList) && c.TrustedHash != hash {
		c.TrustedHash = hash
		var cmds []string
		for _, a := range j.ActionList {
			if a.Shell != "" {
				cmds = append(cmds, a.Name+": "+audit.Redact(a.Shell))
			}
		}
		audit.Log("trust.granted", map[string]any{"job": name, "file": source, "shell_actions": cmds})
	}
	trusted := c.TrustedHash == hash
	// The file is the source of truth: a file action replaces a CLI action
	// of the same name.
	fileNames := map[string]bool{}
	for _, a := range j.ActionList {
		fileNames[a.Name] = true
	}
	var acts []config.Action
	for _, a := range c.Actions {
		if a.Source != "file" && !fileNames[a.Name] {
			acts = append(acts, a)
		}
	}
	for _, a := range j.ActionList {
		a.Source = "file"
		if a.Shell != "" && !trusted {
			res.Untrusted = append(res.Untrusted, a)
			continue
		}
		acts = append(acts, a)
		res.Actions++
	}
	c.Actions = acts
	c.Display, c.Link, c.Source = j.Widgets, j.Link, source
	err := e.saveJob(name)
	e.mu.Unlock()
	e.emit(nil, true)
	return res, err
}

// Actions lists a job's actions.
func (e *Engine) Actions(job string) []config.Action {
	job = CleanJobName(job)
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.jobs[job]; c != nil {
		return append([]config.Action(nil), c.Actions...)
	}
	return nil
}

// AddAction adds (or replaces) an action defined from the CLI.
func (e *Engine) AddAction(job string, a config.Action) error {
	job = CleanJobName(job)
	a.Source = "cli"
	if err := a.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	c := e.job(job)
	for i, x := range c.Actions {
		if x.Name == a.Name {
			if x.Source == "file" {
				e.mu.Unlock()
				return fmt.Errorf("%q is defined in %s; edit it there", a.Name, filepath.Base(c.Source))
			}
			c.Actions = append(c.Actions[:i], c.Actions[i+1:]...)
			break
		}
	}
	c.Actions = append(c.Actions, a)
	err := e.saveJob(job)
	e.mu.Unlock()
	if err == nil {
		audit.Log("action.added", map[string]any{"job": job, "action": a.Name, "kind": a.Kind(), "detail": audit.Redact(a.Shell + a.Send + a.Open)})
	}
	e.emit(nil, true)
	return err
}

// RemoveAction deletes a CLI-defined action.
func (e *Engine) RemoveAction(job, name string) error {
	job = CleanJobName(job)
	e.mu.Lock()
	c := e.job(job)
	for i, x := range c.Actions {
		if x.Name != name {
			continue
		}
		if x.Source == "file" {
			e.mu.Unlock()
			return fmt.Errorf("%q is defined in %s; remove it there", name, filepath.Base(c.Source))
		}
		c.Actions = append(c.Actions[:i], c.Actions[i+1:]...)
		err := e.saveJob(job)
		e.mu.Unlock()
		e.emit(nil, true)
		return err
	}
	e.mu.Unlock()
	return fmt.Errorf("no action %q on %s", name, job)
}

// ActionResult is what running an action produced.
type ActionResult struct {
	Action string          `json:"action"`
	Kind   string          `json:"kind"`
	Shell  *actions.Result `json:"shell,omitempty"`
	URL    string          `json:"url,omitempty"`
	Sent   bool            `json:"sent,omitempty"`
}

// RunAction runs a job action in the context of one run. by says who
// triggered it (ui, cli, …) for the audit log.
func (e *Engine) RunAction(ctx context.Context, runID, name, by string) (ActionResult, error) {
	e.mu.Lock()
	r := e.runs[runID]
	if r == nil {
		e.mu.Unlock()
		if stored, err := e.st.Run(runID); err == nil && stored != nil {
			r = stored
		} else {
			return ActionResult{}, fmt.Errorf("no such run")
		}
		e.mu.Lock()
	}
	c := e.jobs[r.Job]
	var act *config.Action
	if c != nil {
		for i := range c.Actions {
			if c.Actions[i].Name == name {
				a := c.Actions[i]
				act = &a
			}
		}
	}
	if act == nil {
		e.mu.Unlock()
		return ActionResult{}, fmt.Errorf("no action %q on %s", name, r.Job)
	}
	running := r.Status == model.Running || r.Status == model.Idle
	if (act.When == "running" && !running) || (act.When == "finished" && running) {
		e.mu.Unlock()
		return ActionResult{}, fmt.Errorf("%q is only available while the run is %s", name, act.When)
	}
	res := ActionResult{Action: name, Kind: act.Kind()}
	if act.Send != "" {
		if why := sendBlocked(r, act.Send); why != "" {
			e.mu.Unlock()
			return res, fmt.Errorf("can't send %q: %s", act.Send, why)
		}
		e.enqueue(r.ID, act.Send)
		res.Sent = true
	}
	dir := r.Cwd
	if c.Source != "" && act.Source == "file" {
		dir = filepath.Dir(c.Source)
	}
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	if act.Shell != "" {
		// Never fall back to some other folder: a relative command would run
		// in the wrong place. (A run reported from another machine has a
		// folder that doesn't exist here.)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			e.mu.Unlock()
			return res, fmt.Errorf("can't run %q: its folder %s doesn't exist on this Mac", name, dir)
		}
	}
	env := []string{"PULSE_JOB=" + r.Job, "PULSE_RUN=" + r.ID, "PULSE_RUN_STATUS=" + string(r.Status), "PULSE_RUN_CWD=" + r.Cwd}
	job, seq := r.Job, r.Seq
	e.mu.Unlock()

	switch {
	case act.Shell != "":
		out := actions.Shell(ctx, act.Shell, dir, env, 2*time.Minute)
		res.Shell = &out
		log.Printf("action %q on %s #%d by %s: exit %d in %dms", name, job, seq, by, out.ExitCode, out.Millis)
		audit.Log("action.shell", map[string]any{"job": job, "run": runID, "action": name, "command": audit.Redact(act.Shell),
			"dir": dir, "by": by, "exit": out.ExitCode, "timed_out": out.TimedOut})
	case act.Open != "":
		res.URL = act.Open
		log.Printf("action %q on %s #%d by %s: open %s", name, job, seq, by, act.Open)
	default:
		log.Printf("action %q on %s #%d by %s: sent %q", name, job, seq, by, act.Send)
	}
	return res, nil
}

// mergeLabels combines CLI-set names with pulse.yaml ones (the file wins).
func mergeLabels(cli, file map[string]string) map[string]string {
	if len(cli) == 0 && len(file) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range cli {
		out[k] = v
	}
	for k, v := range file {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// SetLabels changes display names and units set from the CLI. An empty
// value removes that key's name or unit.
func (e *Engine) SetLabels(job string, labels, units map[string]string) error {
	job = CleanJobName(job)
	// Check everything before changing anything.
	for _, m := range []map[string]string{labels, units} {
		if err := config.CheckLabels(m, true); err != nil {
			return err
		}
	}
	e.mu.Lock()
	c := e.job(job)
	apply := func(dst *map[string]string, src map[string]string) error {
		for k, v := range src {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if *dst == nil {
				*dst = map[string]string{}
			}
			if v == "" {
				delete(*dst, k)
			} else {
				(*dst)[k] = v
			}
		}
		return nil
	}
	err := apply(&c.Labels, labels)
	if err == nil {
		err = apply(&c.Units, units)
	}
	if err == nil {
		err = e.saveJob(job)
	}
	e.mu.Unlock()
	e.emit(nil, true)
	return err
}

// sendBlocked says why a command can't be sent to r ("" if it can).
func sendBlocked(r *model.Run, cmd string) string {
	if r.Status != model.Running && r.Status != model.Idle {
		return "the run isn't running"
	}
	for _, a := range r.Accepts {
		if a == cmd {
			return ""
		}
	}
	return "the job isn't listening for it (jobs receive commands through the Pulse helper library)"
}

// view returns a copy of r decorated with its job's display config and actions.
// Caller holds e.mu.
func (e *Engine) view(r *model.Run) model.Run {
	v := copyRun(r)
	c := e.jobs[r.Job]
	var d config.Display
	if c != nil {
		d = c.Display
		v.Link, v.ConfigSource = c.Link, c.Source
		v.Pinned, v.Hidden = d.Stats, d.Ignore
		for _, a := range c.Actions {
			info := model.ActionInfo{Name: a.Name, Kind: a.Kind(), Confirm: a.Confirm, When: a.When, Output: a.Output, Source: a.Source}
			info.Detail = a.Shell + a.Send + a.Open
			if a.Send != "" {
				info.Blocked = sendBlocked(r, a.Send)
			}
			v.Actions = append(v.Actions, info)
		}
	}
	v.Asks = e.openAsksLocked(r.ID)
	if c != nil {
		v.Labels, v.Units = mergeLabels(c.Labels, d.Labels), mergeLabels(c.Units, d.Units)
	}
	v.Charts = layout.ChartsFor(v.Metrics, d)
	v.Progress = layout.OrderProgress(v.Progress, d.Progress)
	return v
}
