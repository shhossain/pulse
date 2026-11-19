package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pulse/internal/model"
)

func TestJobTokensAreScoped(t *testing.T) {
	s := start(t)
	u := s.URL()
	tok, _, err := s.eng.CreateToken("nightly-sync", "vm")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.eng.Push("other", map[string]any{"status": "running"}, false)
	cases := []struct {
		name   string
		resp   *http.Response
		status int
	}{
		{"push own job", do(t, "POST", u+"/api/v1/push/nightly-sync", tok, `{"progress":"7/147"}`), 200},
		{"unversioned path is gone", do(t, "POST", u+"/api/push/nightly-sync", tok, `{"progress":"8/147"}`), 404},
		{"push other job", do(t, "POST", u+"/api/v1/push/other", tok, `{"x":1}`), 403},
		{"read runs", do(t, "GET", u+"/api/v1/runs", tok, ""), 403},
		{"events for other job", do(t, "POST", u+"/api/v1/runs/r_aaaaaaaaaaaa/events", tok, `{"job":"other","events":[]}`), 403},
		{"events into other job's run", do(t, "POST", u+"/api/v1/runs/"+other.Run+"/events", tok, `{"job":"nightly-sync","events":[]}`), 403},
		{"events with job omitted", do(t, "POST", u+"/api/v1/runs/r_bbbbbbbbbbbb/events", tok, `{"events":[{"x":1}]}`), 200},
		{"commands of other job's run", do(t, "GET", u+"/api/v1/runs/"+other.Run+"/commands", tok, ""), 403},
		{"revoked-looking token", do(t, "POST", u+"/api/v1/push/nightly-sync", "pls_nope", `{}`), 401},
	}
	for _, c := range cases {
		if c.resp.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.name, c.resp.StatusCode, c.status)
		}
	}
	if job := s.eng.RunJob("r_bbbbbbbbbbbb"); job != "nightly-sync" {
		t.Errorf("run created with omitted job belongs to %q", job)
	}
}

