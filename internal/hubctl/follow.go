package hubctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"pulse/internal/follow"
)

// FollowService is where the follower's service-token secret is kept.
const FollowService = "pulse-hub-follow"

// Follow lets this computer's Pulse show the hub's runs: a view-only
// Access service token of its own, listed on the hub as a viewer, its
// secret in the Keychain. The menubar app picks it up within 30 seconds.
func Follow(ctx context.Context, c Config, cf *Cloudflare, run Runner, label string, ready func(context.Context, follow.Config, string) error) (fc follow.Config, err error) {
	if c.Cloudflare == nil || cf == nil {
		return fc, errors.New("following needs the hub behind Cloudflare Access and a Cloudflare API token (pulse hub setup)")
	}
	unlock, err := lock()
	if err != nil {
		return fc, err
	}
	defer unlock()
	if _, ok := follow.Load(); ok { // under the lock: two follows can't both pass
		return fc, errors.New("already following a hub: pulse hub unfollow first")
	}
	var undo []func()
	defer func() {
		var notReady ReadyErr // set up, just not confirmed yet: keep it
		if err != nil && !errors.As(err, &notReady) {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	st, err := cf.CreateServiceToken(ctx, "pulse: "+label+" (view only)")
	if err != nil {
		return fc, fmt.Errorf("creating the Cloudflare service token: %w", err)
	}
	undo = append(undo, func() { _ = cf.DeleteServiceToken(context.Background(), st.ID) })
	if err := cf.AllowServiceToken(ctx, st.ID); err != nil {
		return fc, fmt.Errorf("allowing it through Access: %w", err)
	}
	undo = append(undo, func() { _ = cf.DisallowServiceToken(context.Background(), st.ID) })
	if _, err := c.admin(ctx, run, "remote", "web", "--add-service", st.ClientID); err != nil {
		return fc, fmt.Errorf("letting it view on the hub (is the web UI on? pulse-admin remote web …): %w", err)
	}
	undo = append(undo, func() { _, _ = c.admin(context.Background(), run, "remote", "web", "--remove-service", st.ClientID) })
	if _, err := SaveSecret(run, FollowService, st.ClientID, st.ClientSecret); err != nil {
		return fc, err
	}
	undo = append(undo, func() { DeleteSecret(run, FollowService, st.ClientID) })
	fc = follow.Config{URL: strings.TrimRight(c.URL, "/"), ClientID: st.ClientID, CFTokenID: st.ID, Expires: st.ExpiresAt}
	if err := follow.Save(fc); err != nil {
		return fc, err
	}
	undo = append(undo, func() { _ = follow.Remove() })
	if ready != nil {
		if rerr := ready(ctx, fc, st.ClientSecret); rerr != nil {
			return fc, ReadyErr{rerr}
		}
	}
	return fc, nil
}

// Unfollow undoes Follow.
func Unfollow(ctx context.Context, c Config, cf *Cloudflare, run Runner) (follow.Config, error) {
	fc, ok := follow.Load()
	if !ok {
		return fc, errors.New("not following a hub")
	}
	unlock, err := lock()
	if err != nil {
		return fc, err
	}
	defer unlock()
	// The hub may have no web UI or tunnel any more: nothing to remove there.
	if _, err := c.admin(ctx, run, "remote", "web", "--remove-service", fc.ClientID); err != nil &&
		!strings.Contains(err.Error(), "turn the web UI on first") && !strings.Contains(err.Error(), "needs a tunnel") {
		return fc, fmt.Errorf("removing it from the hub: %w", err)
	}
	if fc.CFTokenID != "" {
		if cf == nil {
			return fc, errors.New("a Cloudflare API token is needed to remove its service token")
		}
		if err := cf.DisallowServiceToken(ctx, fc.CFTokenID); err != nil && !errors.Is(err, errLastRule) {
			return fc, fmt.Errorf("removing it from Access: %w", err)
		}
		if err := cf.DeleteServiceToken(ctx, fc.CFTokenID); err != nil && !strings.Contains(err.Error(), "not found") {
			return fc, fmt.Errorf("deleting its service token: %w", err)
		}
	}
	DeleteSecret(run, FollowService, fc.ClientID)
	return fc, follow.Remove()
}

// FollowReady waits until the hub serves this computer as a viewer.
func FollowReady(timeout time.Duration) func(context.Context, follow.Config, string) error {
	return func(ctx context.Context, fc follow.Config, secret string) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		hc := &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for {
			req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(fc.URL, "/")+"/api/v1/session", nil)
			req.Header.Set("User-Agent", "pulse-follow")
			req.Header.Set("CF-Access-Client-Id", fc.ClientID)
			req.Header.Set("CF-Access-Client-Secret", secret)
			if resp, err := hc.Do(req); err == nil {
				ok := resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
				resp.Body.Close()
				if ok {
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return errors.New("the hub doesn't show this computer its runs yet; Cloudflare can take a minute")
			case <-time.After(3 * time.Second):
			}
		}
	}
}

// Hostname is a label for this computer.
func Hostname() string {
	h, _ := os.Hostname()
	h = strings.TrimSuffix(h, ".local")
	if h == "" {
		h = "this computer"
	}
	return h
}
