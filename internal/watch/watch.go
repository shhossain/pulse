// Package watch polls external sources (a command, a URL, a file, a GitHub
// Actions run) and turns what they report into job updates, so a job that
// can't call Pulse itself can still be monitored.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pulse/internal/actions"
	"pulse/internal/parse"
)

// Kinds of source.
const (
	KindCmd    = "cmd"
	KindURL    = "url"
	KindFile   = "file"
	KindGitHub = "github"
)

const (
	MinEvery     = 5 * time.Second
	maxBody      = 1 << 20
	fetchTimeout = 60 * time.Second
)

// Spec describes one watcher (one per job).
type Spec struct {
	Job     string            `json:"job"`
	Kind    string            `json:"kind"`
	Source  string            `json:"source"` // command, URL, path, or GitHub run URL
	EveryS  float64           `json:"every_s"`
	Map     map[string]string `json:"map,omitempty"` // dest key → JSON path ("a.b" or "a.done/a.total")
	Dir     string            `json:"dir,omitempty"` // working folder for commands
	Created time.Time         `json:"created"`
	// LastHash is what was last reported, kept across restarts so an
	// unchanged (e.g. finished) source isn't reported again as a new run.
	LastHash string `json:"last_hash,omitempty"`
}

// Every is the polling interval.
func (s Spec) Every() time.Duration { return time.Duration(s.EveryS * float64(time.Second)) }

// Status is a watcher's spec plus how it's doing.
type Status struct {
	Spec
	LastPoll  *time.Time `json:"last_poll,omitempty"`
	LastPush  *time.Time `json:"last_push,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Failures  int        `json:"failures"`
}

var ghRunRe = regexp.MustCompile(`^https://github\.com/([\w.-]+)/([\w.-]+)/actions/runs/(\d+)(?:[/?#]|$)`)

// Validate checks a spec and fills defaults. GitHub run URLs given as a
// url source are recognised.
func (s *Spec) Validate() error {
	if s.Kind == KindURL && ghRunRe.MatchString(s.Source) {
		s.Kind = KindGitHub
	}
	switch s.Kind {
	case KindCmd:
		if strings.TrimSpace(s.Source) == "" {
			return errors.New("--cmd needs a command")
		}
	case KindURL:
		u, err := url.Parse(s.Source)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("--url must be an http(s) URL")
		}
	case KindFile:
		if st, err := os.Stat(s.Source); err != nil || st.IsDir() {
			return fmt.Errorf("--file: %s isn't a readable file", s.Source)
		}
	case KindGitHub:
		if !ghRunRe.MatchString(s.Source) {
			return errors.New("GitHub source must look like https://github.com/<owner>/<repo>/actions/runs/<id>")
		}
	default:
		return errors.New("pick one source: --cmd, --url, --file or a GitHub Actions run URL")
	}
	if s.EveryS == 0 {
		s.EveryS = 60
		if s.Kind == KindGitHub {
			s.EveryS = 15
		}
	}
	if s.Every() < MinEvery {
		return fmt.Errorf("--every must be at least %s", MinEvery)
	}
	for dst, src := range s.Map {
		if dst == "" || src == "" {
			return fmt.Errorf("bad --map %q=%q (use key=path, e.g. progress=stats.done/stats.total)", dst, src)
		}
	}
	return nil
}

// GitHubJobName is the default job name for a GitHub run URL.
func GitHubJobName(src string) string {
	m := ghRunRe.FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[2] + " CI #" + m[3]
}

// Pusher receives updates (the engine).
type Pusher interface {
	Push(job string, raw map[string]any, forceNew bool) (any, error)
}

// Store persists specs (the engine's job config).
type Store interface {
	SaveWatch(job string, s *Spec) error
	Watches() []Spec
}

// Manager runs one goroutine per watcher.
type Manager struct {
	push  func(job string, raw map[string]any) error
	store Store

	mu      sync.Mutex
	running map[string]*watcher

	lastSpecForTest Spec // the spec of the last removed watcher (tests)
	fetchers        map[string]func(ctx context.Context, s Spec) ([]byte, error)
}

type watcher struct {
	spec   Spec
	cancel context.CancelFunc
	status Status
	last   string // hash of the last payload pushed
}

