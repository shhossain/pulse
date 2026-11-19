package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolveNeverSendsLocalTokensElsewhere(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	t.Setenv("PULSE_TOKEN", "")
	t.Setenv("PULSE_URL", "http://gpu-box.example:7777")
	if _, err := Resolve(false); err == nil || !strings.Contains(err.Error(), "PULSE_TOKEN") {
		t.Fatalf("remote URL without token: %v", err)
	}
	t.Setenv("PULSE_URL", "http://127.0.0.1:7777")
	if _, err := Resolve(false); err != nil {
		t.Fatalf("local URL: %v", err)
	}
}

func TestBatchesFitTheBudget(t *testing.T) {
	var events []map[string]any
	for i := 0; i < 500; i++ {
		events = append(events, map[string]any{"blob": strings.Repeat("x", 9000)})
	}
	n, _ := fitBudget(events, nil)
	b, _ := json.Marshal(events[:n])
	if n == 0 || n == 500 || len(b) > maxBatchBytes {
		t.Fatalf("n=%d bytes=%d", n, len(b))
	}
	if n, nl := fitBudget(events[:1], []string{strings.Repeat("y", 5<<20)}); n != 1 || nl != 0 {
		t.Fatalf("huge line should wait for its own batch: n=%d nl=%d", n, nl)
	}
	if n, nl := fitBudget(nil, []string{strings.Repeat("y", 5<<20)}); n != 0 || nl != 1 {
		t.Fatalf("a lone huge line still goes out: n=%d nl=%d", n, nl)
	}
}

func TestCoalesceKeepsOtherBars(t *testing.T) {
	s := NewRunSender("r_1", "j", modelMeta())
	s.Add(map[string]any{"progress.epoch": []any{3.0, 10.0}, "_coalesce": true})
	s.Add(map[string]any{"progress.batch": []any{1.0, 100.0}, "_coalesce": true})
	s.Add(map[string]any{"progress.batch": []any{2.0, 100.0}, "_coalesce": true})
	if len(s.pending) != 2 {
		t.Fatalf("pending = %v", s.pending)
	}
}

func TestUnsendableEventIsDropped(t *testing.T) {
	_, err := json.Marshal(map[string]any{"x": math.Inf(1)})
	var u *json.UnsupportedValueError
	if err == nil || !errorsAs(err, &u) {
		t.Fatalf("expected UnsupportedValueError, got %v", err)
	}
}

func TestLogTrimKeepsInFlightLines(t *testing.T) {
	s := NewRunSender("r_1", "j", modelMeta())
	for i := 0; i < maxLogPending; i++ {
		s.AddLog("old")
	}
	s.logsOut = 2000 // a send of the first 2000 lines is in flight
	s.AddLog("new1")
	s.AddLog("new2")
	if len(s.logs) != maxLogPending {
		t.Fatalf("len = %d", len(s.logs))
	}
	s.logs = dropFront(s.logs, 2000) // the send succeeded
	if got := s.logs[len(s.logs)-2:]; got[0] != "new1" || got[1] != "new2" {
		t.Fatalf("unsent lines lost: %v", got)
	}
	if len(s.logs) != maxLogPending-2000 {
		t.Fatalf("wrong lines removed: %d left", len(s.logs))
	}
}

func TestAccessHeaders(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	t.Setenv("PULSE_TOKEN", "pls_job")
	t.Setenv("PULSE_URL", "https://pulse.example.com")
	t.Setenv(EnvAccessID, "abc.access")
	t.Setenv(EnvAccessSecret, "s3cret")
	c, err := Resolve(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Header.Get("CF-Access-Client-Id") != "abc.access" || c.Header.Get("CF-Access-Client-Secret") != "s3cret" {
		t.Fatalf("headers = %v", c.Header)
	}
	t.Setenv(EnvAccessSecret, "")
	if _, err := Resolve(false); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("half a service token: %v", err)
	}
	t.Setenv(EnvAccessSecret, "s3cret")
	t.Setenv("PULSE_URL", "http://pulse.example.com")
	if c, err := Resolve(false); err != nil || c.Header != nil {
		t.Fatalf("service token over http: must not be sent, nor break the CLI: %v %v", c, err)
	}
	t.Setenv(EnvAccessID, "")
	t.Setenv(EnvAccessSecret, "")
	if c, err := Resolve(false); err != nil || c.Header != nil {
		t.Fatalf("no service token: %v %v", c, err)
	}
}

