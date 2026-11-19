package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pulse/internal/audit"
	"pulse/internal/model"
	"pulse/internal/paths"
	"pulse/internal/webpush"
)

// Web push: viewers of the web UI can get Pulse's notifications on their
// phone. Each viewer's subscriptions are stored on this machine; payloads are
// encrypted for that browser, so the push service relaying them can't read
// them. Only people still on the web UI's list are sent anything.

const maxSubsPerViewer = 10

type pushSub struct {
	Email   string               `json:"email"`
	Sub     webpush.Subscription `json:"sub"`
	Created time.Time            `json:"created"`
}

type pusher struct {
	mu       sync.Mutex
	keys     *webpush.Keys
	subs     []pushSub
	client   *http.Client
	lastTest map[string]time.Time
	// send is webpush delivery (tests replace it).
	send func(ctx context.Context, sub webpush.Subscription, m webpush.Message) error
}

func pushKeyFile() string  { return filepath.Join(paths.Home(), "vapid.json") }
func pushSubsFile() string { return filepath.Join(paths.Home(), "push-subscriptions.json") }

// loadPusher reads (or creates) this server's push key and subscriptions.
func loadPusher() (*pusher, error) {
	p := &pusher{client: &http.Client{Timeout: 15 * time.Second}, lastTest: map[string]time.Time{}}
	// A key that can't be read is an error, never a reason to make a new
	// one: every phone subscribed with the old key would silently stop.
	switch b, err := os.ReadFile(pushKeyFile()); {
	case err == nil:
		if p.keys, err = webpush.LoadKeys(b); err != nil {
			return nil, fmt.Errorf("push key %s: %w", pushKeyFile(), err)
		}
	case errors.Is(err, os.ErrNotExist):
		if p.keys, err = webpush.NewKeys(); err != nil {
			return nil, err
		}
		if err := writePrivate(pushKeyFile(), p.keys.Marshal()); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("push key: %w", err)
	}
	switch b, err := os.ReadFile(pushSubsFile()); {
	case err == nil:
		if err := json.Unmarshal(b, &p.subs); err != nil {
			return nil, fmt.Errorf("%s: %w", pushSubsFile(), err) // don't overwrite it later
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("push subscriptions: %w", err)
	}
	return p, nil
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *pusher) saveLocked() error {
	b, _ := json.MarshalIndent(p.subs, "", "  ")
	return writePrivate(pushSubsFile(), b)
}

func (p *pusher) add(email string, sub webpush.Subscription) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := make([]pushSub, 0, len(p.subs)+1)
	n := 0
	for _, s := range p.subs {
		if s.Sub.Endpoint == sub.Endpoint { // this device again: replace it
			continue
		}
		if s.Email == email {
			n++
		}
		kept = append(kept, s)
	}
	if n >= maxSubsPerViewer { // nothing changed yet
		return fmt.Errorf("at most %d devices per person; turn notifications off on one first", maxSubsPerViewer)
	}
	kept = append(kept, pushSub{Email: email, Sub: sub, Created: time.Now().UTC()})
	prev := p.subs
	p.subs = kept
	if err := p.saveLocked(); err != nil {
		p.subs = prev
		return err
	}
	return nil
}

// remove drops endpoint; email limits it to that person's (empty: anyone's).
func (p *pusher) remove(email, endpoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, s := range p.subs {
		if s.Sub.Endpoint == endpoint && (email == "" || s.Email == email) {
			p.subs = append(p.subs[:i], p.subs[i+1:]...)
			_ = p.saveLocked()
			return true
		}
	}
	return false
}

func (p *pusher) has(email, endpoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.subs {
		if s.Email == email && s.Sub.Endpoint == endpoint {
			return true
		}
	}
	return false
}

// targets returns the subscriptions of people allowed (by allowed) and,
// with email set, only that person's.
func (p *pusher) targets(allowed func(string) bool, email string) []pushSub {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []pushSub
	for _, s := range p.subs {
		if allowed(s.Email) && (email == "" || s.Email == email) {
			out = append(out, s)
		}
	}
	return out
}

// pushPayload is what the service worker shows.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Level string `json:"level,omitempty"`
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func payloadFor(n model.Notification) pushPayload {
	body := n.Subtitle
	if n.Body != "" {
		body = strings.TrimSpace(body + "\n" + n.Body)
	}
	body = clip(body, 400)
	if n.AskID != "" { // after clipping, so it's never the part cut off
		body = strings.TrimSpace(body + "\nAnswer it on the machine running Pulse.")
	}
	p := pushPayload{Title: clip(n.Title, 120), Body: body, Level: n.Level, Tag: n.RunID}
	if n.RunID != "" {
		p.URL = "/r/" + n.RunID
	}
	return p
}

