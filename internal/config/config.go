// Package config reads pulse.yaml files. A file describes one job (top-level
// fields) or several (a `jobs:` list). Everything is optional; anything left
// out is inferred. The file is the source of truth: Pulse never writes it.
//
//	name: train-gpt
//	run: python train.py --cfg big.yaml
//	link: https://wandb.ai/me/runs/abc
//	widgets:
//	  progress: [epoch, batch]      # bar order
//	  charts:
//	    - [loss, val_loss]          # one chart, two lines
//	    - { y: acc, style: both }   # line | points | both
//	    - { x: lr, y: loss }        # scatter: loss against lr
//	  stats: [gpu_util]             # chips shown first
//	  primary: loss                 # chart on the popover card
//	  ignore: [seed]
//	alerts:
//	  - val_loss new min
//	  - loss is nan: { run: stop }
//	actions:
//	  gpu: { shell: ssh gpu1 nvidia-smi, output: true }
//	  checkpoint: { send: save_checkpoint, when: running }
//	  wandb: { open: https://wandb.ai/me/runs/abc }
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Names pulse looks for in a folder, in order.
var Names = []string{"pulse.yaml", "pulse.yml", ".pulse.yaml"}

// Display overrides how a job's values are shown.
type Display struct {
	Progress []string          `yaml:"progress" json:"progress,omitempty"`
	Charts   []ChartSpec       `yaml:"charts" json:"charts,omitempty"`
	Stats    []string          `yaml:"stats" json:"stats,omitempty"`
	Primary  string            `yaml:"primary" json:"primary,omitempty"`
	Ignore   []string          `yaml:"ignore" json:"ignore,omitempty"`
	Labels   map[string]string `yaml:"labels" json:"labels,omitempty"` // key → display name
	Units    map[string]string `yaml:"units" json:"units,omitempty"`   // key → unit (GB, %, s, …)
}

// Chart styles: a line, dots, or dots joined by a line.
const (
	StyleLine   = "line"
	StylePoints = "points"
	StyleBoth   = "both"
)

// ChartSpec places metrics on one chart. In YAML it's either a list of keys
// (`[loss, val_loss]`) or a map: `{ y: [loss], x: lr, style: points, name: … }`.
// Without x the chart is over steps (or time); with x it's a scatter of the
// y metrics against x, matched on the same step.
type ChartSpec struct {
	Name  string   `yaml:"name" json:"name,omitempty"`
	Y     []string `yaml:"y" json:"y"`
	X     string   `yaml:"x" json:"x,omitempty"`
	Style string   `yaml:"style" json:"style,omitempty"`
}

func (c *ChartSpec) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		return n.Decode(&c.Y)
	case yaml.ScalarNode:
		c.Y = []string{n.Value}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch k := n.Content[i].Value; k {
			case "name", "y", "x", "style":
			default:
				return fmt.Errorf("line %d: chart field %q: use y, x, style or name", n.Content[i].Line, k)
			}
		}
		var o struct {
			Name  string    `yaml:"name"`
			Y     yaml.Node `yaml:"y"`
			X     string    `yaml:"x"`
			Style string    `yaml:"style"`
		}
		if err := n.Decode(&o); err != nil {
			return err
		}
		c.Name, c.X, c.Style = o.Name, o.X, o.Style
		switch o.Y.Kind {
		case yaml.ScalarNode:
			c.Y = []string{o.Y.Value}
		case yaml.SequenceNode:
			if err := o.Y.Decode(&c.Y); err != nil {
				return err
			}
		case 0:
		default:
			return fmt.Errorf("line %d: y is a metric or a list of metrics", o.Y.Line)
		}
		return nil
	}
	return fmt.Errorf("line %d: a chart is a list of metrics or { y: …, x: …, style: … }", n.Line)
}

