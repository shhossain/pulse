package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291, Appendix A: the worked example, byte for byte.
func TestEncryptMatchesRFC8291(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt([]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), as, mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := base64.RawURLEncoding.EncodeToString(got); g != want {
		t.Fatalf("got  %s\nwant %s", g, want)
	}
}

func TestValidate(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(nil)
	sub := func(endpoint string) Subscription {
		var s Subscription
		s.Endpoint = endpoint
		s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
		s.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
		return s
	}
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc:def",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://web.push.apple.com/QOx",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
	} {
		if err := sub(ok).Validate(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/x",     // clear text
		"https://169.254.169.254/latest/meta-data", // the cloud metadata service
		"https://127.0.0.1:7777/api/v1/runs",       // the hub itself
		"https://fcm.googleapis.com.evil.com/x",    // lookalike
		"https://fcm.googleapis.com:8443/x",        // odd port
		"https://user@fcm.googleapis.com/x",        // credentials
		"https://evilnotify.windows.com/x",         // suffix without the dot
	} {
		if err := sub(bad).Validate(); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	s := sub("https://fcm.googleapis.com/fcm/send/x")
	s.Keys.Auth = "short"
	if s.Validate() == nil {
		t.Error("malformed auth secret accepted")
	}
	s = sub("https://fcm.googleapis.com/fcm/send/x")
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(make([]byte, 65))
	if s.Validate() == nil {
		t.Error("a key that isn't on the curve accepted")
	}
}

func TestKeysRoundTripAndVAPID(t *testing.T) {
	k, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadKeys(k.Marshal())
	if err != nil || k2.Public() != k.Public() {
		t.Fatalf("reload: %v", err)
	}
	h, err := k.vapid("https://fcm.googleapis.com/fcm/send/x", "https://pulse.example.com", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimPrefix(strings.Split(h, ", k=")[0], "vapid t=")
	parts := strings.Split(tok, ".")
	var claims map[string]any
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(b, &claims)
	if claims["aud"] != "https://fcm.googleapis.com" || claims["sub"] != "https://pulse.example.com" {
		t.Fatalf("claims = %v", claims)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(k.Public())
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck // test-only point decode
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature doesn't verify")
	}
}

func TestSendHeadersAndGone(t *testing.T) {
	k, _ := NewKeys()
	ua, _ := ecdh.P256().GenerateKey(nil)
	var got *http.Request
	var body []byte
	status := 201
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	var sub Subscription
	sub.Endpoint = "https://fcm.googleapis.com/fcm/send/x"
	sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	sub.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	// Route the push service's host to the test server.
	c := srv.Client()
	tr := c.Transport.(*http.Transport).Clone()
	addr := srv.Listener.Addr().String()
	tr.DialTLSContext = nil
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	tr.TLSClientConfig.InsecureSkipVerify = true
	c = &http.Client{Transport: tr}
	err := k.Send(context.Background(), c, sub, Message{Payload: []byte(`{"title":"t"}`), TTL: time.Hour, Urgent: true, Topic: "r1"}, "https://pulse.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") != "3600" ||
		got.Header.Get("Urgency") != "high" || !strings.HasPrefix(got.Header.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers = %v", got.Header)
	}
	if strings.Contains(string(body), `"title"`) {
		t.Fatal("payload sent in clear text")
	}
	status = 410
	if err := k.Send(context.Background(), c, sub, Message{Payload: []byte("x")}, "https://pulse.example.com"); err != ErrGone {
		t.Fatalf("410: %v", err)
	}
}