// deliver sends m to targets, dropping subscriptions that are gone.
func (p *pusher) deliver(targets []pushSub, m webpush.Message, subject string) (sent int) {
	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var err error
		if p.send != nil {
			err = p.send(ctx, t.Sub, m)
		} else {
			err = p.keys.Send(ctx, p.client, t.Sub, m, subject)
		}
		cancel()
		switch {
		case errors.Is(err, webpush.ErrGone) || errors.Is(err, webpush.ErrRejected):
			p.remove("", t.Sub.Endpoint)
			audit.Log("push.unsubscribed", map[string]any{"email": t.Email, "reason": "gone"})
		case err != nil:
			log.Printf("push to %s: %v", t.Email, err)
		default:
			sent++
		}
	}
	return sent
}

// PushNotifier sends notifications to the web UI's viewers who turned them
// on. It's a no-op while the web UI is off.
func (s *Server) PushNotifier() pushNotifier { return pushNotifier{s} }

type pushNotifier struct{ s *Server }

func (pn pushNotifier) Notify(n model.Notification) {
	s := pn.s
	wa := s.webAccess()
	p := s.pushes()
	if wa == nil || p == nil {
		return
	}
	targets := p.targets(func(e string) bool { return wa.emails[e] }, "")
	if len(targets) == 0 {
		return
	}
	b, _ := json.Marshal(payloadFor(n))
	urgent := n.Level == "error" || n.AskID != ""
	topic := ""
	if n.RunID != "" && len(n.RunID) <= 32 {
		topic = n.RunID // a newer notice about a run replaces an undelivered older one
	}
	go p.deliver(targets, webpush.Message{Payload: b, TTL: 24 * time.Hour, Urgent: urgent, Topic: topic}, s.publicURL())
}

func (s *Server) pushes() *pusher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pusher == nil {
		p, err := loadPusher()
		if err != nil {
			log.Printf("web push unavailable: %v", err)
			return nil
		}
		s.pusher = p
	}
	return s.pusher
}

func (s *Server) publicURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remote != nil && s.remote.publicURL != "" {
		return s.remote.publicURL
	}
	return "https://pulse.invalid"
}

// Viewer endpoints: a signed-in viewer manages their own subscriptions.

func (s *Server) pushStatus(w http.ResponseWriter, r *http.Request) {
	p := s.pushes()
	if p == nil {
		writeErr(w, http.StatusServiceUnavailable, "web push unavailable")
		return
	}
	writeJSON(w, map[string]any{
		"public_key": p.keys.Public(),
		"subscribed": p.has(viewerEmail(r), r.URL.Query().Get("endpoint")),
	})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	p := s.pushes()
	if p == nil {
		writeErr(w, http.StatusServiceUnavailable, "web push unavailable")
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := sub.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := viewerEmail(r)
	if err := p.add(email, sub); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	audit.Log("push.subscribed", map[string]any{"email": email})
	writeJSON(w, map[string]any{"subscribed": true})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	p := s.pushes()
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if p == nil || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if p.remove(viewerEmail(r), in.Endpoint) {
		audit.Log("push.unsubscribed", map[string]any{"email": viewerEmail(r), "reason": "viewer"})
	}
	writeJSON(w, map[string]any{"subscribed": false})
}

// pushTest sends the viewer a test notification (at most every 10 seconds).
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	p := s.pushes()
	if p == nil {
		writeErr(w, http.StatusServiceUnavailable, "web push unavailable")
		return
	}
	email := viewerEmail(r)
	p.mu.Lock()
	if time.Since(p.lastTest[email]) < 10*time.Second {
		p.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "one test every 10 seconds")
		return
	}
	p.lastTest[email] = time.Now()
	p.mu.Unlock()
	targets := p.targets(func(string) bool { return true }, email)
	b, _ := json.Marshal(pushPayload{Title: "Pulse notifications are on", Body: "You'll hear here when a job finishes, fails or needs you.", URL: "/popover", Tag: "test"})
	go p.deliver(targets, webpush.Message{Payload: b, TTL: time.Hour}, s.publicURL()) // push services can be slow
	writeJSON(w, map[string]any{"sending": len(targets)})
}