// UnmarshalJSON accepts the object form or a bare list of keys.
func (c *ChartSpec) UnmarshalJSON(b []byte) error {
	if t := strings.TrimSpace(string(b)); strings.HasPrefix(t, "[") {
		*c = ChartSpec{}
		return json.Unmarshal(b, &c.Y)
	}
	var p struct {
		Name  string          `json:"name"`
		Y     json.RawMessage `json:"y"`
		X     string          `json:"x"`
		Style string          `json:"style"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = ChartSpec{Name: p.Name, X: p.X, Style: p.Style}
	if len(p.Y) == 0 || string(p.Y) == "null" {
		return nil
	}
	var one string
	if json.Unmarshal(p.Y, &one) == nil {
		c.Y = []string{one}
		return nil
	}
	return json.Unmarshal(p.Y, &c.Y)
}

// Validate checks a chart spec (also used for specs arriving over the API).
func (c ChartSpec) Validate() error {
	if len(c.Y) == 0 {
		return errors.New("a chart needs at least one metric (y)")
	}
	if utf8.RuneCountInString(c.Name) > MaxLabel {
		return errors.New("chart name: at most 60 characters")
	}
	switch c.Style {
	case "", StyleLine, StylePoints, StyleBoth:
	default:
		return fmt.Errorf("chart style %q: use line, points or both", c.Style)
	}
	for _, k := range c.Y {
		if k == "" || utf8.RuneCountInString(k) > 200 {
			return errors.New("chart metric names must be 1–200 characters")
		}
		if k == c.X {
			return fmt.Errorf("chart: %q can't be both x and y", k)
		}
	}
	if utf8.RuneCountInString(c.X) > 200 {
		return errors.New("chart metric names must be 1–200 characters")
	}
	return nil
}

// MaxLabel is the longest display name or unit, in characters.
const MaxLabel = 60

// CheckLabels checks a key → display name (or unit) map. allowEmpty lets an
// empty value through (the CLI uses it to remove a name).
func CheckLabels(m map[string]string, allowEmpty bool) error {
	if len(m) > 500 {
		return errors.New("at most 500 labels or units")
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || utf8.RuneCountInString(k) > 200 {
			return fmt.Errorf("label key %q: 1–200 characters", k)
		}
		v = strings.TrimSpace(v)
		if (v == "" && !allowEmpty) || utf8.RuneCountInString(v) > MaxLabel {
			return fmt.Errorf("label or unit for %q: 1–%d characters", k, MaxLabel)
		}
	}
	return nil
}

// Alert is a rule plus options. In YAML it's either a string or a
// one-key map: `- loss is nan: { run: stop, level: error }`.
type Alert struct {
	Expr     string   `json:"expr"`
	Level    string   `yaml:"level" json:"level,omitempty"`
	Run      string   `yaml:"run" json:"run,omitempty"`
	Cooldown string   `yaml:"cooldown" json:"cooldown,omitempty"`
	Once     bool     `yaml:"once" json:"once,omitempty"`
	Ask      string   `yaml:"ask" json:"ask,omitempty"`         // ask this instead of just notifying
	Answers  []string `yaml:"answers" json:"answers,omitempty"` // "Label=stop|send:x|action:y|none"
}

func (a *Alert) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		a.Expr = n.Value
		return nil
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return fmt.Errorf("line %d: an alert is a rule, optionally followed by options: '- loss is nan: { run: stop }'", n.Line)
		}
		a.Expr = n.Content[0].Value
		type opts Alert
		var o opts
		if err := n.Content[1].Decode(&o); err != nil {
			return err
		}
		a.Level, a.Run, a.Cooldown, a.Once, a.Ask, a.Answers = o.Level, o.Run, o.Cooldown, o.Once, o.Ask, o.Answers
		return nil
	}
	return fmt.Errorf("line %d: alerts are strings like 'val_loss new min'", n.Line)
}

// Action is something the user can trigger from Pulse. Exactly one of
// Shell, Send or Open is set.
type Action struct {
	Name    string `yaml:"-" json:"name"`
	Shell   string `yaml:"shell" json:"shell,omitempty"`     // run on this Mac via your login shell
	Send    string `yaml:"send" json:"send,omitempty"`       // command delivered to the running job
	Open    string `yaml:"open" json:"open,omitempty"`       // URL to open
	Confirm bool   `yaml:"confirm" json:"confirm,omitempty"` // ask before running
	When    string `yaml:"when" json:"when,omitempty"`       // "", running, finished
	Output  bool   `yaml:"output" json:"output,omitempty"`   // show the command's output
	Source  string `yaml:"-" json:"source,omitempty"`        // "file" or "cli"
}

var actionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,40}$`)

// Validate checks an action definition.
func (a Action) Validate() error {
	if !actionNameRe.MatchString(a.Name) {
		return fmt.Errorf("action name %q: use letters, digits, - _ .", a.Name)
	}
	set := 0
	for _, v := range []string{a.Shell, a.Send, a.Open} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("action %q needs exactly one of shell, send or open", a.Name)
	}
	if a.Open != "" {
		u, err := url.Parse(a.Open)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("action %q: open must be an http(s) URL", a.Name)
		}
	}
	if a.Send != "" && !actionNameRe.MatchString(a.Send) {
		return fmt.Errorf("action %q: send must be a simple command name like save_checkpoint", a.Name)
	}
	switch a.When {
	case "", "running", "finished":
	default:
		return fmt.Errorf("action %q: when must be running or finished", a.Name)
	}
	return nil
}

