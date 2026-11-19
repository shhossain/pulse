package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/logs"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func limitedServer(t *testing.T, lim Limits) (*Server, *clock, string) {
	t.Helper()
	s := start(t)
	c := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	s.lim.now = c.now
	s.Limits = lim
	tok, _, err := s.eng.CreateToken("vm-job", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, c, tok
}

func TestJobTokenRequestRate(t *testing.T) {
	s, c, tok := limitedServer(t, Limits{RequestsPerSecond: 2, RequestBurst: 3, ControlPerSecond: 1, ControlBurst: 2})
	push := func(token string) *http.Response {
		return do(t, "POST", s.URL()+"/api/v1/push/vm-job", token, `{"progress":"1/9"}`)
	}
	for i := 0; i < 3; i++ {
		if r := push(tok); r.StatusCode != 200 {
			t.Fatalf("burst request %d: %d", i, r.StatusCode)
		}
	}
	r := push(tok)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("over the burst: %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if r := push(pushTok); r.StatusCode != 200 {
		t.Errorf("this machine's own push token is limited: %d", r.StatusCode)
	}
	// Control has its own budget: a data flood can't keep Stop from arriving.
	run, _ := s.eng.LatestRun("vm-job")
	if r := do(t, "GET", s.URL()+"/api/v1/runs/"+run+"/commands?wait=0s", tok, ""); r.StatusCode != 200 {
		t.Errorf("waiting for commands while over the data rate: %d", r.StatusCode)
	}
	c.advance(time.Second) // two more requests' worth
	if r := push(tok); r.StatusCode != 200 {
		t.Errorf("after refill: %d", r.StatusCode)
	}
}

