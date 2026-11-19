// Package parse extracts progress and metrics from a program's terminal
// output, one line at a time. It returns raw flat maps; the app normalizes them.
package parse

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"pulse/internal/normalize"
)

// Marker lets a program report explicitly: `::pulse:: {"loss": 0.4}`.
const Marker = "::pulse::"

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

	// tqdm: "desc:  45%|████▌     | 450/1000 [01:02<01:15,  7.20it/s, loss=0.43]"
	tqdmRe = regexp.MustCompile(`^\s*(?:(.*?):\s*)?(\d{1,3})%\|[^|]*\|\s*([\d.]+[kMGTP]?)/([\d.]+[kMGTP]?)\s*(?:\[([^\]]*)\])?`)

	// "epoch 3/10", "Step 120 of 1000", "batch [3/10]", "Epoch: 3/10"
	keywordRe = regexp.MustCompile(`(?i)\b(epoch|step|iter|iteration|batch|round|episode|stage|phase|task|file|item|chunk|shard|fold|trial|sample|page|test|job|update|generation|gen)s?\b\s*[:=#]?\s*\[?\s*(\d+)\s*(?:/|of|out of)\s*(\d+)`)

	// "[3/10] doing something"
	bracketRe = regexp.MustCompile(`^\s*\[\s*(\d+)\s*/\s*(\d+)\s*\]`)

	numPat = `[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?|[-+]?(?i:nan|infinity|inf)`
	// "loss=0.43" (no space needed) or "loss: 0.43" (space required after colon,
	// so "localhost:8080" and "12:30" are not metrics).
	// The character after the value is checked in code (Go regexp has no lookahead).
	kvEqRe    = regexp.MustCompile(`(?:^|[\s,;|(\[{])([A-Za-z_][\w./-]{0,40}?)\s*=\s*(` + numPat + `)`)
	kvColonRe = regexp.MustCompile(`(?:^|[\s,;|(\[{])([A-Za-z_][\w./-]{0,40}?):[ \t]+(` + numPat + `)`)

	// 'loss': 1.23 or "loss": NaN — Python dict reprs (Hugging Face Trainer
	// prints these) and JSON with bare NaN/Infinity, which encoding/json refuses.
	quotedKVRe = regexp.MustCompile(`['"]([A-Za-z_][\w./-]{0,60})['"]\s*:\s*(` + numPat + `)`)

	nameCleanRe = regexp.MustCompile(`[^a-z0-9]+`)
	digitsRe    = regexp.MustCompile(`\d+`)
)

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// Line parses one line of output. ok is false if nothing was found.
func Line(line string) (map[string]any, bool) {
	line = strings.TrimSpace(StripANSI(line))
	if line == "" {
		return nil, false
	}

	// 1. explicit marker
	if i := strings.Index(line, Marker); i >= 0 {
		rest := strings.TrimSpace(line[i+len(Marker):])
		if m, ok := jsonObject(rest); ok {
			return m, true
		}
		if m := quotedKV(rest); len(m) > 0 {
			return m, true
		}
	}
	// 2. JSON line. Structured logs are full of text (level, logger, ids,
	// sometimes secrets): keep only numbers and progress values from them.
	// Text and status need the explicit ::pulse:: marker.
	if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
		if m, ok := jsonObject(line); ok {
			if m = numericOnly(m); len(m) > 0 {
				return m, true
			}
			return nil, false
		}
		// Not strict JSON: a Python dict repr or JSON with NaN.
		if m := quotedKV(line); len(m) > 0 {
			return m, true
		}
		// Otherwise read it like any other line ({loss=0.43 step=3}).
	}

	out := map[string]any{}

	// 3. tqdm bar
	if m := tqdmRe.FindStringSubmatch(line); m != nil {
		n, ok1 := siNumber(m[3])
		total, ok2 := siNumber(m[4])
		if ok1 && ok2 && total > 0 && finite(n) && finite(total) {
			out["progress."+progressName(m[1])] = []any{n, total}
			// "Epoch 3/10: 45%|…" carries the outer epoch count in its
			// description; keep it as its own bar.
			for _, km := range keywordRe.FindAllStringSubmatch(m[1], -1) {
				if cur, tot, ok := ratio(km[2], km[3]); ok {
					out["progress."+keyword(km[1])] = []any{cur, tot}
				}
			}
			out[normalize.KeyCoalesce] = true
			if m[5] != "" {
				for _, part := range strings.Split(m[5], ",") {
					addKV(out, part)
				}
			}
			return out, true
		}
	}

	// 4. keyword progress
	for _, m := range keywordRe.FindAllStringSubmatch(line, -1) {
		cur, total, ok := ratio(m[2], m[3])
		if !ok {
			continue
		}
		kw := keyword(m[1])
		out["progress."+kw] = []any{cur, total}
		if kw == "step" {
			out["step"] = cur
		}
	}
	if m := bracketRe.FindStringSubmatch(line); m != nil {
		if cur, total, ok := ratio(m[1], m[2]); ok {
			if _, exists := out["progress.main"]; !exists {
				out["progress.main"] = []any{cur, total}
			}
		}
	}

	// 5. key=value / key: value metrics
	addKV(out, line)

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// ratio parses "cur/total" parts; huge or inverted values are refused (a
// non-finite number would make the whole batch unsendable).
func ratio(a, b string) (float64, float64, bool) {
	cur, err1 := strconv.ParseFloat(a, 64)
	total, err2 := strconv.ParseFloat(b, 64)
	if err1 != nil || err2 != nil || !finite(cur) || !finite(total) || total <= 0 || cur > total {
		return 0, 0, false
	}
	return cur, total, true
}