// Kind is shell, send or open.
func (a Action) Kind() string {
	switch {
	case a.Shell != "":
		return "shell"
	case a.Send != "":
		return "send"
	}
	return "open"
}

// Job is one job's configuration.
type Job struct {
	Name    string            `yaml:"name" json:"name"`
	Run     string            `yaml:"run" json:"run,omitempty"`
	Link    string            `yaml:"link" json:"link,omitempty"`
	Widgets Display           `yaml:"widgets" json:"widgets"`
	Alerts  []Alert           `yaml:"alerts" json:"alerts,omitempty"`
	Actions map[string]Action `yaml:"actions" json:"-"`
	// ActionList is Actions in name order (what's sent over the API).
	ActionList []Action `yaml:"-" json:"actions,omitempty"`
}

// File is a parsed config file.
type File struct {
	Path string
	Jobs []Job
}

type rawFile struct {
	Job  `yaml:",inline"`
	Jobs []Job `yaml:"jobs"`
}

// Parse reads YAML config data.
func Parse(data []byte) (File, error) {
	var raw rawFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) { // empty file = empty config
		return File{}, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var f File
	if len(raw.Jobs) > 0 {
		top := raw.Job
		if top.Name != "" || top.Run != "" || top.Link != "" || len(top.Alerts) > 0 || len(top.Actions) > 0 ||
			len(top.Widgets.Charts)+len(top.Widgets.Progress)+len(top.Widgets.Stats)+len(top.Widgets.Ignore)+len(top.Widgets.Labels)+len(top.Widgets.Units) > 0 || top.Widgets.Primary != "" {
			return f, errors.New("use either top-level job fields or a jobs: list, not both (top-level fields would be ignored)")
		}
		f.Jobs = raw.Jobs
	} else {
		f.Jobs = []Job{raw.Job}
	}
	for i := range f.Jobs {
		j := &f.Jobs[i]
		if j.Link != "" {
			if u, err := url.Parse(j.Link); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return f, fmt.Errorf("job %q: link must be an http(s) URL", j.Name)
			}
		}
		names := make([]string, 0, len(j.Actions))
		for n := range j.Actions {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a := j.Actions[n]
			a.Name, a.Source = n, "file"
			if err := a.Validate(); err != nil {
				return f, err
			}
			j.ActionList = append(j.ActionList, a)
		}
		for _, c := range j.Widgets.Charts {
			if err := c.Validate(); err != nil {
				return f, fmt.Errorf("job %q: %w", j.Name, err)
			}
		}
		for _, m := range []map[string]string{j.Widgets.Labels, j.Widgets.Units} {
			if err := CheckLabels(m, false); err != nil {
				return f, fmt.Errorf("job %q: %w", j.Name, err)
			}
		}
		if len(f.Jobs) > 1 && j.Name == "" {
			return f, fmt.Errorf("every job in jobs: needs a name")
		}
	}
	return f, nil
}

// Load reads and parses a config file.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	f, err := Parse(data)
	if err != nil {
		return f, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	abs, _ := filepath.Abs(path)
	f.Path = abs
	return f, nil
}

// Find returns the config file in dir, if any.
func Find(dir string) (string, bool) {
	for _, n := range Names {
		p := filepath.Join(dir, n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// ShellHash fingerprints a job's shell actions (plus the folder they run
// in). Trust is granted to this exact set; any change asks again.
func ShellHash(dir string, actions []Action) string {
	h := sha256.New()
	fmt.Fprintf(h, "dir\x00%s\x00", dir)
	for _, a := range actions {
		if a.Shell != "" {
			fmt.Fprintf(h, "%s\x00%s\x00", a.Name, a.Shell)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// HasShell reports whether any action runs a shell command.
func HasShell(actions []Action) bool {
	for _, a := range actions {
		if a.Shell != "" {
			return true
		}
	}
	return false
}
