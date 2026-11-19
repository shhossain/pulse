package server

import (
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
	if err := s.listenRemote("0.0.0.0:0"); err == nil || !strings.Contains(err.Error(), "never listens publicly") {
		t.Fatalf("0.0.0.0 accepted: %v", err)
	}
	if err := s.listenRemote("8.8.8.8:7777"); err == nil {
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
