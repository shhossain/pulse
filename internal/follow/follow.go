// Package follow lets this computer's Pulse show a hub's runs: it reads the
// hub's run stream through its tunnel with a view-only Access service token
// (set up by `pulse hub follow`) and hands each snapshot to the app.
package follow

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pulse/internal/model"
	"pulse/internal/paths"
)

// Config is what the app needs to follow a hub. Saved in ConfigFile (no
// secrets: the service token's secret lives in the Keychain or a 0600 file).
type Config struct {
	URL       string    `json:"url"`
	ClientID  string    `json:"client_id"`
	CFTokenID string    `json:"cf_token_id,omitempty"` // to delete it on unfollow
	Expires   time.Time `json:"expires,omitempty"`     // the service token's end: follow again before
}

func ConfigFile() string { return filepath.Join(paths.Home(), "follow.json") }

// Load reads the follow config (ok=false when not following).
func Load() (Config, bool) {
	var c Config
	b, err := os.ReadFile(ConfigFile())
	if err != nil || json.Unmarshal(b, &c) != nil || !strings.HasPrefix(c.URL, "https://") || c.ClientID == "" {
		return Config{}, false
	}
	return c, true
}

// Save writes it (0600).
func Save(c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		return err
	}
	tmp := ConfigFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ConfigFile())
}

// Remove stops following.
func Remove() error {
	err := os.Remove(ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// StaleAfter is how long a hub may be unreachable before its runs are no
// longer shown (better nothing than a frozen "running").
var StaleAfter = 2 * time.Minute

// Follower streams a hub's runs.
type Follower struct {
	Config
	Secret string
	HTTP   *http.Client // no timeout: the stream is long-lived
	// OnRuns receives each snapshot (nil: the hub is out of reach for StaleAfter).
	OnRuns func(runs []model.Run)
}

// Run follows until ctx ends, reconnecting with backoff.
func (f *Follower) Run(ctx context.Context) {
	hc := f.HTTP
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second // a hub that accepts but never answers
		hc = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if !f.Expires.IsZero() && time.Until(f.Expires) < 30*24*time.Hour {
		log.Printf("follow %s: this Mac's view-only token expires %s: pulse hub unfollow, then follow again", f.URL, f.Expires.Format(time.DateOnly))
	}
	backoff := time.Second
	lastOK := time.Now() // last sign of life: an event or a keep-alive
	shown := false
	for ctx.Err() == nil {
		err := f.stream(ctx, hc, func() { lastOK = time.Now() }, func(runs []model.Run) {
			backoff, shown = time.Second, true
			f.OnRuns(runs)
		})
		if ctx.Err() != nil {
			return
		}
		if shown && time.Since(lastOK) > StaleAfter {
			f.OnRuns(nil)
			shown = false
		}
		log.Printf("follow %s: %v (retrying in %s)", f.URL, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// stream reads one connection's events until it ends. A stream that goes
// silent past the hub's 15 s keep-alives is treated as dead.
func (f *Follower) stream(ctx context.Context, hc *http.Client, alive func(), each func([]model.Run)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(f.URL, "/")+"/api/v1/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "pulse-follow")
	req.Header.Set("CF-Access-Client-Id", f.ClientID)
	req.Header.Set("CF-Access-Client-Secret", f.Secret)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("HTTP %d (%s): is this Mac still allowed to view the hub? (pulse hub follow)", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	tick := make(chan struct{}, 1)
	go func() { // watchdog: no line for 45 s means the connection is gone
		t := time.NewTimer(45 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				t.Reset(45 * time.Second)
			case <-t.C:
				cancel()
				return
			}
		}
	}()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	event := ""
	for sc.Scan() {
		select {
		case tick <- struct{}{}:
		default:
		}
		alive()
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "runs":
			var v struct {
				Runs []model.Run `json:"runs"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				return fmt.Errorf("bad event: %w", err)
			}
			for i := range v.Runs {
				v.Runs[i].Hub = strings.TrimRight(f.URL, "/")
			}
			each(v.Runs)
		case line == "":
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("stream ended")
}