func keyword(kw string) string {
	switch kw = strings.ToLower(kw); kw {
	case "iteration":
		return "iter"
	case "gen":
		return "generation"
	}
	return kw
}

func quotedKV(s string) map[string]any {
	out := map[string]any{}
	for _, m := range quotedKVRe.FindAllStringSubmatch(s, -1) {
		if _, err := strconv.ParseFloat(m[2], 64); err == nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

func addKV(out map[string]any, s string) {
	for _, re := range []*regexp.Regexp{kvEqRe, kvColonRe} {
		for _, idx := range re.FindAllStringSubmatchIndex(s, -1) {
			end := idx[1]
			if end < len(s) && !strings.ContainsRune(" \t,;|)]}%", rune(s[end])) &&
				!(s[end] == '.' && (end+1 == len(s) || s[end+1] == ' ')) { // "loss=0.43." ends a sentence
				continue // value runs into other text, e.g. "v=1.2.3" or "x=5abc"
			}
			m := []string{s[idx[0]:idx[1]], s[idx[2]:idx[3]], s[idx[4]:idx[5]]}
			key := strings.Trim(m[1], "./-")
			if key == "" {
				continue
			}
			if _, exists := out[key]; exists {
				continue
			}
			// Only keep values that parse; the normalizer handles NaN/Inf.
			if _, err := strconv.ParseFloat(m[2], 64); err == nil {
				out[key] = m[2]
			}
		}
	}
}

// progressName turns a tqdm description into a stable key:
// "Training" → "training", "Epoch 3" → "batch", "" → "main".
func progressName(desc string) string {
	s := strings.ToLower(strings.TrimSpace(desc))
	numbered := digitsRe.MatchString(s)
	s = digitsRe.ReplaceAllString(s, "")
	s = strings.Trim(nameCleanRe.ReplaceAllString(s, "_"), "_")
	switch s {
	case "":
		return "main"
	case "epoch", "epochs":
		// "Epoch 3: 45%|…| 450/1000" counts batches inside epoch 3, while a
		// bar described just "Epoch" is the epoch loop itself.
		if numbered {
			return "batch"
		}
		return "epoch"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

func siNumber(s string) (float64, bool) {
	mult := 1.0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k':
			mult = 1e3
		case 'M':
			mult = 1e6
		case 'G':
			mult = 1e9
		case 'T':
			mult = 1e12
		case 'P':
			mult = 1e15
		}
		if mult != 1 {
			s = s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f * mult, true
}

// numericOnly keeps numbers, numeric strings and progress-like strings
// ("3/10", "45%"), recursing into nested objects.
func numericOnly(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		switch x := v.(type) {
		case float64, []any:
			out[k] = x
		case string:
			if _, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil || progressish.MatchString(x) {
				out[k] = x
			}
		case map[string]any:
			if sub := numericOnly(x); len(sub) > 0 {
				out[k] = sub
			}
		}
	}
	return out
}

var progressish = regexp.MustCompile(`^\s*\d+(\.\d+)?\s*(/\s*\d+(\.\d+)?|%)\s*$`)

func jsonObject(s string) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil || len(m) == 0 {
		return nil, false
	}
	return m, true
}