// NewManager makes a manager. push sends one update for a job.
func NewManager(push func(job string, raw map[string]any) error, store Store) *Manager {
	m := &Manager{push: push, store: store, running: map[string]*watcher{}}
	m.fetchers = map[string]func(context.Context, Spec) ([]byte, error){
		KindCmd: fetchCmd, KindURL: fetchURL, KindFile: fetchFile, KindGitHub: fetchGitHub,
	}
	return m
}

// Start runs every stored watcher.
func (m *Manager) Start() {
	for _, s := range m.store.Watches() {
		m.run(s)
	}
}

// Set adds or replaces a job's watcher and saves it.
func (m *Manager) Set(s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.Created.IsZero() {
		s.Created = time.Now().UTC()
	}
	m.mu.Lock()
	if old := m.running[s.Job]; old != nil && old.spec.Kind == s.Kind && old.spec.Source == s.Source {
		s.LastHash = old.last // same source set up again: don't report it twice
	}
	m.mu.Unlock()
	if err := m.store.SaveWatch(s.Job, &s); err != nil {
		return err
	}
	m.run(s)
	return nil
}

// Remove stops and forgets a job's watcher.
func (m *Manager) Remove(job string) error {
	m.mu.Lock()
	w, ok := m.running[job]
	delete(m.running, job)
	if ok {
		m.lastSpecForTest = w.spec
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s has no watcher", job)
	}
	w.cancel()
	return m.store.SaveWatch(job, nil)
}

// List returns every watcher's status.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.running))
	for _, w := range m.running {
		out = append(out, w.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Job < out[j].Job })
	return out
}

func (m *Manager) run(s Spec) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{spec: s, cancel: cancel, status: Status{Spec: s}, last: s.LastHash}
	m.mu.Lock()
	if old := m.running[s.Job]; old != nil {
		old.cancel()
	}
	m.running[s.Job] = w
	m.mu.Unlock()
	go func() {
		m.poll(ctx, w)
		t := time.NewTicker(s.Every())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.poll(ctx, w)
			}
		}
	}()
}

// poll fetches once and pushes if the content changed.
func (m *Manager) poll(ctx context.Context, w *watcher) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	body, err := m.fetchers[w.spec.Kind](fctx, w.spec)
	cancel()
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	var raw map[string]any
	if err == nil {
		raw, err = Decode(body, w.spec.Map)
	}
	m.mu.Lock()
	w.status.LastPoll = &now
	if err != nil {
		// A half-written file or a flaky endpoint: report it, keep polling,
		// push nothing (a bad read must never look like real data).
		w.status.LastError, w.status.Failures = err.Error(), w.status.Failures+1
		fails := w.status.Failures
		m.mu.Unlock()
		if fails == 3 || fails%60 == 0 {
			log.Printf("watch %s: %v (%d failures in a row)", w.spec.Job, err, fails)
		}
		return
	}
	sum := sha256.Sum256(body)
	hash := fmt.Sprintf("%x", sum[:8])
	unchanged := hash == w.last
	w.status.LastError, w.status.Failures = "", 0
	m.mu.Unlock()
	if unchanged {
		return // same content: nothing new (and a finished source doesn't start new runs)
	}
	if err := m.push(w.spec.Job, raw); err != nil {
		log.Printf("watch %s: push: %v", w.spec.Job, err)
		return
	}
	m.mu.Lock()
	w.last = hash
	w.spec.LastHash = hash
	w.status.LastPush = &now
	spec := w.spec
	m.mu.Unlock()
	if err := m.store.SaveWatch(spec.Job, &spec); err != nil {
		log.Printf("watch %s: save: %v", spec.Job, err)
	}
	if w.spec.Kind == KindGitHub && isTerminal(raw) {
		log.Printf("watch %s: GitHub run finished; watcher removed", w.spec.Job)
		_ = m.Remove(w.spec.Job)
	}
}

func isTerminal(raw map[string]any) bool {
	s, _ := raw["status"].(string)
	return s == "done" || s == "failed" || s == "stopped"
}

