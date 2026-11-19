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
}

// Resolve finds the app without contacting it. PULSE_URL / PULSE_TOKEN take
// precedence (remote machines); otherwise the local server.json and token
// files are used. admin selects the admin token (local only).
func Resolve(admin bool) (*Client, error) {
	c := &Client{HTTP: &http.Client{Timeout: 5 * time.Second}}
	if u := os.Getenv("PULSE_URL"); u != "" {
		c.BaseURL = strings.TrimRight(u, "/")
		c.Token = os.Getenv("PULSE_TOKEN")
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

func localToken(admin bool) string {
	if admin {
		return paths.ReadToken(paths.AdminToken())
	}
	return paths.ReadToken(paths.PushToken())
}

// Connect resolves the app and checks it is alive. If it is not, and this is
// a Mac, it launches Pulse.app in the background and waits up to wait.
func Connect(ctx context.Context, admin bool, wait time.Duration) (*Client, error) {
	if c, err := Resolve(admin); err == nil && c.Healthy(ctx) {
		return c, nil
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
func (c *Client) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1/health", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Msg    string
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// WaitCommands long-polls the app for commands addressed to a run.
func (c *Client) WaitCommands(ctx context.Context, runID string, wait time.Duration, accepts ...string) ([]model.Command, error) {
	lc := *c
	lc.HTTP = &http.Client{Timeout: wait + 10*time.Second}
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
	cc.HTTP = &http.Client{Timeout: d}
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
