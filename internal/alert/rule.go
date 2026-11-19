// Package alert parses alert rules and evaluates them against runs.
//
// Rule grammar (one rule per string):
//
//	<metric> <op> <number>[%]     val_loss < 0.3, gpu_util < 0.2, epoch >= 5
//	progress <op> <number>%       progress >= 90%
//	<metric> new min|max|best     val_loss new min
//	<metric|any> is nan           loss is nan
//	eta < <duration>              eta < 5m
//	no update for <duration>      no update for 15m
//	no progress for <duration>    no progress for 10m
package alert

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is the type of condition a rule checks.
type Kind string

const (
	Threshold  Kind = "threshold"   // metric (or progress bar's current value) vs a number
	Progress   Kind = "progress"    // primary progress percentage vs a number
	NewBest    Kind = "new_best"    // a metric reaches a new min/max
	NaN        Kind = "nan"         // a metric becomes NaN/Inf
	ETA        Kind = "eta"         // estimated time left drops below a duration
	NoUpdate   Kind = "no_update"   // no update for a duration
	NoProgress Kind = "no_progress" // progress hasn't moved for a duration
	Stale      Kind = "stale"       // built-in: no update for much longer than usual
)

// Rule is one alert rule.
type Rule struct {
	ID        string  `json:"id"`
	Expr      string  `json:"expr"`
	Kind      Kind    `json:"kind"`
	Metric    string  `json:"metric,omitempty"`
	Op        string  `json:"op,omitempty"`
	Value     float64 `json:"value,omitempty"`
	DurS      float64 `json:"dur_s,omitempty"`
	Best      string  `json:"best,omitempty"` // min | max
	Level     string  `json:"level"`          // info | success | warn | error
	Action    string  `json:"action,omitempty"`
	CooldownS float64 `json:"cooldown_s,omitempty"`
	Once      bool    `json:"once,omitempty"`
	Percent   bool    `json:"percent,omitempty"` // "acc >= 90%": a 0–1 metric is compared as a percentage
	// Ask turns the alert into a question; each answer can do something.
	Ask      string   `json:"ask,omitempty"`
	Answers  []Answer `json:"answers,omitempty"`
	Builtin  bool     `json:"builtin,omitempty"`
	Title    string   `json:"title,omitempty"`     // built-ins: what the notification says instead of the rule
	FromFile bool     `json:"from_file,omitempty"` // defined in pulse.yaml
	Enabled  bool     `json:"enabled"`
}

// Dur is the rule's duration parameter.
func (r Rule) Dur() time.Duration { return time.Duration(r.DurS * float64(time.Second)) }

// Cooldown is the minimum time between two firings for the same run.
func (r Rule) Cooldown() time.Duration { return time.Duration(r.CooldownS * float64(time.Second)) }

// Defaults are the built-in rules every job gets (they can be disabled).
func Defaults() []Rule {
	return []Rule{
		{ID: "stale", Expr: "no updates for much longer than usual", Kind: Stale, Level: "warn", Builtin: true, Enabled: true},
		{ID: "nan", Expr: "any metric is NaN or Inf", Kind: NaN, Metric: "*", Level: "error", Builtin: true, Enabled: true},
		// pulse run reports free space where the job runs (sys.disk_free, GB):
		// a full disk is what kills long jobs at the checkpoint.
		{ID: "disk", Expr: "less than 2 GB free disk space", Kind: Threshold, Metric: "sys.disk_free", Op: "<", Value: 2,
			Level: "warn", CooldownS: (6 * time.Hour).Seconds(), Title: "disk almost full", Builtin: true, Enabled: true},
	}
}

var (
	reNoUpdate   = regexp.MustCompile(`(?i)^no\s+updates?\s+for\s+(.+)$`)
	reNoProgress = regexp.MustCompile(`(?i)^no\s+progress\s+for\s+(.+)$`)
	reNaN        = regexp.MustCompile(`(?i)^(\S+)\s+is\s+(nan|inf|nan\s+or\s+inf)$`)
	reBest       = regexp.MustCompile(`(?i)^(\S+)\s+new\s+(min|max|best|low|high)$`)
	reETA        = regexp.MustCompile(`(?i)^eta\s*(<=|<)\s*(.+)$`)
	reCompare    = regexp.MustCompile(`^(\S+?)\s*(>=|<=|==|!=|>|<)\s*([-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?)\s*(%)?$`)
)

