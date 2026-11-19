package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pulse/internal/model"
	"pulse/internal/normalize"
	"pulse/internal/paths"
)

const (
	flushEvery     = 250 * time.Millisecond // ≤ 4 sends per second
	heartbeatEvery = 10 * time.Second
	maxBatch       = 500
	maxBatchBytes  = 1 << 20 // well under the server's request limit
	maxPending     = 20000
	maxLogPending  = 5000
	maxLogLine     = 4000
)

// RunSender delivers a run's events in the background. It never blocks the
// caller: events are buffered, coalesced, retried with backoff, and the app
// is launched if it isn't running.
type RunSender struct {
	RunID string
	Job   string
	Meta  model.RunMeta

	mu        sync.Mutex
	pending   []map[string]any
	inflight  int // entries at the front of pending currently being sent
	logs      []string
	logsOut   int   // lines at the front of logs currently being sent
	logsSent  int64 // lines delivered so far (numbers the next batch)
	seq       int64
	id        string // sequence numbers are per sender
	client    *Client
	lastOK    time.Time
	everOK    bool
	failures  int
	nextTry   time.Time
	launched  bool
	stop      chan struct{}
	loopDone  chan struct{}
	startOnce sync.Once
}

func NewRunSender(runID, job string, meta model.RunMeta) *RunSender {
	return &RunSender{RunID: runID, Job: job, Meta: meta, id: "runner-" + paths.RandomHex(4),
		stop: make(chan struct{}), loopDone: make(chan struct{})}
}

// Add queues an event. Consecutive events marked _coalesce (tqdm redraws)
// replace each other instead of piling up.
func (s *RunSender) Add(ev map[string]any) {
	if _, ok := ev[normalize.KeyTS]; !ok {
		ev[normalize.KeyTS] = float64(time.Now().UnixNano()) / 1e9
	}
	coalesce, _ := ev[normalize.KeyCoalesce].(bool)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev[normalize.KeySeq] = s.seq
	if n := len(s.pending); coalesce && n > s.inflight {
		// Replace only a redraw of the same bar(s): nested tqdm bars take
		// turns, and the outer bar's update must not be thrown away.
		if last, _ := s.pending[n-1][normalize.KeyCoalesce].(bool); last && sameBars(s.pending[n-1], ev) {
			s.pending[n-1] = ev
			return
		}
	}
	s.pending = append(s.pending, ev)
	if len(s.pending) > maxPending { // app unreachable for a long time: drop oldest
		drop := len(s.pending) - maxPending
		if drop > len(s.pending)-s.inflight {
			drop = len(s.pending) - s.inflight
		}
		s.pending = append(s.pending[:s.inflight], s.pending[s.inflight+drop:]...)
	}
}

// AddLog queues an output line for the run's log.
func (s *RunSender) AddLog(line string) {
	if len(line) > maxLogLine {
		cut := maxLogLine
		for cut > 0 && !utf8.RuneStart(line[cut]) { // don't split a character
			cut--
		}
		line = line[:cut] + "…"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, line)
	if over := len(s.logs) - maxLogPending; over > 0 {
		// Drop the oldest lines that aren't in flight: the in-flight ones are
		// removed by position when the send succeeds.
		over = min(over, len(s.logs)-s.logsOut)
		s.logs = append(s.logs[:s.logsOut], s.logs[s.logsOut+over:]...)
	}
}

// Start begins background delivery.
func (s *RunSender) Start() {
	s.startOnce.Do(func() {
		go func() {
			defer close(s.loopDone)
			t := time.NewTicker(flushEvery)
			defer t.Stop()
			for {
				select {
				case <-s.stop:
					return
				case <-t.C:
					s.flush(false)
				}
			}
		}()
	})
}

// Close queues final, stops the background loop and tries to deliver
// everything within timeout. It reports whether the app ever received data.
func (s *RunSender) Close(final map[string]any, timeout time.Duration) bool {
	if final != nil {
		s.Add(final)
	}
	s.Start()
	close(s.stop)
	<-s.loopDone
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		empty := len(s.pending) == 0 && len(s.logs) == 0
		s.mu.Unlock()
		if empty {
			break
		}
		if !s.flush(true) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everOK && len(s.pending) == 0
}

