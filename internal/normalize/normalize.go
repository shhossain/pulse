// Package normalize turns whatever a job sends (flat k=v, nested JSON,
// strings like "3/10" or "62%") into a model.Event.
package normalize

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pulse/internal/model"
)

// Reserved keys set by the CLI (not user data).
const (
	KeyTS       = "_ts"       // unix seconds (float) when the event happened
	KeyCoalesce = "_coalesce" // event may be merged with the next one
	KeySeq      = "_seq"      // per-run sequence number (makes retries idempotent)
)

var (
	// Whole words in the key (so "author" and "session_name" are fine).
	secretKeyRe = regexp.MustCompile(`(?i)(^|[_.\-])(secrets?|passwords?|passwd|pwd|tokens?|api_?keys?|auth|authorization|credentials?|private_?key|cookies?|session_?(id|key|token)|access_?key)($|[_.\-])`)
	ratioRe     = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)\s*(?:/|of)\s*(\d+(?:\.\d+)?)\s*$`)
	percentRe   = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)\s*%\s*$`)
)

// denyExact keys are never charted or turned into progress.
var denyExact = map[string]bool{
	"pid": true, "ppid": true, "port": true, "id": true, "uid": true, "gid": true,
	"code": true, "date": true, "time": true, "timestamp": true, "ts": true,
	"year": true, "month": true, "day": true, "hour": true, "minute": true, "second": true,
	"version": true, "line": true, "lineno": true,
}

var denySuffix = []string{"_id", "_code", "_port", "_pid", "_date", "_version"}