// Decode turns fetched content into a raw update: a JSON object (with
// --map applied), or, for plain text, the same line parser `pulse run` uses.
func Decode(body []byte, mapping map[string]string) (map[string]any, error) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, errors.New("source returned nothing")
	}
	if strings.HasPrefix(text, "{") {
		var obj map[string]any
		// Python's json.dump writes bare NaN/Infinity, which isn't JSON:
		// quote them (Pulse reads "NaN" and "Infinity" as numbers).
		if err := json.Unmarshal([]byte(bareNonFinite.ReplaceAllString(text, `$1"$2"`)), &obj); err != nil {
			return nil, fmt.Errorf("not valid JSON yet (half-written?): %v", err)
		}
		return applyMap(obj, mapping)
	}
	if len(mapping) > 0 {
		return nil, errors.New("--map needs the source to print a JSON object")
	}
	out := map[string]any{}
	// Later lines win, and "\r" ends a line too (progress-bar redraws).
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if ev, ok := parse.Line(line); ok {
			for k, v := range ev {
				out[k] = v
			}
		}
	}
	delete(out, "_coalesce")
	if len(out) == 0 {
		return nil, errors.New("no progress or metrics found in the output")
	}
	return out, nil
}

var bareNonFinite = regexp.MustCompile(`([:\[,]\s*)(-?Infinity|NaN)\b`)

// applyMap renames/combines fields: dest=path or dest=pathA/pathB (a ratio).
// Mapped source fields are dropped so they aren't reported twice.
func applyMap(obj map[string]any, mapping map[string]string) (map[string]any, error) {
	if len(mapping) == 0 {
		return obj, nil
	}
	out := map[string]any{}
	used := map[string]bool{}
	for dst, src := range mapping {
		if a, b, ok := strings.Cut(src, "/"); ok {
			av, aok := lookup(obj, a)
			bv, bok := lookup(obj, b)
			if !aok || !bok {
				return nil, fmt.Errorf("--map %s: %q or %q not found", dst, a, b)
			}
			out[dst] = []any{av, bv}
			used[top(a)], used[top(b)] = true, true
			continue
		}
		v, ok := lookup(obj, src)
		if !ok {
			return nil, fmt.Errorf("--map %s: %q not found", dst, src)
		}
		out[dst] = v
		used[top(src)] = true
	}
	for k, v := range obj {
		if _, set := out[k]; !set && !used[k] {
			out[k] = v
		}
	}
	return out, nil
}

func top(path string) string {
	k, _, _ := strings.Cut(strings.TrimPrefix(path, "."), ".")
	return k
}

func lookup(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, part := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// --- fetchers ---

// budget is how long a fetch may take: until ctx's deadline, else the default.
func budget(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return fetchTimeout
}

func fetchCmd(ctx context.Context, s Spec) ([]byte, error) {
	dir := s.Dir
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	r := actions.ShellStdout(ctx, s.Source, dir, []string{"PULSE_JOB=" + s.Job}, budget(ctx))
	if r.TimedOut {
		return nil, errors.New("command timed out")
	}
	if r.ExitCode != 0 {
		msg := strings.TrimSpace(r.Stderr)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("command exited %d: %s", r.ExitCode, msg)
	}
	return []byte(r.Output), nil
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func fetchURL(ctx context.Context, s Spec) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.Source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pulse-watch/1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err == nil && len(b) > maxBody {
		return nil, fmt.Errorf("response is over %d MB", maxBody>>20)
	}
	return b, err
}

// fetchFile reads a JSON file whole (it must fit), or the end of a text
// file: a growing log's latest lines are what matter.
func fetchFile(ctx context.Context, s Spec) ([]byte, error) {
	f, err := os.Open(s.Source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() <= maxBody {
		return io.ReadAll(f)
	}
	head := make([]byte, 1)
	if _, err := f.ReadAt(head, 0); err == nil && head[0] == '{' {
		return nil, fmt.Errorf("JSON file is over %d MB", maxBody>>20)
	}
	buf := make([]byte, maxBody)
	if _, err := f.ReadAt(buf, st.Size()-maxBody); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if i := strings.IndexAny(string(buf), "\n\r"); i >= 0 { // drop the partial first line
		buf = buf[i+1:]
	}
	return buf, nil
}