// Parse turns a rule expression into a Rule (enabled, level "warn" unless
// the kind implies otherwise). It returns a friendly error for bad input.
func Parse(expr string) (Rule, error) {
	// Keywords are case-insensitive; metric names keep their case (they're
	// matched against what the job sends).
	e := strings.Join(strings.Fields(expr), " ")
	r := Rule{Expr: strings.TrimSpace(expr), Enabled: true, Level: "warn"}
	switch {
	case reNoUpdate.MatchString(e):
		d, err := ParseDuration(reNoUpdate.FindStringSubmatch(e)[1])
		if err != nil {
			return r, err
		}
		r.Kind, r.DurS = NoUpdate, d.Seconds()
	case reNoProgress.MatchString(e):
		d, err := ParseDuration(reNoProgress.FindStringSubmatch(e)[1])
		if err != nil {
			return r, err
		}
		r.Kind, r.DurS = NoProgress, d.Seconds()
	case reNaN.MatchString(e):
		m := reNaN.FindStringSubmatch(e)
		r.Kind, r.Metric, r.Level = NaN, m[1], "error"
		if strings.EqualFold(r.Metric, "any") {
			r.Metric = "*"
		}
	case reBest.MatchString(e):
		m := reBest.FindStringSubmatch(e)
		r.Kind, r.Metric, r.Level = NewBest, m[1], "info"
		switch strings.ToLower(m[2]) {
		case "min", "low":
			r.Best = "min"
		case "max", "high":
			r.Best = "max"
		default:
			r.Best = defaultBest(m[1])
		}
		r.CooldownS = (10 * time.Minute).Seconds()
	case reETA.MatchString(e):
		d, err := ParseDuration(reETA.FindStringSubmatch(e)[2])
		if err != nil {
			return r, err
		}
		r.Kind, r.DurS, r.Level = ETA, d.Seconds(), "info"
	case reCompare.MatchString(e):
		m := reCompare.FindStringSubmatch(e)
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return r, fmt.Errorf("%q isn't a usable number", m[3])
		}
		r.Metric, r.Op, r.Value, r.Percent = m[1], m[2], v, m[4] == "%"
		r.Kind = Threshold
		r.CooldownS = (10 * time.Minute).Seconds() // noisy metrics cross back and forth
		if strings.EqualFold(m[1], "progress") {
			r.Kind, r.Level = Progress, "info"
			if m[4] == "" && v <= 1 {
				r.Value = v * 100 // "progress >= 0.9" means 90%
			}
		}
	default:
		return r, fmt.Errorf("don't understand %q; try e.g. 'val_loss < 0.3', 'val_loss new min', 'loss is nan', 'progress >= 90%%', 'eta < 5m', 'no update for 15m'", expr)
	}
	return r, nil
}

// Answer is one button of a question an alert asks, and what it does:
// "" (nothing), "stop", "send:<command>" or "action:<name>".
type Answer struct {
	Label string `json:"label"`
	Do    string `json:"do,omitempty"`
}

// ParseAnswer reads "Label=do" (or just "Label", which does nothing).
func ParseAnswer(s string) (Answer, error) {
	label, do, _ := strings.Cut(s, "=")
	a := Answer{Label: strings.TrimSpace(label), Do: strings.TrimSpace(do)}
	if a.Do == "none" {
		a.Do = ""
	}
	return a, a.check()
}

var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,40}$`)

func (a Answer) check() error {
	if a.Label == "" || len([]rune(a.Label)) > 40 {
		return fmt.Errorf("answer labels must be 1–40 characters")
	}
	switch kind, arg, _ := strings.Cut(a.Do, ":"); {
	case a.Do == "", a.Do == "stop":
	case (kind == "send" || kind == "action") && commandName.MatchString(arg):
	default:
		return fmt.Errorf("answer %q: do must be stop, send:<command>, action:<name> or none", a.Label)
	}
	return nil
}

// DefaultAnswers are offered when a question rule lists none.
func DefaultAnswers() []Answer {
	return []Answer{{Label: "Stop", Do: "stop"}, {Label: "Keep going"}}
}

// Validate checks optional fields set from flags.
func (r Rule) Validate() error {
	if strings.TrimSpace(r.Ask) != r.Ask {
		return fmt.Errorf("the question has blank space around it")
	}
	if r.Ask == "" && len(r.Answers) > 0 {
		return fmt.Errorf("answers need a question (ask)")
	}
	if r.Ask != "" && r.Action != "" {
		return fmt.Errorf("an alert either asks or stops on its own; for a question, offer the stop as an answer (Stop=stop)")
	}
	if r.Ask != "" {
		if len([]rune(r.Ask)) > 300 {
			return fmt.Errorf("the question is too long (300 characters max)")
		}
		if len(r.Answers) == 0 || len(r.Answers) > 6 {
			return fmt.Errorf("a question needs 1–6 answers")
		}
		seen := map[string]bool{}
		for _, a := range r.Answers {
			if err := a.check(); err != nil {
				return err
			}
			if seen[a.Label] {
				return fmt.Errorf("answer %q is listed twice", a.Label)
			}
			seen[a.Label] = true
		}
	}
	switch r.Level {
	case "info", "success", "warn", "error":
	default:
		return fmt.Errorf("level must be info, success, warn or error")
	}
	switch r.Action {
	case "", "stop":
	default:
		return fmt.Errorf("unknown action %q (supported: stop)", r.Action)
	}
	return nil
}

// defaultBest guesses whether lower or higher is better for a metric name.
func defaultBest(name string) string {
	for _, s := range []string{"loss", "err", "perplexity", "ppl", "wer", "cer", "mse", "mae", "rmse", "latency", "time"} {
		if strings.Contains(name, s) {
			return "min"
		}
	}
	return "max"
}

var durUnits = strings.NewReplacer(
	"hours", "h", "hour", "h", "hrs", "h", "hr", "h",
	"days", "d", "day", "d",
	"minutes", "m", "minute", "m", "mins", "m", "min", "m",
	"seconds", "s", "second", "s", "secs", "s", "sec", "s",
)

// ParseDuration accepts Go durations (30s, 15m, 1h30m), words (15min, 2hours)
// and bare numbers, which mean minutes.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Minute)), nil
	}
	s = durUnits.Replace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok { // Go durations have no days
		if f, err := strconv.ParseFloat(n, 64); err == nil && f > 0 {
			return time.Duration(f * 24 * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q (use e.g. 30s, 15m, 2h)", s)
	}
	return d, nil
}

// Compare applies op to a and b.
func Compare(a float64, op string, b float64) bool {
	switch op {
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case "==":
		return a == b
	case "!=":
		return a != b
	}
	return false
}

// ParseCooldown is ParseDuration that also accepts "0" (no cooldown).
func ParseCooldown(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "0" {
		return 0, nil
	}
	d, err := ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad cooldown %q (use e.g. 30s, 10m)", s)
	}
	return d, nil
}
