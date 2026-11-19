package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecode(t *testing.T) {
	got, err := Decode([]byte(`{"stats":{"done":7,"total":147},"gb":8.03,"current":"clip_007.mp4"}`),
		map[string]string{"progress": "stats.done/stats.total"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"progress": []any{7.0, 147.0}, "gb": 8.03, "current": "clip_007.mp4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if _, err := Decode([]byte(`{"stats":{"done":7,`), nil); err == nil || !strings.Contains(err.Error(), "half-written") {
		t.Fatalf("partial JSON: %v", err)
	}
	got, err = Decode([]byte("epoch 3/10 loss=0.42\n"), nil)
	if err != nil || got["loss"] != "0.42" {
		t.Fatalf("text: %v %v", got, err)
	}
	if _, err := Decode([]byte(`{"a":1}`), map[string]string{"progress": "x/y"}); err == nil {
		t.Fatal("missing mapped field should be an error")
	}
}

func TestValidate(t *testing.T) {
	s := Spec{Job: "ci", Kind: KindURL, Source: "https://github.com/acme/app/actions/runs/123"}
	if err := s.Validate(); err != nil || s.Kind != KindGitHub || s.EveryS != 15 {
		t.Fatalf("github: %+v %v", s, err)
	}
	if GitHubJobName(s.Source) != "app CI #123" {
		t.Fatal(GitHubJobName(s.Source))
	}
	for _, bad := range []Spec{
		{Kind: KindURL, Source: "file:///etc/passwd"},
		{Kind: KindCmd, Source: " "},
		{Kind: KindFile, Source: "/definitely/not/here"},
		{Kind: KindCmd, Source: "true", EveryS: 1},
		{Kind: ""},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if githubStatus("completed", "cancelled") != "stopped" || githubStatus("in_progress", "") != "running" {
		t.Fatal("status mapping")
	}
}

type memStore struct {
	mu    sync.Mutex
	specs map[string]Spec
}

func (m *memStore) SaveWatch(job string, s *Spec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s == nil {
		delete(m.specs, job)
	} else {
		m.specs[job] = *s
	}
	return nil
}

func (m *memStore) Watches() []Spec { return nil }

func TestManagerPushesOnlyChangesAndSkipsBadReads(t *testing.T) {
	file := filepath.Join(t.TempDir(), "progress.json")
	os.WriteFile(file, []byte(`{"progress":"1/10"}`), 0o644)
	var mu sync.Mutex
	var pushes []map[string]any
	m := NewManager(func(job string, raw map[string]any) error {
		mu.Lock()
		pushes = append(pushes, raw)
		mu.Unlock()
		return nil
	}, &memStore{specs: map[string]Spec{}})
	w := &watcher{spec: Spec{Job: "j", Kind: KindFile, Source: file}}
	m.running["j"] = w
	ctx := context.Background()

	m.poll(ctx, w)                                        // pushed
	m.poll(ctx, w)                                        // unchanged: skipped
	os.WriteFile(file, []byte(`{"progress":"2/1`), 0o644) // half-written
	m.poll(ctx, w)                                        // error: skipped
	os.WriteFile(file, []byte(`{"progress":"2/10"}`), 0o644)
	m.poll(ctx, w) // pushed

	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 || pushes[1]["progress"] != "2/10" {
		t.Fatalf("pushes = %v", pushes)
	}
	if st := m.List()[0]; st.Failures != 0 || st.LastPush == nil {
		t.Fatalf("status = %+v", st)
	}
}

func TestCmdSourceUsesStdoutOnly(t *testing.T) {
	body, err := fetchCmd(context.Background(), Spec{Job: "j", Source: `echo noise >&2; echo '{"progress":"3/4"}'`, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := fetchCmd(ctx, Spec{Job: "j", Source: "echo boom >&2; exit 3", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failing command: %v", err)
	}
	if got, _ := Decode(body, nil); got["progress"] != "3/4" {
		t.Fatalf("got %v", got)
	}
}

func TestDecodeNaNAndRedraws(t *testing.T) {
	got, err := Decode([]byte(`{"loss": NaN, "best": -Infinity, "step": 3}`), nil)
	if err != nil || got["loss"] != "NaN" || got["best"] != "-Infinity" {
		t.Fatalf("NaN JSON: %v %v", got, err)
	}
	got, _ = Decode([]byte("train:  10%|█| 10/100 [00:01<00:09]\r train:  50%|█████| 50/100 [00:05<00:05]\r train:  90%|█████████| 90/100 [00:09<00:01]"), nil)
	if p, _ := got["progress.train"].([]any); len(p) != 2 || p[0] != 90.0 {
		t.Fatalf("latest redraw: %v", got)
	}
}

func TestBigFilesReadFromTheEnd(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "train.log")
	var b strings.Builder
	for i := 1; i <= 60000; i++ {
		fmt.Fprintf(&b, "step %d/60000 loss=%.4f\n", i, 1/float64(i))
	}
	os.WriteFile(log, []byte(b.String()), 0o644)
	body, err := fetchFile(context.Background(), Spec{Source: log})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Decode(body, nil)
	if p, _ := got["progress.step"].([]any); len(p) != 2 || p[0] != 60000.0 {
		t.Fatalf("big log: %v", got["progress.step"])
	}
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, []byte(`{"x":"`+strings.Repeat("y", 2<<20)+`"}`), 0o644)
	if _, err := fetchFile(context.Background(), Spec{Source: big}); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("big JSON: %v", err)
	}
}

type persistStore struct{ memStore }

func (p *persistStore) Watches() []Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Spec
	for _, s := range p.specs {
		out = append(out, s)
	}
	return out
}

func TestWatcherDoesNotReportAgainAfterRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(file, []byte(`{"status":"done","progress":"10/10"}`), 0o644)
	store := &persistStore{memStore{specs: map[string]Spec{}}}
	pushes := 0
	push := func(string, map[string]any) error { pushes++; return nil }
	m := NewManager(push, store)
	if err := m.Set(Spec{Job: "j", Kind: KindFile, Source: file, EveryS: 60}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	m.Remove("j")
	store.specs["j"] = m.lastSpecForTest // what was saved after the push
	m2 := NewManager(push, store)        // the app restarts
	m2.Start()
	time.Sleep(200 * time.Millisecond)
	if pushes != 1 {
		t.Fatalf("pushes = %d (finished source reported again)", pushes)
	}
}
