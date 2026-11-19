// Package audit records security-relevant events (shell actions run, trust
// granted, tokens created/revoked/rotated, remote access, deletions, stops,
// answers) as JSON lines in audit.log. It's append-only and never contains
// secrets.
package audit

import (
	"encoding/json"
	"io"
	"log"
	"regexp"
	"sync"
	"time"
)

var secretish = regexp.MustCompile(`(?i)(\b(?:token|password|passwd|secret|api[_-]?key|auth)\s*[=:]\s*|bearer\s+|--(?:token|password|secret)[= ])(\S+)|\b(pls_[0-9a-f]+|sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16})\b`)

// Redact masks values that look like credentials in a command line.
func Redact(s string) string {
	return secretish.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretish.FindStringSubmatch(m)
		if sub[1] != "" {
			return sub[1] + "***"
		}
		return "***"
	})
}

var (
	mu  sync.Mutex
	out io.Writer
)

// SetOutput sets where events go (nil disables auditing, e.g. in tests).
func SetOutput(w io.Writer) {
	mu.Lock()
	out = w
	mu.Unlock()
}

// Log records one event. fields must not hold secrets.
func Log(event string, fields map[string]any) {
	mu.Lock()
	defer mu.Unlock()
	if out == nil {
		return
	}
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		log.Printf("audit: %v", err)
		return
	}
	if _, err := out.Write(append(b, '\n')); err != nil {
		log.Printf("audit: %v", err)
	}
}
