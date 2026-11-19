// Package model holds the types shared by the Pulse CLI and app.
package model

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"time"
)

// Status is the lifecycle state of a run.
type Status string

const (
	Running Status = "running"
	Done    Status = "done"
	Failed  Status = "failed"
	Lost    Status = "lost"    // heartbeats stopped
	Idle    Status = "idle"    // push run with no recent updates
	Stopped Status = "stopped" // stopped by the user (or an alert action)
)

// Terminal reports whether the run has finished for good.
func (s Status) Terminal() bool { return s == Done || s == Failed || s == Stopped }

// Num is a float64 that survives JSON even when it is NaN or ±Inf
// (encoded as the strings "NaN", "Infinity", "-Infinity").
type Num float64

func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	switch {
	case math.IsNaN(f):
		return []byte(`"NaN"`), nil
	case math.IsInf(f, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(f, -1):
		return []byte(`"-Infinity"`), nil
	}
	return strconv.AppendFloat(nil, f, 'g', -1, 64), nil
}

func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// Progress is a current/total pair.
type Progress struct {
	Cur   float64 `json:"cur"`
	Total float64 `json:"total"`
}

// Frac returns progress as a fraction in [0,1].
func (p Progress) Frac() float64 {
	if p.Total <= 0 {
		return 0
	}
	return math.Max(0, math.Min(1, p.Cur/p.Total))
}

// Event is one normalized update for a run.
type Event struct {
	TS       time.Time
	Status   Status
	Step     *float64
	Progress map[string]Progress
	Metrics  map[string]float64
	Message  string
	ExitCode *int
	Text     map[string]string // other text values (current=clip_042.mp4)
	Unset    []string          // keys sent empty: remove them from the run
}

// RunMeta describes where a run comes from. Sent by `pulse run`.
type RunMeta struct {
	Source    string   `json:"source"` // run | push | watch
	Cmd       string   `json:"cmd,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Host      string   `json:"host,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Heartbeat bool     `json:"heartbeat,omitempty"` // sender promises heartbeats → lost detection
	Inferred  bool     `json:"inferred,omitempty"`  // job name was guessed from the command
	Accepts   []string `json:"accepts,omitempty"`   // commands the sender handles (e.g. "stop")
	Helper    bool     `json:"helper,omitempty"`    // sent by the Python helper
}

// Batch is the wire format for POST /api/runs/{id}/events.
// Events are raw flat maps; the app normalizes them. An empty batch is a heartbeat.
type Batch struct {
	Job     string           `json:"job"`
	Sender  string           `json:"sender,omitempty"` // who sends: sequence numbers are per sender
	Meta    RunMeta          `json:"meta"`
	Events  []map[string]any `json:"events"`
	Log     []string         `json:"log,omitempty"`      // output lines (progress-bar redraws excluded)
	LogFrom int64            `json:"log_from,omitempty"` // number of the first line in Log (per sender)
}

// Series is one metric's samples, ready to plot.
type Series struct {
	Key       string    `json:"key"`
	X         []float64 `json:"x"`
	Y         []float64 `json:"y"`
	NonFinite int       `json:"nonfinite,omitempty"` // NaN/Inf samples (not plotted)
	// Scatter only: X holds the x metric's values, and T the step or time
	// of each point (the run's order).
	T []float64 `json:"t,omitempty"`
}

// SeriesSet is the response of the series endpoint. XKind is "step" when
// every sample has a step, otherwise "time" (seconds since the run started).
type SeriesSet struct {
	Run    string   `json:"run"`
	XKind  string   `json:"x_kind"`
	XKey   string   `json:"x_key,omitempty"` // scatter: x is this metric (XKind is then T's axis)
	Series []Series `json:"series"`
}

// BatchResponse is returned by event and push endpoints.
type BatchResponse struct {
	Job string `json:"job"`
	Run string `json:"run"`
}

// ProgressItem is a named progress value, kept outer → inner.
type ProgressItem struct {
	Name string     `json:"name"`
	ETA  *time.Time `json:"eta,omitempty"` // estimated finish, from the recent rate
	Progress
}

// MetricItem is a named latest metric value, kept in first-seen order.
type MetricItem struct {
	Name  string `json:"name"`
	Value Num    `json:"value"`
	N     int    `json:"n"` // number of values received
}

// TextItem is a named text value reported by a job.
type TextItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SetField updates or appends a text field, preserving order.
func (r *Run) SetField(name, value string) {
	for i := range r.Fields {
		if r.Fields[i].Name == name {
			r.Fields[i].Value = value
			return
		}
	}
	if len(r.Fields) < maxItems {
		r.Fields = append(r.Fields, TextItem{Name: name, Value: value})
	}
}

// Remove drops a progress bar, metric and text field named name.
func (r *Run) Remove(name string) {
	p := r.Progress[:0]
	for _, x := range r.Progress {
		if x.Name != name {
			p = append(p, x)
		}
	}
	r.Progress = p
	m := r.Metrics[:0]
	for _, x := range r.Metrics {
		if x.Name != name {
			m = append(m, x)
		}
	}
	r.Metrics = m
	f := r.Fields[:0]
	for _, x := range r.Fields {
		if x.Name != name {
			f = append(f, x)
		}
	}
	r.Fields = f
}

