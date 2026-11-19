package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/audit"
	"pulse/internal/engine"
	"pulse/internal/store"
)

const pushTok, adminTok = "push-token", "admin-token"

func start(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pulse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng, err := engine.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(eng, pushTok, adminTok)
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.ln.Close() })
	return s
}

func do(t *testing.T, method, url, token, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAuthAndGuards(t *testing.T) {
	s := start(t)
	u := s.URL()
	cases := []struct {
		name   string
		resp   *http.Response
		status int
	}{
		{"health is public", do(t, "GET", u+"/api/v1/health", "", ""), 200},
		{"no token", do(t, "GET", u+"/api/v1/runs", "", ""), 401},
		{"wrong token", do(t, "GET", u+"/api/v1/runs", "nope", ""), 401},
		{"push token can't read", do(t, "GET", u+"/api/v1/runs", pushTok, ""), 403},
		{"push token can push", do(t, "POST", u+"/api/v1/push/deploy", pushTok, `{"status":"running"}`), 200},
		{"admin can read", do(t, "GET", u+"/api/v1/runs", adminTok, ""), 200},
		{"foreign host", do(t, "GET", u+"/api/v1/runs", adminTok, "", "Host", "evil.example"), 403},
		{"foreign origin", do(t, "POST", u+"/api/v1/push/x", adminTok, `{}`, "Origin", "http://evil.example"), 403},
		{"same origin ok", do(t, "POST", u+"/api/v1/push/x", adminTok, `{}`, "Origin", u), 200},
		{"popover needs login", do(t, "GET", u+"/popover", "", ""), 401},
	}
	for _, c := range cases {
		if c.resp.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.name, c.resp.StatusCode, c.status)
		}
	}
}

func TestOneTimeLogin(t *testing.T) {
	s := start(t)
	link := s.OneTimeURL("/popover")
	resp := do(t, "GET", link, "", "")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/popover" {
		t.Fatalf("login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c.Name + "=" + c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie flags: %+v", c)
			}
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	if do(t, "GET", link, "", "").StatusCode != http.StatusUnauthorized {
		t.Error("one-time link worked twice")
	}
	if got := do(t, "GET", s.URL()+"/api/v1/runs", "", "", "Cookie", cookie).StatusCode; got != 200 {
		t.Errorf("cookie auth: %d", got)
	}
}

func TestStreamAndDismiss(t *testing.T) {
	s := start(t)
	req, _ := http.NewRequest("GET", s.URL()+"/api/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan map[string]any, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				_ = json.Unmarshal([]byte(data), &m)
				events <- m
			}
		}
	}()
	next := func() []any {
		select {
		case m := <-events:
			runs, _ := m["runs"].([]any)
			return runs
		case <-time.After(3 * time.Second):
			t.Fatal("no SSE event")
			return nil
		}
	}
	if runs := next(); len(runs) != 0 {
		t.Fatalf("initial snapshot: %v", runs)
	}
	do(t, "POST", s.URL()+"/api/v1/push/build", pushTok, `{"status":"failed","message":"boom"}`)
	runs := next()
	if len(runs) != 1 {
		t.Fatalf("after push: %v", runs)
	}
	run := runs[0].(map[string]any)
	if run["status"] != "failed" || run["seen"] != false {
		t.Fatalf("run: %v", run)
	}
	if got := do(t, "POST", s.URL()+"/api/v1/runs/"+run["id"].(string)+"/seen", adminTok, "").StatusCode; got != 200 {
		t.Fatalf("seen: %d", got)
	}
	if runs := next(); runs[0].(map[string]any)["seen"] != true {
		t.Fatalf("after seen: %v", runs)
	}
}

func TestPushBodyAndHostRules(t *testing.T) {
	s := start(t)
	u := s.URL()
	// curl -d '{…}' sends JSON with a form content type.
	req, _ := http.NewRequest("POST", u+"/api/v1/push/curljob", strings.NewReader(`{"epoch":"3/10","loss":0.4}`))
	req.Header.Set("Authorization", "Bearer "+pushTok)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("push: %v %v", resp, err)
	}
	id, _ := s.eng.LatestRun("curljob")
	if r, _ := s.eng.Run(id); len(r.Metrics) != 1 || len(r.Progress) != 1 {
		t.Fatalf("run = %+v", r)
	}
	// An SSH tunnel on another loopback port is fine; a foreign host isn't.
	if got := do(t, "GET", u+"/api/v1/runs", adminTok, "", "Host", "localhost:7777").StatusCode; got != 200 {
		t.Errorf("tunnel host: %d", got)
	}
	if got := do(t, "GET", u+"/api/v1/runs", adminTok, "", "Host", "attacker.example:7777").StatusCode; got != 403 {
		t.Errorf("foreign host: %d", got)
	}
}

func TestAssetsServeOnlyFiles(t *testing.T) {
	s := start(t)
	for _, path := range []string{"/assets/", "/assets/%2e%2e/popover.html", "/assets/../popover.html", "/assets/missing.js"} {
		if got := do(t, "GET", s.URL()+path, "", "").StatusCode; got == 200 {
			t.Errorf("%s served without login", path)
		}
	}
}

func TestRunIDsAndOpenPathsAreChecked(t *testing.T) {
	s := start(t)
	opened := ""
	s.OpenPage = func(p string) error { opened = p; return nil }
	u := s.URL()
	if got := do(t, "GET", u+"/api/v1/runs/..%2Fsecret/log", adminTok, "").StatusCode; got != 404 {
		t.Errorf("traversal id: %d", got)
	}
	if got := do(t, "POST", u+"/api/v1/ui/open?path=/j/typo", adminTok, "").StatusCode; got != 404 || opened != "" {
		t.Errorf("open unknown job: %d opened=%q", got, opened)
	}
	s.eng.Push("real", map[string]any{"status": "running"}, false)
	if got := do(t, "POST", u+"/api/v1/ui/open?path=/j/real", adminTok, "").StatusCode; got != 200 || opened != "/j/real" {
		t.Errorf("open real job: %d opened=%q", got, opened)
	}
}

func TestTokenRotationAndAudit(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	var buf strings.Builder
	audit.SetOutput(&buf)
	defer audit.SetOutput(nil)
	s := start(t)
	u := s.URL()
	var out map[string]string
	resp := do(t, "POST", u+"/api/v1/tokens/rotate", adminTok, `{"which":"push"}`)
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("rotate: %d", resp.StatusCode)
	}
	if got := do(t, "POST", u+"/api/v1/push/j", pushTok, `{}`).StatusCode; got != 401 {
		t.Errorf("old push token still works: %d", got)
	}
	newTok, _ := os.ReadFile(out["file"])
	if got := do(t, "POST", u+"/api/v1/push/j", strings.TrimSpace(string(newTok)), `{}`).StatusCode; got != 200 {
		t.Errorf("new push token: %d", got)
	}
	if !strings.Contains(buf.String(), `"event":"token.rotated"`) || strings.Contains(buf.String(), strings.TrimSpace(string(newTok))) {
		t.Fatalf("audit: %s", buf.String())
	}
}
