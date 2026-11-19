package alert

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"pulse/internal/format"
	"pulse/internal/model"
)

// Fired is a rule that triggered, with the notification text.
type Fired struct {
	Rule     Rule
	Level    string
	Title    string
	Subtitle string
}

// State is one run's evaluation memory (crossings, cooldowns, bests).
type State struct {
	armed   map[string]bool      // rule id → condition was false, may fire again
	last    map[string]time.Time // rule id → last fired
	best    map[string]float64
	bestN   map[string]int
	nanSeen map[string]bool
	updates []time.Time // recent update times (for "stale")
	stale   bool        // stale already reported for this quiet spell
	progAt  time.Time   // when primary progress last moved
	progVal float64
	progSet bool
}

func NewState() *State {
	return &State{
		armed: map[string]bool{}, last: map[string]time.Time{},
		best: map[string]float64{}, bestN: map[string]int{}, nanSeen: map[string]bool{},
	}
}

func (st *State) isArmed(id string) bool {
	a, ok := st.armed[id]
	return !ok || a
}

// allow reports whether a rule may fire now (once/cooldown) and records it.
func (st *State) allow(r Rule, now time.Time) bool {
	if last, ok := st.last[r.ID]; ok {
		if r.Once || now.Sub(last) < r.Cooldown() {
			return false
		}
	}
	st.last[r.ID] = now
	return true
}

// crossing fires when cond becomes true (edge-triggered).
func (st *State) crossing(r Rule, cond bool, now time.Time) bool {
	if !cond {
		st.armed[r.ID] = true
		return false
	}
	if !st.isArmed(r.ID) {
		return false
	}
	st.armed[r.ID] = false
	return st.allow(r, now)
}

// OnEvent evaluates rules after ev has been applied to run.
// A background event (system samples) is checked against value rules but
// doesn't count as the job reporting: it leaves the rhythm and staleness alone.
func (st *State) OnEvent(rules []Rule, run *model.Run, ev model.Event, now time.Time, background bool) []Fired {
	// The rhythm uses when events happened (a batch carries several, each
	// with its own time), and counts each moment once.
	at := ev.TS
	if at.IsZero() || at.After(now) {
		at = now
	}
	if !background {
		if n := len(st.updates); n == 0 || at.After(st.updates[n-1]) {
			st.updates = append(st.updates, at)
		}
		if len(st.updates) > 21 {
			st.updates = st.updates[len(st.updates)-21:]
		}
		st.stale = false
	}
	if p, ok := run.Primary(); ok && (!st.progSet || p.Cur != st.progVal) {
		st.progAt, st.progVal, st.progSet = now, p.Cur, true
	}

	var out []Fired
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		switch r.Kind {
		case Threshold:
			v, ok := lookupMetric(ev.Metrics, r.Metric)
			if !ok {
				if p, okp := lookupProgress(ev.Progress, r.Metric); okp {
					v, ok = p.Cur, true
					if r.Percent {
						v = p.Frac() * 100
					}
				}
			} else if r.Percent && v >= 0 && v <= 1 && format.Unit(r.Metric) != "%" {
				v *= 100 // "acc >= 90%" against acc=0.93
			}
			if !ok || !finite(v) {
				continue
			}
			if st.crossing(r, Compare(v, r.Op, r.Value), now) {
				f := Fired{Rule: r, Level: r.Level,
					Title:    fmt.Sprintf("%s: %s %s %s", run.Job, r.Metric, r.Op, format.Num(r.Value)),
					Subtitle: fmt.Sprintf("%s is %s%s", r.Metric, format.Num(v), atStep(run))}
				if r.Title != "" {
					f.Title = run.Job + ": " + r.Title
					f.Subtitle = format.LabelFor(run, r.Metric) + ": " + format.WithUnit(run, r.Metric, v)
				}
				out = append(out, f)
			}
		case Progress:
			p, ok := run.Primary()
			if !ok || len(ev.Progress) == 0 {
				continue
			}
			pct := p.Frac() * 100
			if st.crossing(r, Compare(pct, r.Op, r.Value), now) {
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    fmt.Sprintf("%s is %d%% done", run.Job, int(math.Round(pct))),
					Subtitle: etaText(p, now)})
			}
		case NewBest:
			v, ok := lookupMetric(ev.Metrics, r.Metric)
			if !ok || !finite(v) {
				continue
			}
			st.bestN[r.ID]++
			old, seen := st.best[r.ID]
			better := !seen || (r.Best == "min" && v < old) || (r.Best == "max" && v > old)
			if !better {
				continue
			}
			st.best[r.ID] = v
			// Skip the first few values: every early value is a "new best".
			if seen && st.bestN[r.ID] >= 3 && st.allow(r, now) {
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    fmt.Sprintf("%s: new best %s", run.Job, r.Metric),
					Subtitle: fmt.Sprintf("%s (was %s)%s", format.Num(v), format.Num(old), atStep(run))})
			}
		case NaN:
			for _, k := range sortedKeys(ev.Metrics) {
				v := ev.Metrics[k]
				seen := r.ID + ":" + k
				if (r.Metric != "*" && !strings.EqualFold(r.Metric, k)) || finite(v) || st.nanSeen[seen] {
					continue
				}
				st.nanSeen[seen] = true
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    fmt.Sprintf("%s: %s is %s", run.Job, k, format.Num(v)),
					Subtitle: fmt.Sprintf("Became non-finite%s. Training may have diverged.", atStep(run))})
			}
		case NoUpdate:
			st.armed[r.ID] = true
		}
	}
	return out
}