// Chart is a group of related metrics drawn together (e.g. loss + val_loss).
type Chart struct {
	Name  string   `json:"name"`            // identifies the chart (unique per run)
	Title string   `json:"title,omitempty"` // set in pulse.yaml: shown as written
	Keys  []string `json:"keys"`
	X     string   `json:"x,omitempty"`     // scatter: plot Keys against this metric
	Style string   `json:"style,omitempty"` // line (default) | points | both
	Group string   `json:"group,omitempty"` // "system": the job's CPU, memory, GPU and disk
}

// Run is one execution of a job.
type Run struct {
	ID        string     `json:"id"`
	Job       string     `json:"job"`
	Seq       int        `json:"seq"`
	Status    Status     `json:"status"`
	Source    string     `json:"source"`
	Cmd       string     `json:"cmd,omitempty"`
	Cwd       string     `json:"cwd,omitempty"`
	Host      string     `json:"host,omitempty"`
	Heartbeat bool       `json:"heartbeat,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	LastBeat  time.Time  `json:"last_beat"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Step      *float64   `json:"step,omitempty"`
	Message   string     `json:"message,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`

	Progress []ProgressItem `json:"progress"`
	Metrics  []MetricItem   `json:"metrics"`
	Charts   []Chart        `json:"charts,omitempty"` // inferred; first is the headline
	Fields   []TextItem     `json:"fields,omitempty"` // latest text values, first-seen order
	Updates  int            `json:"updates"`
	// SysUpdates counts system samples (CPU, memory…), which aren't updates
	// from the job: they don't make a hung job look alive.
	SysUpdates int `json:"sys_updates,omitempty"`

	// Seen is false for a failed/lost run the user hasn't acknowledged yet
	// ("Needs you"). Archived runs are hidden from the popover.
	Seen     bool `json:"seen"`
	Archived bool `json:"archived,omitempty"`

	// ExplicitStatus is true once the job itself reported a status (so it is
	// expected to keep reporting). StopRequested is set while a stop is pending.
	ExplicitStatus bool             `json:"explicit_status,omitempty"`
	StopRequested  *time.Time       `json:"stop_requested,omitempty"`
	Accepts        []string         `json:"accepts,omitempty"`   // commands the job listens for
	LastSeqs       map[string]int64 `json:"last_seqs,omitempty"` // per sender: highest sequence applied
	LogNext        map[string]int64 `json:"log_next,omitempty"`  // per sender: next log line number expected
	Helper         bool             `json:"helper,omitempty"`    // the Python helper reports for this run
	LogLines       int              `json:"log_lines,omitempty"` // output lines received (UI refreshes the log tail on change)
	Asks           []Ask            `json:"asks,omitempty"`      // open questions (filled in views)

	// Filled in from the job's config when the run is viewed (not stored).
	Link         string            `json:"link,omitempty"`
	Actions      []ActionInfo      `json:"actions,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"` // display names (else humanised keys)
	Units        map[string]string `json:"units,omitempty"`
	Pinned       []string          `json:"pinned,omitempty"` // metrics shown first
	Hidden       []string          `json:"hidden,omitempty"` // metrics not shown
	ConfigSource string            `json:"config_source,omitempty"`
}