func TestRemoteHandlerOnlyServesPush(t *testing.T) {
	s := start(t)
	remote := httptest.NewServer(s.RemoteHandler())
	defer remote.Close()
	tok, _, _ := s.eng.CreateToken("ci", "")
	cases := []struct {
		name   string
		resp   *http.Response
		status int
	}{
		{"health", do(t, "GET", remote.URL+"/api/v1/health", "", ""), 200},
		{"push with job token", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{"status":"running"}`), 200},
		{"push with global push token", do(t, "POST", remote.URL+"/api/v1/push/ci", pushTok, `{"progress":"1/2"}`), 200},
		{"admin token refused remotely", do(t, "POST", remote.URL+"/api/v1/push/ci", adminTok, `{}`), 403},
		{"admin routes don't exist remotely", do(t, "GET", remote.URL+"/api/v1/runs", pushTok, ""), 404},
		{"no UI remotely", do(t, "GET", remote.URL+"/popover", "", ""), 404},
		{"browsers refused", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Origin", "http://evil.example"), 403},
		{"browser cookies refused", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Cookie", "session=abc"), 403},
		{"Cloudflare Access cookie isn't a browser", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Cookie", "CF_Authorization=eyJ"), 200},
		{"Access cookie doesn't hide others", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Cookie", "CF_Authorization=eyJ; session=abc"), 403},
		{"unparseable cookie is still a cookie", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Cookie", `session="abc`), 403},
		{"nameless cookie is still a cookie", do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`, "Cookie", "=x"), 403},
	}
	for _, c := range cases {
		if c.resp.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.name, c.resp.StatusCode, c.status)
		}
	}
}

func TestRemoteOnlyPrivateAddresses(t *testing.T) {
	for ip, want := range map[string]bool{
		"100.101.2.3": true, "192.168.1.5": true, "10.0.0.8": true, "172.20.1.1": true, "fd7a:115c:a1e0::1": true,
		"8.8.8.8": false, "0.0.0.0": false, "127.0.0.1": false, "172.32.0.1": false, "::": false,
	} {
		if got := PrivateIP(net.ParseIP(ip)); got != want {
			t.Errorf("PrivateIP(%s) = %v", ip, got)
		}
	}
	s := start(t)
	if err := s.listenRemote("0.0.0.0:0", ""); err == nil || !strings.Contains(err.Error(), "never listens publicly") {
		t.Fatalf("0.0.0.0 accepted: %v", err)
	}
	if err := s.listenRemote("8.8.8.8:7777", ""); err == nil {
		t.Fatal("public address accepted")
	}
}

func TestJobTokenNamesAreNeverRenamed(t *testing.T) {
	s := start(t)
	u := s.URL()
	// Another "train" is already running from a different machine.
	s.eng.ApplyBatch("r_aaaaaaaaaaaa", model.Batch{Job: "train", Meta: model.RunMeta{Source: "run", Host: "laptop", Heartbeat: true, Inferred: true}})
	tok, _, _ := s.eng.CreateToken("train", "")
	body := `{"job":"train","meta":{"source":"run","host":"gpu-box","heartbeat":true,"inferred":true},"events":[]}`
	first := do(t, "POST", u+"/api/v1/runs/r_bbbbbbbbbbbb/events", tok, body)
	second := do(t, "POST", u+"/api/v1/runs/r_bbbbbbbbbbbb/events", tok, body)
	if first.StatusCode != 200 || second.StatusCode != 200 || s.eng.RunJob("r_bbbbbbbbbbbb") != "train" {
		t.Fatalf("first=%d second=%d job=%s", first.StatusCode, second.StatusCode, s.eng.RunJob("r_bbbbbbbbbbbb"))
	}
}

func TestTunnelURLValidation(t *testing.T) {
	for raw, want := range map[string]string{
		"":                               "",
		"https://pulse.example.com":      "https://pulse.example.com",
		"https://pulse.example.com/":     "https://pulse.example.com",
		"https://pulse.example.com:8443": "https://pulse.example.com:8443",
		"https://Pulse.Example.com":      "https://pulse.example.com",
		"https://pulse.example.com:443":  "https://pulse.example.com", // as browsers write the origin
	} {
		if got, err := tunnelURL(raw); err != nil || got != want {
			t.Errorf("tunnelURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"http://pulse.example.com", // plain http: tokens would cross the internet in clear text
		"https://user:pw@pulse.example.com",
		"https://pulse.example.com/pulse",
		"https://pulse.example.com/?x=1",
		"https://pulse.example.com/#x",
		"pulse.example.com",
		"https://",
		"https://pulse.example.com:", // would be printed as every job's PULSE_URL
		"https://pulse.example.com:0",
		"https://pulse.example.com:99999",
		"https://-.com",
		"https://pulse_hub.example.com",
		"https://localhost",
		"https://10.0.0.1",
		"https://pulse.example.com?",
	} {
		if _, err := tunnelURL(raw); err == nil {
			t.Errorf("tunnelURL(%q) accepted", raw)
		}
	}
}

func TestTunnelModeListensOnLoopbackOnly(t *testing.T) {
	s := start(t)
	for _, addr := range []string{"192.168.1.5:0", "0.0.0.0:0", "8.8.8.8:7778"} {
		if err := s.listenRemote(addr, "https://pulse.example.com"); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("tunnel mode on %s: %v", addr, err)
		}
	}
}

func TestTunnelModeAcceptsLocalhost(t *testing.T) {
	s := start(t)
	defer s.stopRemote()
	if err := s.listenRemote("localhost:0", "https://pulse.example.com"); err != nil {
		t.Fatalf("localhost refused in tunnel mode: %v", err)
	}
}

func TestTunnelModeEndToEnd(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	s := start(t)
	defer s.stopRemote()
	body := `{"enable":true,"addr":"127.0.0.1:0","public_url":"https://pulse.example.com/"}`
	resp := do(t, "POST", s.URL()+"/api/v1/remote", adminTok, body)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enable: %d %s", resp.StatusCode, b)
	}
	var v remoteView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || !v.Tunnel || v.URL != "https://pulse.example.com" || !strings.HasPrefix(v.Listen, "127.0.0.1:") {
		t.Fatalf("view = %+v", v)
	}
	if c := loadRemote(); c.PublicURL != "https://pulse.example.com" || !c.Enabled {
		t.Fatalf("saved config = %+v", c)
	}
	// What the tunnel daemon forwards: a request for the public host, to the loopback listener.
	tok, _, _ := s.eng.CreateToken("vm-train", "")
	base := "http://" + v.Listen
	if r := do(t, "POST", base+"/api/v1/push/vm-train", tok, `{"progress":"1/4"}`, "Host", "pulse.example.com"); r.StatusCode != 200 {
		t.Errorf("push through tunnel: %d", r.StatusCode)
	}
	if r := do(t, "GET", base+"/api/v1/runs", adminTok, "", "Host", "pulse.example.com"); r.StatusCode != 403 {
		t.Errorf("admin token through tunnel: %d, want 403", r.StatusCode)
	}
	if r := do(t, "GET", base+"/popover", "", "", "Host", "pulse.example.com"); r.StatusCode != 404 {
		t.Errorf("UI through tunnel: %d, want 404", r.StatusCode)
	}
}
