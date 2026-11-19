// Package client talks to the Pulse app's HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pulse/internal/model"
	"pulse/internal/paths"
)

// ErrNotRunning means the app could not be found or reached.
var ErrNotRunning = errors.New("the Pulse app is not running")

// Client is a configured API client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Header is sent with every request: a Cloudflare Access service
	// token when the app sits behind Access (see Resolve).
	Header http.Header
}

// Environment variables holding a Cloudflare Access service token, for an
// app reached through a tunnel that Access protects.
const (
	EnvAccessID     = "PULSE_CF_ACCESS_CLIENT_ID"
	EnvAccessSecret = "PULSE_CF_ACCESS_CLIENT_SECRET"
)

// newHTTP returns an http.Client that never follows redirects: the API
// doesn't redirect, so a redirect means something in front of it (a login
// page) intercepted the request, and following it would hide that.
func newHTTP(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Resolve finds the app without contacting it. PULSE_URL / PULSE_TOKEN take
// precedence (remote machines); otherwise the local server.json and token
// files are used. admin selects the admin token (local only).
func Resolve(admin bool) (*Client, error) {
	c := &Client{HTTP: newHTTP(5 * time.Second)}
	if u := strings.TrimSpace(os.Getenv("PULSE_URL")); u != "" {
		c.BaseURL = strings.TrimRight(u, "/")
		c.Token = strings.TrimSpace(os.Getenv("PULSE_TOKEN")) // a secret file's trailing newline isn't part of it
		h, err := accessHeader(c.BaseURL, os.Getenv(EnvAccessID), os.Getenv(EnvAccessSecret))
		if err != nil {
			return nil, err
		}
		c.Header = h
		if c.Token == "" {
			// Only hand this Mac's own tokens to this Mac's app; never send
			// them to whatever host PULSE_URL names.
			if !isLoopbackURL(c.BaseURL) {
				return nil, errors.New("PULSE_URL points at another machine; set PULSE_TOKEN too (pulse remote token <job>)")
			}
			c.Token = localToken(admin)
		}
		return c, nil
	}
	return ResolveLocal(admin)
}

// ResolveLocal finds the app on this machine (server.json and the token
// files in the data folder), ignoring PULSE_URL.
func ResolveLocal(admin bool) (*Client, error) {
	c := &Client{HTTP: newHTTP(5 * time.Second)}
	si, err := paths.ReadServerInfo()
	if err != nil || si.URL == "" {
		return nil, ErrNotRunning
	}
	c.BaseURL = si.URL
	c.Token = localToken(admin)
	if c.Token == "" {
		return nil, ErrNotRunning
	}
	return c, nil
}

// accessHeader builds the Cloudflare Access service-token headers. Both
// values or neither; only ever sent over https.
func accessHeader(base, id, secret string) (http.Header, error) {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret) // e.g. a secret file's trailing newline
	if id == "" && secret == "" {
		return nil, nil
	}
	if id == "" || secret == "" {
		return nil, fmt.Errorf("set both %s and %s, or neither", EnvAccessID, EnvAccessSecret)
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" {
		return nil, nil // never in clear text; e.g. a Tailscale http:// URL needs none
	}
	h := http.Header{}
	h.Set("CF-Access-Client-Id", id)
	h.Set("CF-Access-Client-Secret", secret)
	return h, nil
}

func localToken(admin bool) string {
	if admin {
		return paths.ReadToken(paths.AdminToken())
	}
	return paths.ReadToken(paths.PushToken())
}

// Connect resolves the app and checks it is alive. If it is not, and this is
// a Mac, it launches Pulse.app in the background and waits up to wait.
func Connect(ctx context.Context, admin bool, wait time.Duration) (*Client, error) {
	c, err := Resolve(admin)
	if err == nil {
		herr := c.Health(ctx)
		if herr == nil {
			return c, nil
		}
		if os.Getenv("PULSE_URL") != "" { // another machine's Pulse: nothing to launch, say why
			return nil, fmt.Errorf("can't reach Pulse at %s: %w", c.BaseURL, herr)
		}
	}
	if LaunchApp() != nil {
		return nil, ErrNotRunning
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if c, err := Resolve(admin); err == nil && c.Healthy(ctx) {
			return c, nil
		}
	}
	return nil, ErrNotRunning
}

