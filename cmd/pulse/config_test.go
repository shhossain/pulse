package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pulse/internal/client"
	"pulse/internal/config"
	"pulse/internal/model"
)

// fakeRuns serves /api/runs?job=…[&seq=…] from a fixed set of runs.
func fakeRuns(t *testing.T, runs []model.Run) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/runs/") {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		job, seq := r.URL.Query().Get("job"), r.URL.Query().Get("seq")
		out := []model.Run{}
		for _, x := range runs {
			if x.Job == job && (seq == "" || seq == itoa(x.Seq)) {
				out = append(out, x)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"runs": out, "total": len(out)})
	}))
	t.Cleanup(srv.Close)
	return &client.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestResolveRunPrefersRealJobNames(t *testing.T) {
	c := fakeRuns(t, []model.Run{
		{ID: "r_000000000003", Job: "train.py", Seq: 3, Status: model.Done},
		{ID: "r_000000000002", Job: "train.py", Seq: 2, Status: model.Done},
		{ID: "r_00000000000a", Job: "train.py#2", Seq: 1, Status: model.Running},
		{ID: "r_00000000000b", Job: "r_squared", Seq: 1, Status: model.Running},
	})
	ctx := t.Context()
	cases := []struct {
		target  string
		running bool
		wantID  string
		wantErr string
	}{
		{"train.py#2", true, "r_00000000000a", ""}, // a job literally named train.py#2
		{"train.py#3", false, "r_000000000003", ""},
		{"train.py#3x", false, "", "no job named"},
		{"r_squared", true, "r_00000000000b", ""}, // a job name that looks like an id prefix
		{"train.py#2", false, "r_00000000000a", ""},
		{"train.py", true, "", "no running run"},
	}
	for _, tc := range cases {
		r, err := resolveRun(ctx, c, tc.target, tc.running)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.target, err, tc.wantErr)
			}
			continue
		}
		if err != nil || r.ID != tc.wantID {
			t.Errorf("%s: got %q %v, want %q", tc.target, r.ID, err, tc.wantID)
		}
	}
}

func TestShellCommandKeepsArguments(t *testing.T) {
	if got := shellCommand([]string{"ls | wc -l"}); got != "ls | wc -l" {
		t.Errorf("single word: %q", got)
	}
	got := shellCommand([]string{"echo", "hello world; rm", "it's"})
	if got != `echo 'hello world; rm' 'it'\''s'` {
		t.Errorf("several words: %q", got)
	}
}

func TestSendConfigTrustResultIsFresh(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // first apply: one action waits for trust
			json.NewEncoder(w).Encode(map[string]any{"job": "j", "untrusted": []map[string]string{{"name": "gpu", "shell": "nvidia-smi"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"job": "j", "actions": 1}) // untrusted omitted
	}))
	defer srv.Close()
	askYes = func(string) bool { return true }
	isTerminal = func() bool { return true }
	res, err := sendConfig(&client.Client{BaseURL: srv.URL, HTTP: srv.Client()}, config.Job{Name: "j"}, "/x/pulse.yaml", true, false)
	if err != nil || calls != 2 || len(res.Untrusted) != 0 || res.Actions != 1 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, calls)
	}
}

func TestAskExitsThreeWhenTheRunEnded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/health"):
			w.Write([]byte(`{"ok":true}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/push/"):
			w.Write([]byte(`{"job":"j","run":"r_000000000001"}`))
		case strings.HasSuffix(r.URL.Path, "/asks"):
			w.Write([]byte(`{"id":"q_1"}`))
		default:
			w.Write([]byte(`{"id":"q_1","answer":"timeout","answered_at":"2026-01-01T00:00:00Z","answered_by":"run ended"}`))
		}
	}))
	defer srv.Close()
	t.Setenv("PULSE_URL", srv.URL)
	t.Setenv("PULSE_TOKEN", "x")
	if code := cmdAsk([]string{"j", "Ship?", "yes", "no"}); code != 3 {
		t.Fatalf("exit = %d", code)
	}
}

func TestJSONFlagOnlyForReadCommands(t *testing.T) {
	cmd, args, j := splitCommand([]string{"run", "-n", "x", "./tool", "--json"})
	if cmd != "run" || j || args[len(args)-1] != "--json" {
		t.Fatalf("run lost the user's flag: %v %v", args, j)
	}
	if cmd, args, j := splitCommand([]string{"ls", "--json", "--all"}); cmd != "ls" || !j || len(args) != 1 {
		t.Fatalf("ls: %v %v", args, j)
	}
	if cmd, _, j := splitCommand([]string{"--json", "show", "train"}); cmd != "show" || !j {
		t.Fatalf("global --json: %v", j)
	}
}
