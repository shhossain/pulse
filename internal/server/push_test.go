package server

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/model"
	"pulse/internal/webpush"
)

func testSub(t *testing.T, endpoint string) string {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(nil)
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "keys": map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
	}})
	return string(b)
}

type sentPush struct {
	endpoint string
	payload  pushPayload
}

// fakePush records deliveries instead of calling push services.
func fakePush(t *testing.T, s *Server, gone map[string]bool) func() []sentPush {
	t.Helper()
	p := s.pushes()
	if p == nil {
		t.Fatal("no pusher")
	}
	var mu sync.Mutex
	var sent []sentPush
	p.send = func(_ context.Context, sub webpush.Subscription, m webpush.Message) error {
		if gone[sub.Endpoint] {
			return webpush.ErrGone
		}
		var pl pushPayload
		json.Unmarshal(m.Payload, &pl)
		mu.Lock()
		sent = append(sent, sentPush{sub.Endpoint, pl})
		mu.Unlock()
		return nil
	}
	return func() []sentPush { mu.Lock(); defer mu.Unlock(); return append([]sentPush(nil), sent...) }
}

func TestViewerManagesOwnPushSubscription(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s, remote, a := webServer(t)
	sent := fakePush(t, s, nil)
	me := a.token(t, "me@example.com", "")
	hdr := []string{"Cf-Access-Jwt-Assertion", me, "Origin", testURL}
	ep := "https://fcm.googleapis.com/fcm/send/device-1"

	r := do(t, "GET", remote.URL+"/api/v1/web/push", "", "", hdr[:2]...)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "public_key") {
		t.Fatalf("status: %d %s", r.StatusCode, b)
	}
	cases := []struct {
		name   string
		status int
		hdr    []string
		body   string
	}{
		{"no Origin (not our page)", 403, hdr[:2], testSub(t, ep)},
		{"another site's page", 403, []string{"Cf-Access-Jwt-Assertion", me, "Origin", "https://evil.example"}, testSub(t, ep)},
		{"not a push service", 400, hdr, testSub(t, "https://169.254.169.254/latest")},
		{"subscribe", 200, hdr, testSub(t, ep)},
	}
	for _, c := range cases {
		if r := do(t, "POST", remote.URL+"/api/v1/web/push", "", c.body, c.hdr...); r.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.name, r.StatusCode, c.status)
		}
	}
	r = do(t, "GET", remote.URL+"/api/v1/web/push?endpoint="+ep, "", "", hdr[:2]...)
	b, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"subscribed":true`) {
		t.Fatalf("after subscribing: %s", b)
	}
	if r := do(t, "POST", remote.URL+"/api/v1/web/push/test", "", "", hdr...); r.StatusCode != 200 {
		t.Fatalf("test push: %d", r.StatusCode)
	}
	if got := sent(); len(got) != 1 || got[0].endpoint != ep {
		t.Fatalf("test push sent %v", got)
	}
	if r := do(t, "POST", remote.URL+"/api/v1/web/push/test", "", "", hdr...); r.StatusCode != 429 {
		t.Errorf("second test right away: %d", r.StatusCode)
	}
	// Someone else can't remove my device.
	other := a.token(t, "other@example.com", "")
	s.web.emails["other@example.com"] = true
	do(t, "POST", remote.URL+"/api/v1/web/push/unsubscribe", "", `{"endpoint":"`+ep+`"}`, "Cf-Access-Jwt-Assertion", other, "Origin", testURL)
	if !s.pushes().has("me@example.com", ep) {
		t.Fatal("another viewer removed my subscription")
	}
	if r := do(t, "POST", remote.URL+"/api/v1/web/push/unsubscribe", "", `{"endpoint":"`+ep+`"}`, hdr...); r.StatusCode != 200 || s.pushes().has("me@example.com", ep) {
		t.Fatalf("unsubscribe: %d", r.StatusCode)
	}
}

func TestPushNotifierSendsOnlyToListedViewers(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s, _, _ := webServer(t)
	gone := map[string]bool{"https://fcm.googleapis.com/fcm/send/old-phone": true}
	sent := fakePush(t, s, gone)
	p := s.pushes()
	for email, ep := range map[string]string{
		"me@example.com":      "https://fcm.googleapis.com/fcm/send/phone",
		"removed@example.com": "https://fcm.googleapis.com/fcm/send/ex-viewer",
	} {
		var sub webpush.Subscription
		json.Unmarshal([]byte(testSub(t, ep)), &sub)
		if err := p.add(email, sub); err != nil {
			t.Fatal(err)
		}
	}
	var old webpush.Subscription
	json.Unmarshal([]byte(testSub(t, "https://fcm.googleapis.com/fcm/send/old-phone")), &old)
	p.add("me@example.com", old)

	s.PushNotifier().Notify(model.Notification{Level: "error", Title: "train #3 failed", Subtitle: "exit 1", RunID: "r_aaaaaaaaaaaa", Job: "train"})
	deadline := time.Now().Add(2 * time.Second)
	for len(sent()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := sent()
	if len(got) != 1 || got[0].endpoint != "https://fcm.googleapis.com/fcm/send/phone" {
		t.Fatalf("sent to %v, want only the listed viewer's phone", got)
	}
	if got[0].payload.Title != "train #3 failed" || got[0].payload.URL != "/r/r_aaaaaaaaaaaa" {
		t.Errorf("payload = %+v", got[0].payload)
	}
	if p.has("me@example.com", "https://fcm.googleapis.com/fcm/send/old-phone") {
		t.Error("a gone subscription was kept")
	}
	// Web UI off: nothing is sent.
	s.mu.Lock()
	s.web = nil
	s.mu.Unlock()
	s.PushNotifier().Notify(model.Notification{Title: "x", RunID: "r_bbbbbbbbbbbb"})
	time.Sleep(100 * time.Millisecond)
	if len(sent()) != 1 {
		t.Error("sent while the web UI is off")
	}
}

func TestPublicAppFiles(t *testing.T) {
	s, remote, _ := webServer(t)
	// Served without signing in (Chrome fetches them without cookies), when the UI is built.
	r := do(t, "GET", remote.URL+"/pwa/manifest.webmanifest", "", "")
	if r.StatusCode != 200 && r.StatusCode != 404 {
		t.Fatalf("manifest: %d", r.StatusCode)
	}
	if r := do(t, "GET", remote.URL+"/pwa/../api/v1/runs", "", ""); r.StatusCode == 200 {
		t.Fatal("path tricks through the public files")
	}
	if r := do(t, "GET", remote.URL+"/pwa/other.png", "", ""); r.StatusCode == 200 {
		t.Fatal("served a file outside the public list")
	}
	s.mu.Lock()
	s.web = nil
	s.mu.Unlock()
	if r := do(t, "GET", remote.URL+"/pwa/manifest.webmanifest", "", ""); r.StatusCode == 200 {
		t.Fatal("public files served while the web UI is off")
	}
}

func TestPayloadIsClipped(t *testing.T) {
	p := payloadFor(model.Notification{Title: strings.Repeat("é", 200), Body: strings.Repeat("x", 1000), AskID: "q"})
	if len(p.Title) > 125 || len(p.Body) > 450 || !strings.HasSuffix(p.Title, "…") {
		t.Fatalf("title %d body %d", len(p.Title), len(p.Body))
	}
	b, _ := json.Marshal(p)
	if len(b) > webpush.MaxPayload {
		t.Fatalf("payload %d bytes", len(b))
	}
}

func TestUnreadablePushKeyIsNotReplaced(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	if err := os.WriteFile(pushKeyFile(), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPusher(); err == nil {
		t.Fatal("a corrupt push key was accepted")
	}
	if b, _ := os.ReadFile(pushKeyFile()); string(b) != "{garbage" {
		t.Fatal("the push key file was overwritten: every phone's subscription would break")
	}
	os.Remove(pushKeyFile())
	os.WriteFile(pushSubsFile(), []byte("[{bad"), 0o600)
	if _, err := loadPusher(); err == nil {
		t.Fatal("corrupt subscriptions accepted (they'd be overwritten on the next change)")
	}
}

func TestAddOverTheCapChangesNothing(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	p, err := loadPusher()
	if err != nil {
		t.Fatal(err)
	}
	sub := func(ep string) webpush.Subscription {
		var s webpush.Subscription
		json.Unmarshal([]byte(testSub(t, ep)), &s)
		return s
	}
	shared := "https://fcm.googleapis.com/fcm/send/shared"
	p.add("other@example.com", sub(shared))
	for i := 0; i < maxSubsPerViewer; i++ {
		if err := p.add("me@example.com", sub(fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.add("me@example.com", sub(shared)); err == nil {
		t.Fatal("over the cap accepted")
	}
	if !p.has("other@example.com", shared) {
		t.Fatal("a refused add removed someone else's device")
	}
}

func TestRejectedSubscriptionIsDropped(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s, _, _ := webServer(t)
	p := s.pushes()
	var sub webpush.Subscription
	json.Unmarshal([]byte(testSub(t, "https://fcm.googleapis.com/fcm/send/old-key")), &sub)
	p.add("me@example.com", sub)
	p.send = func(context.Context, webpush.Subscription, webpush.Message) error {
		return fmt.Errorf("%w: HTTP 403", webpush.ErrRejected)
	}
	p.deliver(p.targets(func(string) bool { return true }, ""), webpush.Message{Payload: []byte("{}")}, testURL)
	if p.has("me@example.com", sub.Endpoint) {
		t.Fatal("a subscription the push service rejects was kept")
	}
}

func TestQuestionHintSurvivesClipping(t *testing.T) {
	p := payloadFor(model.Notification{Title: "deploy asks", Subtitle: strings.Repeat("q", 300), Body: strings.Repeat("c", 300), AskID: "q_1"})
	if !strings.HasSuffix(p.Body, "Answer it on the machine running Pulse.") {
		t.Fatalf("hint cut off: …%s", p.Body[len(p.Body)-40:])
	}
}

func TestSavedTunnelURLIsNormalized(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	if err := saveRemote(RemoteConfig{Enabled: true, Addr: "127.0.0.1:0", PublicURL: "https://pulse.example.com:443"}); err != nil {
		t.Fatal(err)
	}
	s := start(t)
	s.StartRemote()
	defer s.stopRemote()
	if v := s.remoteView(); v.URL != "https://pulse.example.com" {
		t.Fatalf("URL %q: browsers send the origin without :443", v.URL)
	}
}