// OnTick evaluates time-based rules for a running run.
func (st *State) OnTick(rules []Rule, run *model.Run, now time.Time) []Fired {
	// Idle is a push run that went quiet: exactly when "no update for …"
	// and "stuck" matter, so it's evaluated too.
	if run.Status != model.Running && run.Status != model.Idle {
		return nil
	}
	quiet := now.Sub(run.UpdatedAt)
	var out []Fired
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		switch r.Kind {
		case Stale:
			// Only for runs that promise to keep reporting (pulse run, or an
			// explicit status=running), and only once we know their rhythm.
			if st.stale || len(st.updates) < 5 || (!run.Heartbeat && !run.ExplicitStatus) {
				continue
			}
			usual := medianGap(st.updates)
			limit := max(2*time.Minute, 10*usual)
			if quiet > limit {
				st.stale = true
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    run.Job + " looks stuck",
					Subtitle: fmt.Sprintf("No updates for %s (usually every %s)", format.Duration(quiet), format.Duration(usual))})
			}
		case NoUpdate:
			if st.crossing(r, quiet > r.Dur(), now) {
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    fmt.Sprintf("%s hasn't reported in %s", run.Job, format.Duration(quiet)),
					Subtitle: "Last update at " + run.UpdatedAt.Local().Format("15:04:05")})
			}
		case NoProgress:
			p, ok := run.Primary()
			if !ok || !st.progSet {
				continue
			}
			if st.crossing(r, now.Sub(st.progAt) > r.Dur(), now) {
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    run.Job + " isn't making progress",
					Subtitle: fmt.Sprintf("%s stuck for %s", format.Progress(p), format.Duration(now.Sub(st.progAt)))})
			}
		case ETA:
			p, ok := run.Primary()
			if !ok || p.ETA == nil {
				continue
			}
			left := p.ETA.Sub(now)
			if left >= r.Dur()*6/5 { // hysteresis: re-arm only when clearly above
				st.armed[r.ID] = true
				continue
			}
			if left < r.Dur() && st.crossing(r, true, now) {
				out = append(out, Fired{Rule: r, Level: r.Level,
					Title:    run.Job + " is almost done",
					Subtitle: etaText(p, now)})
			}
		}
	}
	return out
}

// Woke tells the state the Mac slept until now: the quiet spell restarts.
func (st *State) Woke(now time.Time) {
	if st.progSet {
		st.progAt = now
	}
}

func lookupMetric(m map[string]float64, name string) (float64, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return 0, false
}

func lookupProgress(m map[string]model.Progress, name string) (model.Progress, bool) {
	if p, ok := m[name]; ok {
		return p, true
	}
	for k, p := range m {
		if strings.EqualFold(k, name) {
			return p, true
		}
	}
	return model.Progress{}, false
}

func etaText(p model.ProgressItem, now time.Time) string {
	if p.ETA == nil {
		return format.Progress(p)
	}
	return fmt.Sprintf("~%s left · %s", format.Duration(p.ETA.Sub(now)), format.Progress(p))
}

func atStep(run *model.Run) string {
	if run.Step == nil {
		return ""
	}
	return " at step " + format.Num(*run.Step)
}

func medianGap(ts []time.Time) time.Duration {
	if len(ts) < 2 {
		return 0
	}
	gaps := make([]time.Duration, 0, len(ts)-1)
	for i := 1; i < len(ts); i++ {
		gaps = append(gaps, ts[i].Sub(ts[i-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
