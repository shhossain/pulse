// Package cfaccess verifies Cloudflare Access application tokens: the JWT
// Cloudflare adds to every request it lets through to an origin
// (Cf-Access-Jwt-Assertion). Checking it at the origin means a request is
// trusted only if Access really approved it for this application, even if
// the Access setup in front is changed or bypassed.
//
// Standard library only: RS256 signatures, keys from the team's certs
// endpoint, cached.
package cfaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Header is where Cloudflare puts the application token.
const Header = "Cf-Access-Jwt-Assertion"

// Identity is who an approved request comes from.
type Identity struct {
	Email   string // set for a person who signed in; empty for a service token
	Subject string
	Service string // the service token's client ID, for a service token
}

// Verifier checks tokens for one Access application.
type Verifier struct {
	Team string // e.g. "myteam.cloudflareaccess.com"
	AUD  string // the application's audience tag

	// CertsURL overrides https://<Team>/cdn-cgi/access/certs (tests).
	CertsURL string
	HTTP     *http.Client
	Now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetched   time.Time     // last successful fetch
	attempted time.Time     // last fetch attempt (to rate-limit refetches)
	inflight  chan struct{} // closed when the running fetch ends
	lastErr   error         // of the last fetch
}

const (
	keysFresh   = 6 * time.Hour    // refetch keys this often (Cloudflare rotates them)
	maxKeyAge   = 24 * time.Hour   // never trust keys older than this
	refetchWait = 1 * time.Minute  // at most this often (unknown key IDs, failures)
	coldRetry   = 10 * time.Second // while no keys were ever fetched
	clockSkew   = 60 * time.Second
	maxToken    = 32 << 10 // a real token is 1-2 KB; group claims can make it bigger
)

// Verify checks token and returns who it's for. Any doubt is an error:
// the caller must then treat the request as unauthenticated.
func (v *Verifier) Verify(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, errors.New("no token")
	}
	if len(token) > maxToken {
		return Identity{}, errors.New("token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return Identity{}, fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != "RS256" { // never "none", never HMAC with a public key
		return Identity{}, fmt.Errorf("unexpected algorithm %q", hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, errors.New("malformed signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Identity{}, errors.New("bad signature")
	}

	var c struct {
		Aud        audience `json:"aud"`
		Iss        string   `json:"iss"`
		Exp        float64  `json:"exp"`
		Nbf        float64  `json:"nbf"`
		Iat        float64  `json:"iat"`
		Sub        string   `json:"sub"`
		Email      string   `json:"email"`
		CommonName string   `json:"common_name"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return Identity{}, fmt.Errorf("claims: %w", err)
	}
	now := v.now()
	switch {
	case c.Iss != "https://"+v.Team:
		return Identity{}, fmt.Errorf("issuer %q isn't this team", c.Iss)
	case !c.Aud.has(v.AUD):
		return Identity{}, errors.New("token is for another application")
	case c.Exp == 0 || now.After(unix(c.Exp).Add(clockSkew)):
		return Identity{}, errors.New("token expired")
	case c.Nbf != 0 && now.Add(clockSkew).Before(unix(c.Nbf)):
		return Identity{}, errors.New("token not valid yet")
	case c.Iat != 0 && now.Add(clockSkew).Before(unix(c.Iat)):
		return Identity{}, errors.New("token issued in the future")
	}
	id := Identity{Email: strings.ToLower(strings.TrimSpace(c.Email)), Subject: c.Sub, Service: c.CommonName}
	if id.Email == "" && id.Service == "" {
		return Identity{}, errors.New("token names no one")
	}
	return id, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// key returns the signing key kid. Keys are refetched when they're older
// than keysFresh or kid is new: by one fetch at a time, not tied to any
// request (a caller hanging up doesn't fail it), which concurrent callers
// wait for; and at most once a minute (10 seconds while there are no keys
// at all), so bogus key IDs can't hammer Cloudflare. If refreshing keeps
// failing, keys are trusted for at most maxKeyAge, then every token is refused.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	for waited := false; ; waited = true {
		now := v.now()
		v.mu.Lock()
		k, ok := v.keys[kid]
		fetched, lastErr := v.fetched, v.lastErr
		var wait chan struct{}
		if !waited && (!ok || now.Sub(fetched) > keysFresh) {
			retry := refetchWait
			if v.keys == nil {
				retry = coldRetry
			}
			switch {
			case v.inflight != nil:
				wait = v.inflight
			case now.Sub(v.attempted) >= retry:
				v.attempted = now
				v.inflight = make(chan struct{})
				wait = v.inflight
				go v.refresh(wait)
			}
		}
		v.mu.Unlock()
		if wait != nil {
			select {
			case <-wait:
				continue // look again with what the fetch brought
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		switch {
		case !ok && lastErr != nil && v.keys == nil:
			return nil, fmt.Errorf("access keys: %w", lastErr)
		case !ok:
			return nil, errors.New("unknown signing key")
		case now.Sub(fetched) > maxKeyAge:
			return nil, errors.New("access keys are too old to trust and can't be refreshed")
		}
		return k, nil
	}
}

// refresh fetches the keys once and wakes whoever waits on done.
func (v *Verifier) refresh(done chan struct{}) {
	keys, err := v.fetch(context.Background())
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		v.keys, v.fetched = keys, v.now()
	}
	v.lastErr = err
	v.inflight = nil
	close(done)
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	u := v.CertsURL
	if u == "" {
		u = "https://" + v.Team + "/cdn-cgi/access/certs"
	}
	c := v.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pk := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pk.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no usable keys", u)
	}
	return keys, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("bad encoding")
	}
	return json.Unmarshal(b, v)
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

// audience is "aud": a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(s string) bool {
	for _, x := range a {
		if x == s && s != "" {
			return true
		}
	}
	return false
}