// Denied reports whether key should be ignored as a metric/progress.
func Denied(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndex(k, "."); i >= 0 {
		k = k[i+1:]
	}
	if denyExact[k] {
		return true
	}
	for _, s := range denySuffix {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

var statusSynonyms = map[string]model.Status{
	"running": model.Running, "run": model.Running, "started": model.Running, "start": model.Running,
	"in_progress": model.Running, "in-progress": model.Running, "active": model.Running,
	"busy": model.Running, "working": model.Running, "pending": model.Running, "queued": model.Running,
	"done": model.Done, "success": model.Done, "succeeded": model.Done, "successful": model.Done,
	"ok": model.Done, "complete": model.Done, "completed": model.Done, "finished": model.Done,
	"passed": model.Done, "pass": model.Done,
	"failed": model.Failed, "fail": model.Failed, "failure": model.Failed, "error": model.Failed,
	"errored": model.Failed, "crashed": model.Failed, "timeout": model.Failed, "timed_out": model.Failed,
	"killed": model.Failed, "aborted": model.Failed, // e.g. an OOM kill: that's a failure, not a choice
	"stopped": model.Stopped, "cancelled": model.Stopped, "canceled": model.Stopped, "interrupted": model.Stopped,
}

// ParseStatus maps common status words to a Status ("" if unknown).
func ParseStatus(s string) model.Status {
	return statusSynonyms[strings.ToLower(strings.TrimSpace(s))]
}

// Normalize converts a raw event into a model.Event.
func Normalize(raw map[string]any, now time.Time) model.Event {
	ev := model.Event{
		TS:       now,
		Progress: map[string]model.Progress{},
		Metrics:  map[string]float64{},
		Text:     map[string]string{},
	}
	flat := map[string]any{}
	flatten("", raw, flat)

	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		val := flat[key]
		lk := strings.ToLower(key)
		if str, isStr := val.(string); val == nil || (isStr && strings.TrimSpace(str) == "") {
			if !strings.HasPrefix(key, "_") && lk != "status" && lk != "message" && lk != "msg" {
				name := key
				if lk == "progress" {
					name = "main"
				} else if strings.HasPrefix(lk, "progress.") {
					name = key[len("progress."):]
				}
				ev.Unset = append(ev.Unset, name) // key= removes it from the run
			}
			continue
		}
		switch {
		case key == KeyCoalesce || key == KeySeq:
			continue
		case key == KeyTS:
			if f, ok := toFloat(val); ok && f > 0 && isFinite(f) {
				sec, frac := math.Modf(f)
				ev.TS = time.Unix(int64(sec), int64(frac*1e9))
			}
			continue
		case lk == "status" || lk == "state":
			if s, ok := val.(string); ok {
				ev.Status = ParseStatus(s)
			}
			continue
		case lk == "message" || lk == "msg":
			ev.Message = fmt.Sprint(val)
			continue
		case lk == "exit_code" || lk == "exitcode":
			if f, ok := toFloat(val); ok && isFinite(f) {
				c := int(f)
				ev.ExitCode = &c
			}
			continue
		case lk == "step" || lk == "global_step":
			if f, ok := toFloat(val); ok && isFinite(f) {
				ev.Step = &f
			} else if p, ok := progressValue(val, false); ok {
				cur := p.Cur
				ev.Step = &cur
				ev.Progress[lk] = p
			}
			continue
		case lk == "progress" || strings.HasPrefix(lk, "progress."):
			name := "main"
			if lk != "progress" {
				name = key[len("progress."):]
			}
			if p, ok := progressValue(val, true); ok {
				ev.Progress[name] = p
			}
			continue
		}

		if p, ok := progressValue(val, false); ok && !Denied(key) {
			ev.Progress[key] = p
			continue
		}
		if f, ok := toFloat(val); ok {
			if !Denied(key) {
				ev.Metrics[key] = f
			}
			continue
		}
		if str, ok := val.(string); ok && strings.TrimSpace(str) != "" && !secretKeyRe.MatchString(key) {
			// Other text is kept as a field (current=clip_042.mp4, phase=upload).
			// Secret-looking keys are never stored.
			ev.Text[key] = truncateRunes(str, 300)
		}
	}
	return ev
}

// progressValue interprets val as progress. If bare is true (the key was
// literally "progress"), a plain number is accepted as a fraction or percent.
func progressValue(val any, bare bool) (model.Progress, bool) {
	switch v := val.(type) {
	case []any:
		if len(v) == 2 {
			a, ok1 := toFloat(v[0])
			b, ok2 := toFloat(v[1])
			if ok1 && ok2 && validRatio(a, b) {
				return model.Progress{Cur: a, Total: b}, true
			}
		}
	case string:
		if m := ratioRe.FindStringSubmatch(v); m != nil {
			a, _ := strconv.ParseFloat(m[1], 64)
			b, _ := strconv.ParseFloat(m[2], 64)
			if validRatio(a, b) {
				return model.Progress{Cur: a, Total: b}, true
			}
		}
		if m := percentRe.FindStringSubmatch(v); m != nil {
			a, _ := strconv.ParseFloat(m[1], 64)
			if a >= 0 && a <= 100 {
				return model.Progress{Cur: a, Total: 100}, true
			}
		}
		if bare {
			t := strings.TrimSpace(v)
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				// From text, a whole number is a percent (progress=1 is 1%,
				// as a script counting 1..100 means); a fraction needs a
				// decimal point (progress=0.5, progress=1.0).
				if !strings.ContainsAny(t, ".eE") && f >= 0 && f <= 100 {
					return model.Progress{Cur: f, Total: 100}, true
				}
				return bareProgress(f)
			}
		}
	case float64, int, int64:
		if bare {
			f, _ := toFloat(v)
			return bareProgress(f)
		}
	}
	return model.Progress{}, false
}

func bareProgress(f float64) (model.Progress, bool) {
	switch {
	case !isFinite(f) || f < 0:
		return model.Progress{}, false
	case f <= 1:
		return model.Progress{Cur: f * 100, Total: 100}, true
	case f <= 100:
		return model.Progress{Cur: f, Total: 100}, true
	}
	return model.Progress{}, false
}

func validRatio(a, b float64) bool {
	return isFinite(a) && isFinite(b) && b > 0 && a >= 0 && a <= b
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// flatten turns nested maps into dotted keys. Arrays are kept as values.
func flatten(prefix string, in map[string]any, out map[string]any) {
	for k, v := range in {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if m, ok := v.(map[string]any); ok {
			flatten(key, m, out)
			continue
		}
		out[key] = v
	}
}