func TestInterceptedResponses(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		switch r.URL.Path {
		case "/api/v1/login":
			http.Redirect(w, r, "https://team.cloudflareaccess.com/cdn-cgi/access/login", http.StatusFound)
		case "/api/v1/cf403":
			w.Header().Set("Cf-Ray", "8f00")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(403)
		default: // the app's own refusal
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"token can't report this job"}`))
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTP: newHTTP(5 * time.Second), Header: http.Header{"Cf-Access-Client-Id": {"x"}}}
	ctx := context.Background()
	var apiErr *APIError
	if err := c.Do(ctx, "POST", "/api/v1/login", nil, nil); !errors.As(err, &apiErr) || !apiErr.Intercepted || !strings.Contains(apiErr.Msg, "Cloudflare Access") {
		t.Errorf("redirect to Access login: %v", err)
	}
	if got.Get("Cf-Access-Client-Id") != "x" {
		t.Errorf("extra header not sent: %v", got)
	}
	if err := c.Do(ctx, "POST", "/api/v1/cf403", nil, nil); !errors.As(err, &apiErr) || !apiErr.Intercepted || !strings.Contains(apiErr.Msg, "Cloudflare refused") {
		t.Errorf("Cloudflare 403: %v", err)
	}
	if err := c.Do(ctx, "POST", "/api/v1/push/x", nil, nil); !errors.As(err, &apiErr) || apiErr.Intercepted || apiErr.Msg != "token can't report this job" {
		t.Errorf("app's own 403: %v", err)
	}
}

// A response from something in front of Pulse (a proxy, a tunnel, a login
// page) says nothing about the data: the sender keeps it for a retry. Only
// Pulse's own refusals (JSON) drop it.
func TestSenderKeepsDataOnProxyErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ctype  string
		cfRay  bool
		kept   bool
	}{
		{"Access 403 page", 403, "text/html", true, true},
		{"Cloudflare rate limit", 429, "text/html", true, true},
		{"tunnel catch-all 404", 404, "text/plain", false, true},
		{"proxy 413", 413, "text/html", false, true},
		{"Cloudflare 530, tunnel down", 530, "text/html", true, true},
		{"Pulse rate-limits the token", 429, "application/json", false, true},
		{"Pulse refuses the data", 403, "application/json", false, false},
		{"Pulse: bad request", 400, "application/json", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.ctype)
				if c.cfRay {
					w.Header().Set("Cf-Ray", "8f00")
				}
				w.WriteHeader(c.status)
				if c.ctype == "application/json" {
					w.Write([]byte(`{"error":"no"}`))
				}
			}))
			defer srv.Close()
			t.Setenv("PULSE_HOME", t.TempDir())
			t.Setenv("PULSE_URL", srv.URL)
			t.Setenv("PULSE_TOKEN", "pls_job")
			s := NewRunSender("r_aaaaaaaaaaaa", "job", modelMeta())
			s.Add(map[string]any{"progress": "1/2"})
			s.flush(true)
			s.mu.Lock()
			kept := len(s.pending) == 1
			s.mu.Unlock()
			if kept != c.kept {
				t.Errorf("kept = %v, want %v", kept, c.kept)
			}
		})
	}
}

func TestSecretsAreTrimmed(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	t.Setenv("PULSE_URL", "https://pulse.example.com\n")
	t.Setenv("PULSE_TOKEN", "pls_job\n")
	t.Setenv(EnvAccessID, "abc.access\n")
	t.Setenv(EnvAccessSecret, "s3cret\r\n")
	c, err := Resolve(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://pulse.example.com" || c.Token != "pls_job" || c.Header.Get("CF-Access-Client-Secret") != "s3cret" {
		t.Fatalf("got %q %q %v", c.BaseURL, c.Token, c.Header)
	}
}

func TestResolveLocalIgnoresPulseURL(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir()) // no local app
	t.Setenv("PULSE_URL", "https://hub.example.com")
	t.Setenv("PULSE_TOKEN", "pls_job")
	if _, err := Resolve(false); err != nil {
		t.Fatalf("Resolve should use PULSE_URL: %v", err)
	}
	if _, err := ResolveLocal(false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("ResolveLocal found an app through PULSE_URL: %v", err)
	}
}

func TestConnectExplainsARemoteRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cf-Ray", "8f00")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	t.Setenv("PULSE_HOME", t.TempDir())
	t.Setenv("PULSE_URL", srv.URL)
	t.Setenv("PULSE_TOKEN", "pls_job")
	_, err := Connect(context.Background(), false, time.Second)
	if err == nil || errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), "Cloudflare refused") {
		t.Fatalf("got %v, want the Cloudflare refusal", err)
	}
}