// flush sends one batch. force ignores the retry backoff. Returns true on success.
func (s *RunSender) flush(force bool) bool {
	s.mu.Lock()
	now := time.Now()
	n := min(len(s.pending), maxBatch)
	nl := min(len(s.logs), 2000)
	n, nl = fitBudget(s.pending[:n], s.logs[:nl])
	if n == 0 && nl == 0 && now.Sub(s.lastOK) < heartbeatEvery {
		s.mu.Unlock()
		return true
	}
	if !force && now.Before(s.nextTry) {
		s.mu.Unlock()
		return false
	}
	batch := append([]map[string]any(nil), s.pending[:n]...)
	logLines := append([]string(nil), s.logs[:nl]...)
	s.inflight, s.logsOut = n, nl
	c, job, logFrom := s.client, s.Job, s.logsSent
	s.mu.Unlock()

	err := s.send(c, job, batch, logLines, logFrom)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight, s.logsOut = 0, 0
	var unsendable *json.UnsupportedValueError
	if errors.As(err, &unsendable) { // can never succeed: drop it rather than wedge the run
		s.pending = s.pending[n:]
		s.logs = dropFront(s.logs, nl)
		return false
	}
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status == 401 {
			s.client = nil // re-discover: new port after a restart, or a rotated token
		}
		s.failures++
		backoff := min(flushEvery<<min(s.failures, 6), 15*time.Second)
		s.nextTry = time.Now().Add(backoff)
		if !s.launched {
			s.launched = true
			go LaunchApp()
		}
		if apiErr != nil && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 401 {
			s.pending = s.pending[n:] // the app rejected this data; don't retry it forever
			s.logs = dropFront(s.logs, nl)
			s.logsSent += int64(nl)
		}
		return false
	}
	s.pending = s.pending[n:]
	s.logs = dropFront(s.logs, nl)
	s.logsSent += int64(nl)
	s.lastOK = time.Now()
	s.everOK = true
	s.failures = 0
	return true
}

// fitBudget trims a batch to about maxBatchBytes (always keeping at least
// one event or line, so progress is made).
func fitBudget(events []map[string]any, lines []string) (int, int) {
	size, n := 0, 0
	for n < len(events) {
		b, _ := json.Marshal(events[n])
		if n > 0 && size+len(b) > maxBatchBytes/2 {
			break
		}
		size += len(b)
		n++
	}
	nl := 0
	for nl < len(lines) {
		l := len(lines[nl]) + 8
		if (n > 0 || nl > 0) && size+l > maxBatchBytes {
			break
		}
		size += l
		nl++
	}
	return n, nl
}

// dropFront removes the first n lines; AddLog may have trimmed the slice
// meanwhile, so never drop more than what's there.
func dropFront(l []string, n int) []string { return l[min(n, len(l)):] }

func (s *RunSender) send(c *Client, job string, batch []map[string]any, logLines []string, logFrom int64) error {
	if c == nil {
		var err error
		if c, err = Resolve(false); err != nil {
			return err
		}
		s.mu.Lock()
		s.client = c
		s.mu.Unlock()
	}
	// A big backlog over a slow remote link needs more than a few seconds.
	size := 0
	for _, l := range logLines {
		size += len(l)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second+time.Duration(size/(64<<10))*time.Second)
	defer cancel()
	if batch == nil {
		batch = []map[string]any{}
	}
	var resp model.BatchResponse
	err := c.Do(ctx, "POST", "/api/v1/runs/"+url.PathEscape(s.RunID)+"/events",
		model.Batch{Job: job, Sender: s.id, Meta: s.Meta, Events: batch, Log: logLines, LogFrom: logFrom}, &resp)
	if err == nil && resp.Job != "" {
		s.mu.Lock()
		s.Job = resp.Job
		s.mu.Unlock()
	}
	return err
}

func sameBars(a, b map[string]any) bool {
	bars := func(m map[string]any) string {
		var ks []string
		for k := range m {
			if strings.HasPrefix(k, "progress.") {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	return bars(a) == bars(b)
}
