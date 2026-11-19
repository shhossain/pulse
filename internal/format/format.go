// Package format renders durations, numbers and progress for humans.
package format

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"pulse/internal/model"
)

// Duration renders durations like "12s", "4m 5s", "3h 12m".
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// Num renders a metric value compactly.
func Num(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "∞"
	case math.IsInf(f, -1):
		return "-∞"
	case f == math.Trunc(f) && math.Abs(f) < 1e9:
		return strconv.FormatInt(int64(f), 10)
	}
	if a := math.Abs(f); a >= 1e4 && a < 1e9 {
		return strconv.FormatFloat(f, 'f', 1, 64) // 12345.6, not 1.235e+04
	}
	return strconv.FormatFloat(f, 'g', 4, 64)
}

// Progress renders "epoch 3/10" or "62%".
func Progress(p model.ProgressItem) string {
	if p.Total == 100 && p.Name == "main" {
		return fmt.Sprintf("%d%%", int(math.Round(p.Frac()*100)))
	}
	label := ""
	if p.Name != "main" {
		label = p.Name + " "
	}
	return fmt.Sprintf("%s%s/%s", label, Num(p.Cur), Num(p.Total))
}

// Summary is a one-line digest for people (notifications): up to 3
// metrics by their display names, else the primary progress.
func Summary(r *model.Run) string { return summary(r, true) }

// KeySummary is Summary with the metrics' own keys, for the CLI, where
// they're what you type in alerts and read in --json.
func KeySummary(r *model.Run) string { return summary(r, false) }

func summary(r *model.Run, labelled bool) string {
	var parts []string
	for _, m := range r.OrderedMetrics() {
		if len(parts) == 3 {
			break
		}
		if model.IsSystemKey(m.Name) && !slices.Contains(r.Pinned, m.Name) {
			continue // the job's own numbers first; CPU and such have their own row
		}
		if labelled {
			parts = append(parts, LabelFor(r, m.Name)+" "+WithUnit(r, m.Name, float64(m.Value)))
		} else {
			parts = append(parts, m.Name+" "+Num(float64(m.Value)))
		}
	}
	if len(parts) == 0 {
		if p, ok := r.Primary(); ok {
			parts = append(parts, Progress(p))
		}
	}
	return strings.Join(parts, " · ")
}

// words for common abbreviations in metric names; keep in sync with
// web/src/lib/format.js.
var words = map[string]string{
	"lr": "learning rate", "acc": "accuracy", "gpu": "GPU", "cpu": "CPU", "mem": "memory", "pct": "%",
	"gb": "GB", "mb": "MB", "kb": "KB", "tb": "TB", "ms": "ms", "eta": "ETA", "id": "ID", "io": "I/O",
	"f1": "F1", "auc": "AUC", "mse": "MSE", "mae": "MAE", "rmse": "RMSE", "bleu": "BLEU", "rouge": "ROUGE",
	"wer": "WER", "cer": "CER", "ppl": "perplexity", "tps": "tokens/s", "qps": "queries/s", "num": "#",
	"avg": "avg", "std": "std", "max": "max", "min": "min", "url": "URL", "api": "API", "ram": "RAM",
	"vram": "VRAM", "hbm": "HBM", "ttft": "TTFT", "kl": "KL", "vm": "VM", "aws": "AWS", "gcp": "GCP",
	"ssh": "SSH", "s3": "S3", "ec2": "EC2", "db": "DB", "ui": "UI",
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// Label is a key as people read it: "val_loss" → "Val loss",
// "gpu_util" → "GPU util", "clip_pct" → "Clip %", "lr" → "Learning rate".
func Label(key string) string {
	if key == "main" {
		return "Progress"
	}
	if s, ok := system[key]; ok {
		return s[0]
	}
	k := camelBoundary.ReplaceAllString(key, "$1 $2")
	parts := strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '.' || r == '-' || r == '/' || r == ' ' })
	for i, p := range parts {
		if w, ok := words[strings.ToLower(p)]; ok {
			parts[i] = w
		} else {
			parts[i] = strings.ToLower(p)
		}
	}
	out := strings.Join(parts, " ")
	if out == "" {
		return key
	}
	r := []rune(out)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

// System metrics (recorded by pulse run): names and units of their own.
var system = map[string][2]string{
	"sys.cpu": {"CPU", "%"}, "sys.mem": {"Memory", "GB"}, "sys.gpu": {"GPU", "%"},
	"sys.gpu_mem": {"GPU memory", "GB"}, "sys.disk_free": {"Free disk", "GB"},
}

// Unit is the built-in unit of a system metric ("" for others).
func Unit(key string) string { return system[key][1] }

// LabelFor is the run's name for key: a configured label, else Label(key).
func LabelFor(r *model.Run, key string) string {
	if l := r.Labels[key]; l != "" {
		return l
	}
	return Label(key)
}

// WithUnit formats a value with the key's unit, if one is set.
func WithUnit(r *model.Run, key string, v float64) string {
	s := Num(v)
	u := r.Units[key]
	if u == "" {
		u = system[key][1]
	}
	switch u {
	case "":
	case "%":
		s += "%"
	default:
		s += " " + u
	}
	return s
}
