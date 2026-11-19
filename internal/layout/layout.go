// Package layout decides how a run's metrics are displayed: which become
// charts, how they're grouped, and which chart is the headline.
package layout

import (
	"fmt"
	"slices"
	"strings"

	"pulse/internal/config"
	"pulse/internal/model"
)

// MinUpdates is how many values a metric needs before it gets a chart.
const MinUpdates = 5

// MaxSeries per chart; larger groups are split.
const MaxSeries = 4

var splitPrefixes = []string{"train", "training", "val", "valid", "validation", "test", "eval", "dev"}

// Stem groups related metrics: "loss", "val_loss", "train/loss" and
// "loss_val" all have stem "loss".
func Stem(key string) string {
	k := strings.ToLower(key)
	for _, sep := range []string{"/", "."} {
		if i := strings.LastIndex(k, sep); i >= 0 {
			prefix := k[:i]
			if j := strings.LastIndexAny(prefix, "/."); j >= 0 {
				prefix = prefix[j+1:]
			}
			if isSplit(prefix) {
				return k[i+1:]
			}
		}
	}
	for _, p := range splitPrefixes {
		if rest, ok := strings.CutPrefix(k, p+"_"); ok && rest != "" {
			return rest
		}
		if rest, ok := strings.CutSuffix(k, "_"+p); ok && rest != "" {
			return rest
		}
	}
	return k
}

func isSplit(s string) bool {
	for _, p := range splitPrefixes {
		if s == p {
			return true
		}
	}
	return false
}

// Charts groups chartable metrics by stem, in first-seen order, with the
// loss-like chart first (it's the one shown on the popover card).
func Charts(metrics []model.MetricItem) []model.Chart {
	var charts []model.Chart
	index := map[string]int{}
	for _, m := range metrics {
		if m.N < MinUpdates {
			continue
		}
		stem := Stem(m.Name)
		i, ok := index[stem]
		for n := 2; ok && len(charts[i].Keys) >= MaxSeries; n++ { // full: "acc (2)", "acc (3)", …
			stem = fmt.Sprintf("%s (%d)", Stem(m.Name), n)
			i, ok = index[stem]
		}
		if !ok {
			charts = append(charts, model.Chart{Name: stem})
			i = len(charts) - 1
			index[stem] = i
		}
		charts[i].Keys = append(charts[i].Keys, m.Name)
	}
	for i := range charts {
		if len(charts[i].Keys) == 1 {
			charts[i].Name = charts[i].Keys[0] // "val_acc", not "acc"
		}
	}
	for i, c := range charts {
		if strings.Contains(strings.ToLower(c.Name), "loss") {
			charts = append([]model.Chart{c}, append(charts[:i:i], charts[i+1:]...)...)
			break
		}
	}
	return charts
}

// ChartsFor applies a job's display config: explicit chart groups first,
// then inferred charts for the remaining metrics; ignored metrics are left
// out; the chart containing Primary is moved to the front.
func ChartsFor(metrics []model.MetricItem, d config.Display) []model.Chart {
	skip, ignored := map[string]bool{}, map[string]bool{}
	for _, k := range d.Ignore {
		skip[k], ignored[k] = true, true
	}
	have := map[string]bool{}
	for _, m := range metrics {
		have[m.Name] = true
	}
	var charts []model.Chart
	for _, spec := range d.Charts {
		if spec.X != "" && (!have[spec.X] || ignored[spec.X]) {
			continue
		}
		var keys []string
		for _, k := range spec.Y {
			if have[k] && !ignored[k] && k != spec.X && len(keys) < MaxSeries && (spec.X != "" || !skip[k]) {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		name := Stem(keys[0])
		if len(keys) == 1 {
			name = keys[0]
		}
		if spec.X != "" {
			// A scatter is an extra view: its metrics keep their own charts.
			name += " vs " + spec.X
		} else {
			for _, k := range keys {
				skip[k] = true // explicit placement wins over inference
			}
		}
		if spec.Name != "" {
			name = spec.Name
		}
		charts = append(charts, model.Chart{Name: name, Title: spec.Name, Keys: keys, X: spec.X, Style: spec.Style})
	}
	var rest, system []model.MetricItem
	for _, m := range metrics {
		switch {
		case skip[m.Name]:
		case model.IsSystemKey(m.Name):
			system = append(system, m)
		default:
			rest = append(rest, m)
		}
	}
	inferred := Charts(rest)
	// The job's CPU, memory, GPU and disk: one chart each (their units
	// differ), after the job's own charts.
	slices.SortStableFunc(system, func(a, b model.MetricItem) int {
		return slices.Index(model.SystemKeys, a.Name) - slices.Index(model.SystemKeys, b.Name)
	})
	for _, m := range system {
		if m.N >= MinUpdates {
			inferred = append(inferred, model.Chart{Name: m.Name, Keys: []string{m.Name}, Group: "system"})
		}
	}
	if len(d.Charts) > 0 || d.Primary != "" {
		charts = append(charts, inferred...)
	} else {
		charts = inferred
	}
	// Names identify charts in the UI, so they must be unique.
	used := map[string]bool{}
	for i := range charts {
		base := charts[i].Name
		for n := 2; used[charts[i].Name]; n++ {
			charts[i].Name = fmt.Sprintf("%s (%d)", base, n)
		}
		used[charts[i].Name] = true
	}
	if d.Primary != "" {
		for i, c := range charts {
			// A key picks its own chart over a scatter that also plots it.
			if c.Name == d.Primary || (c.X == "" && slices.Contains(c.Keys, d.Primary)) {
				return append([]model.Chart{c}, append(charts[:i:i], charts[i+1:]...)...)
			}
		}
	}
	return charts
}

// OrderProgress sorts progress bars by the configured order (listed names
// first, in that order; others keep their place after them).
func OrderProgress(items []model.ProgressItem, order []string) []model.ProgressItem {
	if len(order) == 0 {
		return items
	}
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i
	}
	out := make([]model.ProgressItem, 0, len(items))
	for _, n := range order {
		for _, p := range items {
			if p.Name == n {
				out = append(out, p)
			}
		}
	}
	for _, p := range items {
		if _, ok := rank[p.Name]; !ok {
			out = append(out, p)
		}
	}
	return out
}