func TestOverDailyQuotaIsDegradedNotCutOff(t *testing.T) {
	s, c, tok := limitedServer(t, Limits{BytesPerDay: 1000, DegradedPerSecond: 1, DegradedBytes: 400})
	s.eng.Logs = logs.New(t.TempDir())
	var tripped []string
	var mu sync.Mutex
	done := make(chan struct{}, 4)
	s.OnLimit = func(job, what string) {
		mu.Lock()
		tripped = append(tripped, job+": "+what)
		mu.Unlock()
		done <- struct{}{}
	}
	u := s.URL() + "/api/v1/runs/r_aaaaaaaaaaaa/events"
	batch := func(note string, logLines ...string) string {
		b, _ := json.Marshal(map[string]any{"job": "vm-job", "meta": map[string]any{"source": "run"},
			"events": []map[string]any{{"status": "running", "note": note}}, "log": logLines})
		return string(b)
	}
	if r := do(t, "POST", u, tok, batch(strings.Repeat("x", 1000))); r.StatusCode != 200 {
		t.Fatalf("under the quota: %d", r.StatusCode)
	}
	// Over it: a small update still goes through, its log lines don't.
	if r := do(t, "POST", u, tok, batch("small", "secret build output")); r.StatusCode != 200 {
		t.Fatalf("small update over the quota: %d", r.StatusCode)
	}
	if lines, _ := s.eng.Logs.Tail("r_aaaaaaaaaaaa", 10); len(lines) != 0 {
		t.Errorf("log lines stored over the quota: %q", lines)
	}
	if r := do(t, "POST", u, tok, batch("again")); r.StatusCode != 429 {
		t.Errorf("second request in the same second while degraded: %d", r.StatusCode)
	}
	c.advance(time.Second)
	if r := do(t, "POST", u, tok, batch(strings.Repeat("y", 500))); r.StatusCode != 429 {
		t.Errorf("large request while degraded: %d", r.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnLimit not called")
	}
	mu.Lock()
	if len(tripped) != 1 || tripped[0] != "vm-job: daily data quota" {
		t.Errorf("tripped = %q (want one notice)", tripped)
	}
	mu.Unlock()
	c.advance(25 * time.Hour)
	if r := do(t, "POST", u, tok, batch(strings.Repeat("z", 500))); r.StatusCode != 200 {
		t.Errorf("next day: %d", r.StatusCode)
	}
}

func TestQueryStringCountsTowardTheQuota(t *testing.T) {
	s, _, tok := limitedServer(t, Limits{BytesPerDay: 1000, DegradedBytes: 100, DegradedPerSecond: 100})
	u := s.URL() + "/api/v1/push/vm-job?note=" + strings.Repeat("q", 1200)
	if r := do(t, "POST", u, tok, ""); r.StatusCode != 200 {
		t.Fatalf("first: %d", r.StatusCode)
	}
	if r := do(t, "POST", u, tok, ""); r.StatusCode != 429 {
		t.Fatalf("data in the URL wasn't counted: %d", r.StatusCode)
	}
}

func TestJobTokenNewRunsPerHour(t *testing.T) {
	s, c, tok := limitedServer(t, Limits{RunsPerHour: 2})
	newRun := func() int {
		return do(t, "POST", s.URL()+"/api/v1/push/vm-job?new=1", tok, "").StatusCode
	}
	if newRun() != 200 || newRun() != 200 {
		t.Fatal("first runs refused")
	}
	if got := newRun(); got != 429 {
		t.Fatalf("third new run in the hour: %d", got)
	}
	if r := do(t, "POST", s.URL()+"/api/v1/push/vm-job", tok, `{"progress":"2/9"}`); r.StatusCode != 200 {
		t.Errorf("updating the current run is limited too: %d", r.StatusCode)
	}
	ev := `{"job":"vm-job","meta":{"source":"run"},"events":[{"status":"running"}]}`
	if r := do(t, "POST", s.URL()+"/api/v1/runs/r_cccccccccccc/events", tok, ev); r.StatusCode != 429 {
		t.Errorf("new run through events: %d", r.StatusCode)
	}
	c.advance(61 * time.Minute)
	if got := newRun(); got != 200 {
		t.Fatalf("an hour later: %d", got)
	}
}

func TestJobTokenAsksPerHour(t *testing.T) {
	s, c, tok := limitedServer(t, Limits{AsksPerHour: 2})
	resp, _ := s.eng.Push("vm-job", map[string]any{"status": "running"}, false)
	u := s.URL() + "/api/v1/runs/" + resp.Run + "/asks"
	ask := `{"question":"Deploy?","options":["yes","no"],"timeout_s":60}`
	for i := 0; i < 2; i++ {
		if r := do(t, "POST", u, tok, ask); r.StatusCode != 200 {
			t.Fatalf("ask %d: %d", i, r.StatusCode)
		}
	}
	c.advance(30 * time.Minute)
	r := do(t, "POST", u, tok, ask)
	if r.StatusCode != 429 {
		t.Fatalf("third ask in the hour: %d", r.StatusCode)
	}
	// The first two free up 30 minutes from now, give or take a bucket.
	if ra, _ := strconv.Atoi(r.Header.Get("Retry-After")); ra < 25*60 || ra > 35*60 {
		t.Errorf("Retry-After %q: should say when a slot frees", r.Header.Get("Retry-After"))
	}
	c.advance(31 * time.Minute)
	if r := do(t, "POST", u, tok, ask); r.StatusCode != 200 {
		t.Fatalf("an hour later: %d", r.StatusCode)
	}
}

func TestTunnelRefusesTheMachinePushToken(t *testing.T) {
	s := start(t)
	remote := httptest.NewServer(s.remoteHandler("https://pulse.example.com"))
	defer remote.Close()
	tok, _, _ := s.eng.CreateToken("ci", "")
	if r := do(t, "POST", remote.URL+"/api/v1/push/ci", pushTok, `{}`); r.StatusCode != 403 {
		t.Errorf("machine push token through the tunnel: %d", r.StatusCode)
	}
	if r := do(t, "POST", remote.URL+"/api/v1/push/ci", tok, `{}`); r.StatusCode != 200 {
		t.Errorf("job token through the tunnel: %d", r.StatusCode)
	}
	r := do(t, "PUT", remote.URL+"/api/v1/jobs/config", tok, `{}`)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 404 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("unknown route: %d %s %s (want a JSON 404 so clients know it's Pulse)", r.StatusCode, r.Header.Get("Content-Type"), b)
	}
}

// Ten busy runs of one job, each sending what the real sender sends at most
// (4 requests a second, ~0.5 KB), plus each waiting for commands: none of
// it is ever refused under the defaults.
func TestDefaultLimitsLeaveHonestJobsAlone(t *testing.T) {
	l := newLimiter()
	c := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	l.now = c.now
	for tick := 0; tick < 4*600; tick++ { // ten minutes at 250 ms
		for run := 0; run < 10; run++ {
			if _, e := l.admit("busy", DefaultLimits, false, 512); e != nil {
				t.Fatalf("tick %d run %d: %v", tick, run, e)
			}
			if tick%120 == 0 { // a 30 s long-poll per run
				if _, e := l.admit("busy", DefaultLimits, true, 0); e != nil {
					t.Fatalf("control, tick %d: %v", tick, e)
				}
			}
		}
		c.advance(250 * time.Millisecond)
	}
	// And a day of one busy run is far below the data quota.
	if perDay := int64(4 * 512 * 86400); perDay*5 > DefaultLimits.BytesPerDay {
		t.Fatalf("one busy run sends %d MB a day; the quota (%d MB) leaves little room", perDay>>20, DefaultLimits.BytesPerDay>>20)
	}
}
