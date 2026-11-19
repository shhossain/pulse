package hubctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Cloudflare is the small part of Cloudflare's API that per-machine Access
// service tokens need. The API token needs "Access: Service Tokens: Edit"
// and "Access: Apps and Policies: Edit" on the account.
type Cloudflare struct {
	Token string
	Conf  CloudflareConfig
	Base  string // API base (tests); default https://api.cloudflare.com/client/v4
	HTTP  *http.Client
}

// ServiceToken is a new service token; the secret is only ever shown once.
type ServiceToken struct {
	ID           string    `json:"id"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// errLastRule: removing the rule would leave the policy empty (invalid).
var errLastRule = errors.New("that's the policy's only rule")

func (c *Cloudflare) do(ctx context.Context, method, path string, in, out any) error {
	base := c.Base
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if !env.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			msg += " (the API token needs Access: Service Tokens and Access: Apps and Policies, Edit)"
		}
		return fmt.Errorf("cloudflare: %s", msg)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *Cloudflare) acct() string { return "/accounts/" + url.PathEscape(c.Conf.Account) }

// CreateServiceToken makes a service token valid for a year.
func (c *Cloudflare) CreateServiceToken(ctx context.Context, name string) (ServiceToken, error) {
	var st ServiceToken
	err := c.do(ctx, "POST", c.acct()+"/access/service_tokens", map[string]any{"name": name, "duration": "8760h"}, &st)
	if err == nil && (st.ID == "" || st.ClientID == "" || st.ClientSecret == "") {
		err = errors.New("cloudflare returned an incomplete service token")
	}
	return st, err
}

// DeleteServiceToken deletes one.
func (c *Cloudflare) DeleteServiceToken(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", c.acct()+"/access/service_tokens/"+url.PathEscape(id), nil, nil)
}

func (c *Cloudflare) policyPath() string {
	return c.acct() + "/access/apps/" + url.PathEscape(c.Conf.App) + "/policies/" + url.PathEscape(c.Conf.Policy)
}

// editPolicy reads the service-auth policy, changes its include list with
// edit, and writes it back (only if it changed).
// The whole policy is kept as Cloudflare returns it (only the include list
// changes), so settings this code doesn't know about survive the PUT.
func (c *Cloudflare) editPolicy(ctx context.Context, edit func([]map[string]any) []map[string]any) error {
	var p map[string]any
	if err := c.do(ctx, "GET", c.policyPath(), nil, &p); err != nil {
		return err
	}
	if p["decision"] != "non_identity" {
		return fmt.Errorf("policy %q is %v, not a Service Auth policy", p["name"], p["decision"])
	}
	var include []map[string]any
	b, _ := json.Marshal(p["include"])
	_ = json.Unmarshal(b, &include)
	after := edit(include)
	if a, _ := json.Marshal(after); bytes.Equal(b, a) {
		return nil
	}
	if len(after) == 0 {
		return errLastRule
	}
	p["include"] = after
	for _, k := range []string{"id", "uid", "created_at", "updated_at", "app_count", "reusable"} {
		delete(p, k) // read-only
	}
	return c.do(ctx, "PUT", c.policyPath(), p, nil)
}

func tokenRule(r map[string]any) string {
	st, _ := r["service_token"].(map[string]any)
	id, _ := st["token_id"].(string)
	return id
}

// AllowServiceToken adds the token to the service-auth policy.
func (c *Cloudflare) AllowServiceToken(ctx context.Context, id string) error {
	return c.editPolicy(ctx, func(rules []map[string]any) []map[string]any {
		for _, r := range rules {
			if tokenRule(r) == id {
				return rules
			}
		}
		return append(rules, map[string]any{"service_token": map[string]any{"token_id": id}})
	})
}

// DisallowServiceToken removes it.
func (c *Cloudflare) DisallowServiceToken(ctx context.Context, id string) error {
	return c.editPolicy(ctx, func(rules []map[string]any) []map[string]any {
		out := rules[:0:0]
		for _, r := range rules {
			if tokenRule(r) != id {
				out = append(out, r)
			}
		}
		return out
	})
}
