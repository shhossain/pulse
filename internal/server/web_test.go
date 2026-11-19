package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/model"
)

const (
	testTeam = "myteam.cloudflareaccess.com"
	testAUD  = "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2"
	testURL  = "https://pulse.example.com"
)

// accessFixture is a fake Cloudflare Access: a signing key and its certs endpoint.
type accessFixture struct {
	key   *rsa.PrivateKey
	certs string
}

func newAccess(t *testing.T) *accessFixture {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kid": "k1", "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return &accessFixture{key: k, certs: srv.URL}
}

func (a *accessFixture) token(t *testing.T, email, commonName string) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	in := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(map[string]any{
		"aud": []string{testAUD}, "iss": "https://" + testTeam, "sub": "s", "email": email,
		"common_name": commonName, "iat": now, "exp": now + 3600})
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func webServer(t *testing.T) (*Server, *httptest.Server, *accessFixture) {
	t.Helper()
	s := start(t)
	a := newAccess(t)
	cfg, err := validWeb(WebConfig{Team: testTeam, AUD: testAUD, Emails: []string{"Me@Example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	wa := newWebAccess(cfg)
	wa.v.CertsURL = a.certs
	s.web = wa
	remote := httptest.NewServer(s.remoteHandler(testURL))
	t.Cleanup(remote.Close)
	return s, remote, a
}

func TestWebViewerReadsButCannotChange(t *testing.T) {
	s, remote, a := webServer(t)
	run, _ := s.eng.Push("train", map[string]any{"status": "running", "progress": "3/10"}, false)
	me := a.token(t, "me@example.com", "")
	jwt := func(tok string) []string { return []string{"Cf-Access-Jwt-Assertion", tok} }
	get := func(path string, hdr ...string) *http.Response { return do(t, "GET", remote.URL+path, "", "", hdr...) }

	r := get("/api/v1/runs", jwt(me)...)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), run.Run) {
		t.Fatalf("viewer list runs: %d %s", r.StatusCode, b)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	r = get("/api/v1/session", jwt(me)...)
	b, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"read_only":true`) || !strings.Contains(string(b), "me@example.com") {
		t.Errorf("session: %s", b)
	}
	// Browser cookies for the domain don't matter on the viewer path.
	if r := get("/api/v1/runs/"+run.Run, append(jwt(me), "Cookie", "CF_Authorization=x; _ga=1")...); r.StatusCode != 200 {
		t.Errorf("viewer with other cookies: %d", r.StatusCode)
	}
	cases := []struct {
		name   string
		resp   *http.Response
		status int
	}{
		{"stop is refused", do(t, "POST", remote.URL+"/api/v1/runs/"+run.Run+"/stop", "", "", jwt(me)...), 403},
		{"answering is refused", do(t, "POST", remote.URL+"/api/v1/asks/q_1/answer", "", `{"answer":"yes"}`, jwt(me)...), 403},
		{"tokens aren't readable", get("/api/v1/tokens", jwt(me)...), 404},
		{"alert rules aren't readable", get("/api/v1/alerts?job=train", jwt(me)...), 404},
		{"remote settings aren't readable", get("/api/v1/remote", jwt(me)...), 404},
		{"watches (shell commands) aren't readable", get("/api/v1/watches", jwt(me)...), 404},
		{"another site through the viewer's browser", get("/api/v1/runs", append(jwt(me), "Origin", "https://evil.example")...), 403},
		{"same-site origin is fine", get("/api/v1/runs", append(jwt(me), "Origin", testURL)...), 200},
		{"someone not on the list", get("/api/v1/runs", jwt(a.token(t, "other@example.com", ""))...), 404},
		{"a service token isn't a viewer", get("/api/v1/runs", jwt(a.token(t, "", "abc.access"))...), 404},
		{"a forged token", get("/api/v1/runs", jwt(me[:len(me)-8]+"AAAAAAAA")...), 404},
		{"no token", get("/api/v1/runs"), 404},
	}
	for _, c := range cases {
		if c.resp.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.name, c.resp.StatusCode, c.status)
		}
	}
	// Job machines still report alongside, with a service-token JWT and a job token.
	tok, _, _ := s.eng.CreateToken("vm-job", "")
	if r := do(t, "POST", remote.URL+"/api/v1/push/vm-job", tok, `{"progress":"1/2"}`, jwt(a.token(t, "", "abc.access"))...); r.StatusCode != 200 {
		t.Errorf("job push next to the web UI: %d", r.StatusCode)
	}
}

func TestWebOffIgnoresAccessTokens(t *testing.T) {
	s, remote, a := webServer(t)
	s.mu.Lock()
	s.web = nil
	s.mu.Unlock()
	if r := do(t, "GET", remote.URL+"/api/v1/runs", "", "", "Cf-Access-Jwt-Assertion", a.token(t, "me@example.com", "")); r.StatusCode != 404 {
		t.Fatalf("web UI off, viewer token: %d", r.StatusCode)
	}
}

func TestValidWeb(t *testing.T) {
	ok := WebConfig{Team: "MyTeam.cloudflareaccess.com", AUD: strings.ToUpper(testAUD), Emails: []string{" A@b.co ", "a@b.co"}}
	c, err := validWeb(ok)
	if err != nil || c.Team != "myteam.cloudflareaccess.com" || c.AUD != testAUD || len(c.Emails) != 1 || c.Emails[0] != "a@b.co" {
		t.Fatalf("got %+v %v", c, err)
	}
	for name, bad := range map[string]WebConfig{
		"other host":     {Team: "evil.example.com", AUD: testAUD, Emails: []string{"a@b.co"}},
		"team lookalike": {Team: "x.cloudflareaccess.com.evil.com", AUD: testAUD, Emails: []string{"a@b.co"}},
		"short aud":      {Team: testTeam, AUD: "abc", Emails: []string{"a@b.co"}},
		"no emails":      {Team: testTeam, AUD: testAUD},
		"not an email":   {Team: testTeam, AUD: testAUD, Emails: []string{"me"}},
		"display name":   {Team: testTeam, AUD: testAUD, Emails: []string{"Me <a@b.co>"}},
	} {
		if _, err := validWeb(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Someone's own Access login on a job machine: Cloudflare adds a user token,
// but the request is a job's (it carries a job token) and must work as one.
func TestJobTokenWinsOverAViewerLogin(t *testing.T) {
	s, remote, a := webServer(t)
	tok, _, _ := s.eng.CreateToken("vm-job", "")
	me := a.token(t, "me@example.com", "")
	if r := do(t, "POST", remote.URL+"/api/v1/push/vm-job", tok, `{"progress":"1/2"}`, "Cf-Access-Jwt-Assertion", me); r.StatusCode != 200 {
		t.Fatalf("job push with the owner's Access login: %d", r.StatusCode)
	}
	run, _ := s.eng.LatestRun("vm-job")
	if r := do(t, "GET", remote.URL+"/api/v1/runs/"+run+"/commands?wait=0s", tok, "", "Cf-Access-Jwt-Assertion", me); r.StatusCode != 200 {
		t.Fatalf("waiting for commands with the owner's Access login: %d", r.StatusCode)
	}
}

// Turning the web UI off ends streams already open, not just new requests.
func TestViewerStreamEndsWhenAccessEnds(t *testing.T) {
	old := viewerRecheck
	viewerRecheck = 20 * time.Millisecond
	t.Cleanup(func() { viewerRecheck = old })
	s, remote, a := webServer(t)
	req, _ := http.NewRequest("GET", remote.URL+"/api/v1/stream", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", a.token(t, "me@example.com", ""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("open stream: %v %v", resp, err)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(ended) }()
	select {
	case <-ended:
		t.Fatal("stream ended on its own")
	case <-time.After(100 * time.Millisecond):
	}
	s.mu.Lock()
	s.web = nil // pulse remote web off
	s.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("a viewer kept streaming after the web UI was turned off")
	}
}

func TestOperatorsActViewersCannot(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s, remote, a := webServer(t)
	cfg, _ := validWeb(WebConfig{Team: testTeam, AUD: testAUD, Emails: []string{"viewer@example.com"}, Operators: []string{"Op@Example.com"}})
	wa := newWebAccess(cfg)
	wa.v.CertsURL = s.web.v.CertsURL
	s.mu.Lock()
	s.web = wa
	s.mu.Unlock()
	if !wa.emails["op@example.com"] || len(cfg.Operators) != 1 {
		t.Fatalf("operators must also be able to look: %+v", cfg)
	}
	op := a.token(t, "op@example.com", "")
	viewer := a.token(t, "viewer@example.com", "")
	post := func(path, tok, body string, origin bool) int {
		hdr := []string{"Cf-Access-Jwt-Assertion", tok}
		if origin {
			hdr = append(hdr, "Origin", testURL)
		}
		return do(t, "POST", remote.URL+path, "", body, hdr...).StatusCode
	}

	run, _ := s.eng.Push("train", map[string]any{"status": "running"}, false)
	ask, err := s.eng.Ask(run.Run, "Deploy?", []string{"yes", "no"}, "no", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := post("/api/v1/asks/"+ask.ID+"/answer", viewer, `{"answer":"yes"}`, true); got != 403 {
		t.Errorf("viewer answered: %d", got)
	}
	if got := post("/api/v1/asks/"+ask.ID+"/answer", op, `{"answer":"yes"}`, false); got != 403 {
		t.Errorf("operator without Origin (not our page): %d", got)
	}
	if got := post("/api/v1/asks/"+ask.ID+"/answer", op, `{"answer":"yes","by":"someone-else"}`, true); got != 200 {
		t.Fatalf("operator answer: %d", got)
	}
	if a, _ := s.eng.GetAsk(ask.ID); a.AnsweredBy != "web:op@example.com" || a.Answer != "yes" {
		t.Errorf("answered %q by %q: must be the verified email, not what the browser says", a.Answer, a.AnsweredBy)
	}
	if got := post("/api/v1/runs/"+run.Run+"/stop", viewer, "", true); got != 403 {
		t.Errorf("viewer stopped a run: %d", got)
	}
	if got := post("/api/v1/runs/"+run.Run+"/stop", op, "", true); got != 200 {
		t.Errorf("operator stop: %d", got)
	}
	for _, path := range []string{"/api/v1/runs/" + run.Run + "/actions/deploy", "/api/v1/mute", "/api/v1/alerts", "/api/v1/tokens"} {
		if got := post(path, op, "{}", true); got != 404 {
			t.Errorf("operator reached %s: %d (shell actions and settings stay off the web)", path, got)
		}
	}
	r := do(t, "GET", remote.URL+"/api/v1/session", "", "", "Cf-Access-Jwt-Assertion", op)
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"can_act":true`) {
		t.Errorf("operator session: %s", b)
	}
}

func TestAnswerButtonsOnlyForOperators(t *testing.T) {
	n := model.Notification{Title: "deploy asks", Subtitle: "Deploy?", AskID: "q_1", AskOptions: []string{"yes", "no"}, RunID: "r_aaaaaaaaaaaa"}
	if p := payloadFor(n, true, true); p.Ask == nil || p.Ask.ID != "q_1" || len(p.Ask.Options) != 2 {
		t.Errorf("operator payload: %+v", p)
	}
	if p := payloadFor(n, false, true); p.Ask != nil {
		t.Error("a viewer got answer buttons")
	}
	if p := payloadFor(n, true, false); p.Ask != nil || !strings.Contains(p.Body, "machine running Pulse") {
		t.Errorf("an answer runs a shell action: no buttons, answer on the hub: %+v", p)
	}
	n.AskOptions = []string{"a", "b", "c"}
	if p := payloadFor(n, true, true); p.Ask != nil || !strings.HasSuffix(p.Body, "Open Pulse to answer.") {
		t.Errorf("three options (phones show two): no buttons, open the app: %+v", p)
	}
}

// An alert rule can tie an answer to a shell action ("Yes=action:deploy").
// Operators can answer from the web, but never that way.
func TestWebAnswerNeverRunsAShellAction(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s, remote, a := webServer(t)
	cfg, _ := validWeb(WebConfig{Team: testTeam, AUD: testAUD, Operators: []string{"op@example.com"}})
	wa := newWebAccess(cfg)
	wa.v.CertsURL = s.web.v.CertsURL
	s.mu.Lock()
	s.web = wa
	s.mu.Unlock()
	s.eng.Push("train", map[string]any{"status": "running"}, false)
	marker := filepath.Join(t.TempDir(), "deployed")
	if r := do(t, "POST", s.URL()+"/api/v1/actions?job=train", adminTok, `{"name":"deploy","shell":"touch `+marker+`"}`); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("add action: %d %s", r.StatusCode, b)
	}
	r := do(t, "POST", s.URL()+"/api/v1/alerts?job=train", adminTok,
		`{"expr":"loss > 1","ask":"Deploy?","answers":["Yes=action:deploy","No"]}`)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("add rule: %d %s", r.StatusCode, b)
	}
	s.eng.Push("train", map[string]any{"loss": 2}, false)
	var ask model.Ask
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if open := s.eng.OpenAsks(); len(open) > 0 {
			ask = open[0]
			break
		}
	}
	if ask.ID == "" || ask.Then["Yes"] != "action:deploy" {
		t.Fatalf("rule question not asked: %+v", ask)
	}
	op := a.token(t, "op@example.com", "")
	answer := func(choice string) int {
		return do(t, "POST", remote.URL+"/api/v1/asks/"+ask.ID+"/answer", "", `{"answer":"`+choice+`"}`,
			"Cf-Access-Jwt-Assertion", op, "Origin", testURL).StatusCode
	}
	if got := answer("Yes"); got != 403 {
		t.Fatalf("an answer that runs a shell action, from the web: %d", got)
	}
	if got := answer("No"); got != 200 {
		t.Fatalf("a plain answer from the web: %d", got)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the shell action ran")
	}
}
