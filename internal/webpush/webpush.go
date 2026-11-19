// Package webpush sends Web Push messages: payloads encrypted for one
// browser (RFC 8291, aes128gcm), so the push service relaying them can't
// read them, and signed with this server's VAPID key (RFC 8292). Standard
// library only.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Subscription is what a browser's PushManager.subscribe() returns.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushHosts are the push services browsers use. The server only ever posts
// to these, so a subscription can't make it send requests anywhere else.
var pushHosts = []string{
	"fcm.googleapis.com",                // Chrome, Edge, Android
	"updates.push.services.mozilla.com", // Firefox
	"web.push.apple.com",                // Safari
}

var pushSuffixes = []string{".push.services.mozilla.com", ".notify.windows.com", ".push.apple.com"}

// Validate checks a subscription before it's stored.
func (s Subscription) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || len(s.Endpoint) > 2048 {
		return errors.New("endpoint must be an https URL of a browser push service")
	}
	host := strings.ToLower(u.Hostname())
	known := false
	for _, h := range pushHosts {
		known = known || host == h
	}
	for _, suf := range pushSuffixes {
		known = known || strings.HasSuffix(host, suf)
	}
	if !known {
		return fmt.Errorf("%s isn't a known browser push service", host)
	}
	pub, err1 := b64(s.Keys.P256dh)
	auth, err2 := b64(s.Keys.Auth)
	if err1 != nil || err2 != nil || len(auth) != 16 {
		return errors.New("subscription keys are malformed")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("subscription key isn't a P-256 point")
	}
	return nil
}

// Keys is this server's VAPID key pair.
type Keys struct {
	priv *ecdsa.PrivateKey
}

// NewKeys makes a new VAPID key pair.
func NewKeys() (*Keys, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Keys{k}, nil
}

// LoadKeys reads a key saved with Marshal.
func LoadKeys(b []byte) (*Keys, error) {
	var s struct {
		D string `json:"d"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	d, err := b64(s.D)
	if err != nil || len(d) != 32 {
		return nil, errors.New("malformed VAPID key")
	}
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, err
	}
	return &Keys{k}, nil
}

// Marshal serializes the private key (keep it secret: it signs pushes).
func (k *Keys) Marshal() []byte {
	d, _ := k.priv.Bytes()
	b, _ := json.Marshal(map[string]string{"d": base64.RawURLEncoding.EncodeToString(d)})
	return b
}

// Public is the application server key browsers subscribe with.
func (k *Keys) Public() string {
	pub, _ := k.priv.PublicKey.Bytes()
	return base64.RawURLEncoding.EncodeToString(pub)
}

// vapid returns the Authorization header for endpoint (RFC 8292).
func (k *Keys) vapid(endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(map[string]string{"typ": "JWT", "alg": "ES256"}) + "." +
		enc(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	sum := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + in + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + k.Public(), nil
}

// ErrGone means the subscription no longer exists (the browser unsubscribed
// or the app was uninstalled): forget it.
var ErrGone = errors.New("subscription is gone")

// ErrRejected means the push service refuses this sender for the
// subscription (e.g. it was made with another key): it can't be used.
var ErrRejected = errors.New("push service rejected this subscription")

// Message is one push.
type Message struct {
	Payload []byte
	TTL     time.Duration // how long the push service keeps it for an offline device
	Urgent  bool
	Topic   string // a newer message with the same topic replaces an undelivered one
}

// MaxPayload is the largest payload that fits one aes128gcm record.
const MaxPayload = 3000

// Send delivers m to sub. subject identifies the sender to push services
// (an https: or mailto: URL).
func (k *Keys) Send(ctx context.Context, client *http.Client, sub Subscription, m Message, subject string) error {
	if err := sub.Validate(); err != nil {
		return err
	}
	if len(m.Payload) > MaxPayload {
		return errors.New("payload too large")
	}
	uaPub, _ := b64(sub.Keys.P256dh)
	auth, _ := b64(sub.Keys.Auth)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	body, err := encrypt(m.Payload, uaPub, auth, as, salt)
	if err != nil {
		return err
	}
	authz, err := k.vapid(sub.Endpoint, subject, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL.Seconds())))
	if m.Urgent {
		req.Header.Set("Urgency", "high")
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	cc := *client
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	case resp.StatusCode >= 300:
		return fmt.Errorf("push service: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// encrypt is RFC 8291 with one aes128gcm record (RFC 8188).
func encrypt(plain, uaPub, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, err
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Key(sha256.New, secret, auth, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := append(append([]byte{}, plain...), 0x02) // the last (only) record's delimiter
	ct := gcm.Seal(nil, nonce, record, nil)

	const rs = 4096
	out := make([]byte, 0, 16+4+1+len(asPub)+len(ct))
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, rs)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return append(out, ct...), nil
}

func b64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
