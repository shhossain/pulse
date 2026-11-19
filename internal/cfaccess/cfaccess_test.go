package cfaccess

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	team = "myteam.cloudflareaccess.com"
	aud  = "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2"
)

type signer struct {
	key *rsa.PrivateKey
	kid string
}

func newSigner(t *testing.T, kid string) signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{k, kid}
}

func seg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s signer) token(t *testing.T, hdr, claims map[string]any) string {
	t.Helper()
	if hdr == nil {
		hdr = map[string]any{"alg": "RS256", "kid": s.kid, "typ": "JWT"}
	}
	in := seg(hdr) + "." + seg(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func certs(signers ...signer) map[string]any {
	var keys []map[string]any
	for _, s := range signers {
		keys = append(keys, map[string]any{
			"kid": s.kid, "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes()),
		})
	}
	return map[string]any{"keys": keys}
}

func setup(t *testing.T, published ...signer) (*Verifier, *atomic.Int32, time.Time) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(certs(published...))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	return &Verifier{Team: team, AUD: aud, CertsURL: srv.URL, Now: func() time.Time { return now }}, &hits, now
}

func claims(now time.Time, over map[string]any) map[string]any {
	c := map[string]any{
		"aud": []string{aud}, "iss": "https://" + team, "sub": "u-1", "email": "Me@Example.com",
		"iat": now.Unix() - 10, "nbf": now.Unix() - 10, "exp": now.Unix() + 3600, "type": "app",
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestVerifyAcceptsAValidUserToken(t *testing.T) {
	s := newSigner(t, "k1")
	v, _, now := setup(t, s)
	id, err := v.Verify(context.Background(), s.token(t, nil, claims(now, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "me@example.com" || id.Subject != "u-1" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestVerifyServiceToken(t *testing.T) {
	s := newSigner(t, "k1")
	v, _, now := setup(t, s)
	id, err := v.Verify(context.Background(), s.token(t, nil, claims(now, map[string]any{"email": "", "common_name": "abc.access", "sub": ""})))
	if err != nil || id.Email != "" || id.Service != "abc.access" {
		t.Fatalf("service token: %+v %v", id, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	good := newSigner(t, "k1")
	other := newSigner(t, "k1") // same key ID, different key: a forgery
	v, _, now := setup(t, good)
	ctx := context.Background()
	valid := good.token(t, nil, claims(now, nil))
	parts := strings.Split(valid, ".")
	cases := map[string]string{
		"empty":                 "",
		"garbage":               "a.b.c",
		"two parts":             parts[0] + "." + parts[1],
		"signed by another key": other.token(t, nil, claims(now, nil)),
		"tampered claims":       parts[0] + "." + seg(claims(now, map[string]any{"email": "attacker@evil.com"})) + "." + parts[2],
		"alg none":              seg(map[string]any{"alg": "none", "kid": "k1"}) + "." + parts[1] + ".",
		"alg HS256":             good.token(t, map[string]any{"alg": "HS256", "kid": "k1"}, claims(now, nil)),
		"unknown key":           good.token(t, map[string]any{"alg": "RS256", "kid": "k9"}, claims(now, nil)),
		"other application":     good.token(t, nil, claims(now, map[string]any{"aud": []string{"someone-else"}})),
		"no audience":           good.token(t, nil, claims(now, map[string]any{"aud": nil})),
		"other team":            good.token(t, nil, claims(now, map[string]any{"iss": "https://evil.cloudflareaccess.com"})),
		"expired":               good.token(t, nil, claims(now, map[string]any{"exp": now.Unix() - 120})),
		"no expiry":             good.token(t, nil, claims(now, map[string]any{"exp": nil})),
		"not yet valid":         good.token(t, nil, claims(now, map[string]any{"nbf": now.Unix() + 600})),
		"names no one":          good.token(t, nil, claims(now, map[string]any{"email": "", "common_name": nil})),
		"oversized":             strings.Repeat("a", 20<<10),
	}
	for name, tok := range cases {
		if id, err := v.Verify(ctx, tok); err == nil {
			t.Errorf("%s: accepted as %+v", name, id)
		}
	}
	if _, err := v.Verify(ctx, good.token(t, nil, claims(now, map[string]any{"aud": aud}))); err != nil {
		t.Errorf("aud as a plain string: %v", err)
	}
}

func TestKeysAreCachedAndRefetchIsRateLimited(t *testing.T) {
	s := newSigner(t, "k1")
	v, hits, now := setup(t, s)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(ctx, s.token(t, nil, claims(now, nil))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ { // bogus key IDs must not hammer the certs endpoint
		v.Verify(ctx, s.token(t, map[string]any{"alg": "RS256", "kid": "bogus"}, claims(now, nil)))
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("certs fetched %d times, want 1", n)
	}
}

func TestUnreachableCertsFailClosed(t *testing.T) {
	s := newSigner(t, "k1")
	now := time.Now()
	v := &Verifier{Team: team, AUD: aud, CertsURL: "http://127.0.0.1:1/certs"}
	if _, err := v.Verify(context.Background(), s.token(t, nil, claims(now, nil))); err == nil {
		t.Fatal("verified without being able to fetch keys")
	}
}

func TestKeysThatCantBeRefreshedExpire(t *testing.T) {
	s := newSigner(t, "k1")
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", 503)
			return
		}
		json.NewEncoder(w).Encode(certs(s))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	v := &Verifier{Team: team, AUD: aud, CertsURL: srv.URL, Now: func() time.Time { return now }}
	ctx := context.Background()
	if _, err := v.Verify(ctx, s.token(t, nil, claims(now, nil))); err != nil {
		t.Fatal(err)
	}
	up = false
	now = now.Add(7 * time.Hour) // stale, refresh fails: still within the hard limit
	if _, err := v.Verify(ctx, s.token(t, nil, claims(now, nil))); err != nil {
		t.Fatalf("stale keys within 24h: %v", err)
	}
	now = now.Add(18 * time.Hour) // 25h old
	if _, err := v.Verify(ctx, s.token(t, nil, claims(now, nil))); err == nil {
		t.Fatal("trusted keys that couldn't be refreshed for over a day")
	}
	up = true
	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(ctx, s.token(t, nil, claims(now, nil))); err != nil {
		t.Fatalf("after Cloudflare is back: %v", err)
	}
}

func TestFirstFetchFailureIsRateLimitedToo(t *testing.T) {
	s := newSigner(t, "k1")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", 503)
	}))
	defer srv.Close()
	now := time.Now()
	v := &Verifier{Team: team, AUD: aud, CertsURL: srv.URL, Now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		v.Verify(context.Background(), s.token(t, nil, claims(now, nil)))
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("certs fetched %d times while down, want 1 a minute", n)
	}
}

// After a restart, many requests arrive before the keys are fetched: one
// fetch serves them all, and a caller hanging up doesn't fail it for others.
func TestColdStartSharesOneFetch(t *testing.T) {
	s := newSigner(t, "k1")
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // slow Cloudflare
		json.NewEncoder(w).Encode(certs(s))
	}))
	defer srv.Close()
	now := time.Now()
	v := &Verifier{Team: team, AUD: aud, CertsURL: srv.URL, Now: func() time.Time { return now }}
	tok := s.token(t, nil, claims(now, nil))

	gone, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 11)
	go func() { _, err := v.Verify(gone, tok); errs <- err }() // the phone that hangs up
	for i := 0; i < 10; i++ {
		go func() { _, err := v.Verify(context.Background(), tok); errs <- err }()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	failed := 0
	for i := 0; i < 11; i++ {
		if <-errs != nil {
			failed++
		}
	}
	if failed != 1 || hits.Load() != 1 {
		t.Fatalf("%d failed (want only the one that hung up), %d fetches (want 1)", failed, hits.Load())
	}
}