// ActionInfo describes an action the UI can offer for a run.
type ActionInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`   // shell | send | open
	Detail  string `json:"detail"` // the command, message or URL
	Confirm bool   `json:"confirm,omitempty"`
	When    string `json:"when,omitempty"`
	Output  bool   `json:"output,omitempty"`
	Source  string `json:"source,omitempty"`  // file | cli
	Blocked string `json:"blocked,omitempty"` // why it can't run on this run right now
}

// NeedsAttention reports whether the run failed or was lost and the user
// hasn't dismissed it, or a question is waiting for an answer.
func (r *Run) NeedsAttention() bool {
	for _, a := range r.Asks {
		if a.Open() {
			return true
		}
	}
	return (r.Status == Failed || r.Status == Lost) && !r.Seen && !r.Archived
}

// HasProgress reports whether the run has a bar called name.
func (r *Run) HasProgress(name string) bool {
	for _, p := range r.Progress {
		if p.Name == name {
			return true
		}
	}
	return false
}

// SetETA sets the estimated finish time of a progress bar (nil clears it).
func (r *Run) SetETA(name string, eta *time.Time) {
	for i := range r.Progress {
		if r.Progress[i].Name == name {
			r.Progress[i].ETA = eta
			return
		}
	}
}

const maxItems = 64

// progressRank orders progress bars outer → inner: an epoch counter is more
// useful as the headline than the batch bar inside it.
func progressRank(name string) int {
	switch name {
	case "main": // an explicit progress=… is always the headline
		return -1
	case "epoch", "stage", "phase", "round", "fold", "trial", "generation", "episode", "task", "job":
		return 0
	case "batch", "step", "iter", "item", "sample", "file", "chunk", "page", "shard":
		return 2
	}
	return 1
}

// SetProgress updates or inserts a progress value. Bars are kept outer →
// inner (see progressRank), then in first-seen order.
func (r *Run) SetProgress(name string, p Progress) {
	for i := range r.Progress {
		if r.Progress[i].Name == name {
			r.Progress[i].Progress = p
			return
		}
	}
	if len(r.Progress) >= maxItems {
		return
	}
	item := ProgressItem{Name: name, Progress: p}
	at := len(r.Progress)
	for i, existing := range r.Progress {
		if progressRank(name) < progressRank(existing.Name) {
			at = i
			break
		}
	}
	r.Progress = append(r.Progress[:at], append([]ProgressItem{item}, r.Progress[at:]...)...)
}

// SetMetric updates or appends a metric, preserving order.
func (r *Run) SetMetric(name string, v float64) {
	for i := range r.Metrics {
		if r.Metrics[i].Name == name {
			r.Metrics[i].Value = Num(v)
			r.Metrics[i].N++
			return
		}
	}
	if len(r.Metrics) < maxItems || IsSystemKey(name) { // system ones don't take the job's room
		r.Metrics = append(r.Metrics, MetricItem{Name: name, Value: Num(v), N: 1})
	}
}

// SystemKeys are the metrics pulse run records about the job's use of the
// machine (internal/sysstat), in display order.
var SystemKeys = []string{"sys.cpu", "sys.mem", "sys.gpu", "sys.gpu_mem", "sys.disk_free"}

// IsSystemKey reports whether key is one of SystemKeys (not just any "sys."
// key: a job may log its own, e.g. a flattened {"sys": {...}}).
func IsSystemKey(key string) bool {
	for _, k := range SystemKeys {
		if k == key {
			return true
		}
	}
	return false
}

// SystemOnly reports whether an event carries nothing but system samples.
func (ev Event) SystemOnly() bool {
	if len(ev.Metrics) == 0 || len(ev.Progress) > 0 || len(ev.Text) > 0 || len(ev.Unset) > 0 ||
		ev.Status != "" || ev.Message != "" || ev.ExitCode != nil || ev.Step != nil {
		return false
	}
	for k := range ev.Metrics {
		if !IsSystemKey(k) {
			return false
		}
	}
	return true
}

// OrderedMetrics returns metrics in display order: pinned (config "stats"),
// then charted ones in chart order, then the rest; hidden ones are left out.
func (r *Run) OrderedMetrics() []MetricItem {
	out := make([]MetricItem, 0, len(r.Metrics))
	used := map[string]bool{}
	for _, h := range r.Hidden {
		used[h] = true
	}
	var keys []string
	keys = append(keys, r.Pinned...)
	for _, c := range r.Charts {
		keys = append(keys, c.Keys...)
	}
	for _, k := range keys {
		for _, m := range r.Metrics {
			if m.Name == k && !used[k] {
				out = append(out, m)
				used[k] = true
			}
		}
	}
	for _, m := range r.Metrics {
		if !used[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

// Primary returns the first progress value, if any.
func (r *Run) Primary() (ProgressItem, bool) {
	if len(r.Progress) == 0 {
		return ProgressItem{}, false
	}
	return r.Progress[0], true
}

// Duration is how long the run has been going (or went).
func (r *Run) Duration(now time.Time) time.Duration {
	end := now
	if r.EndedAt != nil {
		end = *r.EndedAt
	}
	return end.Sub(r.StartedAt)
}

// Point is one sample of a metric series.
type Point struct {
	Run string
	Key string
	X   *float64 // step, if known
	TS  time.Time
	V   float64
}

// Notification is what the engine asks the UI to show.
type Notification struct {
	ID       string
	Level    string // info | success | warn | error
	Title    string
	Subtitle string
	Body     string
	Job      string
	RunID    string
	CanStop  bool // the run is still going and can be stopped from the notification

	AskID      string   // set for a question: the buttons answer it
	AskOptions []string // the answers offered (empty: a text reply)
}

// Ask is a question a job waits on (an approval).
type Ask struct {
	ID         string     `json:"id"`
	Run        string     `json:"run"`
	Job        string     `json:"job"`
	Question   string     `json:"question"`
	Options    []string   `json:"options,omitempty"` // empty: free-text answer
	Default    string     `json:"default,omitempty"` // answer used when it times out
	Created    time.Time  `json:"created"`
	Deadline   *time.Time `json:"deadline,omitempty"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
	AnsweredBy string     `json:"answered_by,omitempty"` // ui, notification, cli, timeout
	// Set when an alert asks: why it's asking, and what each answer does.
	Context string            `json:"context,omitempty"`
	Then    map[string]string `json:"then,omitempty"` // answer → stop | send:<cmd> | action:<name>
	Rule    string            `json:"rule,omitempty"`
}

// Open reports whether the ask still waits for an answer.
func (a Ask) Open() bool { return a.AnsweredAt == nil }

// Command is an instruction for a running job (delivered by long-poll).
type Command struct {
	ID   string `json:"id"`
	Name string `json:"name"` // "stop"
}

// NewRunID returns a fresh random run id like "r_3f9a1c2b7d4e".
func NewRunID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "r_" + hex.EncodeToString(b)
}