// LaunchApp asks macOS to start Pulse.app without bringing it to the front.
// It does nothing when PULSE_URL points at another machine's app.
func LaunchApp() error {
	if runtime.GOOS != "darwin" {
		return errors.New("not macOS")
	}
	if os.Getenv("PULSE_URL") != "" {
		return errors.New("using a remote Pulse (PULSE_URL)")
	}
	return exec.Command("open", "-g", "-b", paths.BundleID).Run()
}

// Healthy reports whether the app answers its health endpoint.
func (c *Client) Healthy(ctx context.Context) bool { return c.Health(ctx) == nil }

// Health asks the app's health endpoint; the error says why it didn't
// answer (unreachable, or refused by something in front of it).
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1/health", nil)
	c.addHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if msg := interceptedBy(resp); msg != "" {
		return &APIError{Status: resp.StatusCode, Msg: msg, Intercepted: true}
	}
	if resp.StatusCode != 200 {
		return &APIError{Status: resp.StatusCode, Msg: "health check failed"}
	}
	return nil
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Msg    string
	// Intercepted: the response came from something in front of the app
	// (a proxy or login page), not the app, so it says nothing about the data.
	Intercepted bool
	// RetryAfter is when to try again, for a 429 (over a limit).
	RetryAfter time.Duration
}

// Limited reports whether err is the app refusing for now (over a limit).
func Limited(err error) (time.Duration, bool) {
	var e *APIError
	if errors.As(err, &e) && !e.Intercepted && e.Status == http.StatusTooManyRequests {
		return max(e.RetryAfter, time.Second), true
	}
	return 0, false
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

// Do sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	c.addHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if msg := interceptedBy(resp); msg != "" {
		return &APIError{Status: resp.StatusCode, Msg: msg, Intercepted: true}
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		ae := &APIError{Status: resp.StatusCode, Msg: e.Error}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		return ae
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) addHeader(req *http.Request) {
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// interceptedBy explains a response that came from something in front of
// the app rather than the app itself ("" if it looks like the app's own).
func interceptedBy(resp *http.Response) string {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if loc != nil && strings.HasSuffix(loc.Hostname(), ".cloudflareaccess.com") {
			return fmt.Sprintf("Cloudflare Access asked for a login: set %s and %s to a service token", EnvAccessID, EnvAccessSecret)
		}
		return fmt.Sprintf("unexpected redirect to %s (is PULSE_URL right?)", resp.Header.Get("Location"))
	}
	// Pulse answers every API error in JSON; anything else came from in front of it.
	if resp.StatusCode < 400 || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return ""
	}
	if resp.Header.Get("Cf-Ray") != "" {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Sprintf("Cloudflare refused the request (HTTP %d) before it reached Pulse: check %s and %s", resp.StatusCode, EnvAccessID, EnvAccessSecret)
		}
		return fmt.Sprintf("Cloudflare answered HTTP %d: Pulse or its tunnel isn't reachable right now", resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d from something in front of Pulse, not Pulse itself (a proxy or tunnel)", resp.StatusCode)
}

// WaitCommands long-polls the app for commands addressed to a run.
func (c *Client) WaitCommands(ctx context.Context, runID string, wait time.Duration, accepts ...string) ([]model.Command, error) {
	lc := *c
	lc.HTTP = newHTTP(wait + 10*time.Second)
	var out struct {
		Commands []model.Command `json:"commands"`
	}
	q := "/api/v1/runs/" + url.PathEscape(runID) + "/commands?wait=" + wait.String()
	if len(accepts) > 0 {
		q += "&accepts=" + url.QueryEscape(strings.Join(accepts, ","))
	}
	err := lc.Do(ctx, "GET", q, nil, &out)
	return out.Commands, err
}

// AckCommand tells the app a command was handled.
func (c *Client) AckCommand(ctx context.Context, runID, cmdID string) error {
	return c.Do(ctx, "POST", "/api/v1/runs/"+url.PathEscape(runID)+"/commands/"+url.PathEscape(cmdID)+"/ack", nil, nil)
}

// WithTimeout returns a copy of c whose requests may take up to d.
func (c *Client) WithTimeout(d time.Duration) *Client {
	cc := *c
	cc.HTTP = newHTTP(d)
	return &cc
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch h := u.Hostname(); h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
