package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pulse/internal/audit"
)

// Limits contain what one job token can do, so a compromised job machine can
// only damage its own job: it can't fill the disk (everything stored arrives
// in requests), flood the API, open endless runs or bury the user in
// questions. A job over a limit is slowed down, never cut off from what keeps
// its runs honest: status updates, Stop and answers still get through.
// They apply to job tokens only; this machine's own tokens are trusted.
// Zero disables a limit.
type Limits struct {
	RequestsPerSecond float64 // data requests (events, pushes) per job token
	RequestBurst      int
	ControlPerSecond  float64 // control requests (waiting for commands or answers, acks), their own budget
	ControlBurst      int
	// BytesPerDay is what a job token may send in a rolling 24 hours. Past
	// it the job is degraded rather than refused: its log lines are dropped,
	// and it may send DegradedPerSecond requests of at most DegradedBytes,
	// enough for status, progress and metrics to stay current.
	BytesPerDay       int64
	DegradedPerSecond float64
	DegradedBytes     int64
	RunsPerHour       int // new runs per job token
	AsksPerHour       int // questions per job token
}

// DefaultLimits sit well above an honest job. The sender sends at most 4
// requests a second per run, about 0.5 KB each when output streams, so one
// busy run is about 170 MB a day and 10 concurrent busy runs of one job
// stay under 50 requests a second. A run's own log is capped at 50 MB.
var DefaultLimits = Limits{
	RequestsPerSecond: 50,
	RequestBurst:      200,
	ControlPerSecond:  10,
	ControlBurst:      50,
	BytesPerDay:       2 << 30,
	DegradedPerSecond: 1,
	DegradedBytes:     16 << 10,
	RunsPerHour:       120,
	AsksPerHour:       20,
}

// limiter tracks usage per job (a job token may only touch its own job, so
// the job names the token's budget; several tokens for one job share it).
type limiter struct {
	mu   sync.Mutex
	now  func() time.Time
	jobs map[string]*usage
	// onTrip is told the first time in an hour a job hits a limit.
	onTrip func(job, what string)
}

type usage struct {
	data, control, degraded bucket
	bytes                   window // request bytes, 24h
	runs                    window // new runs, 1h
	asks                    window // questions, 1h
	tripped                 map[string]time.Time
}

// bucket is a token bucket.
type bucket struct {
	tokens   float64
	refilled time.Time
	primed   bool
}

func (b *bucket) take(now time.Time, rate float64, burst int) (ok bool, wait time.Duration) {
	if rate <= 0 {
		return true, 0
	}
	if burst < 1 {
		burst = 1
	}
	if !b.primed {
		b.tokens, b.refilled, b.primed = float64(burst), now, true
	}
	b.tokens = math.Min(float64(burst), b.tokens+now.Sub(b.refilled).Seconds()*rate)
	b.refilled = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// window counts events in a rolling period, in coarse buckets so memory
// stays small however busy a job is.
type window struct {
	span    time.Duration
	buckets [24]int64
	starts  [24]time.Time
}

func (w *window) slot(now time.Time) int {
	size := w.span / time.Duration(len(w.buckets))
	start := now.Truncate(size)
	i := int(start.UnixNano()/int64(size)) % len(w.buckets)
	if !w.starts[i].Equal(start) {
		w.starts[i], w.buckets[i] = start, 0
	}
	return i
}

func (w *window) add(now time.Time, n int64) { w.buckets[w.slot(now)] += n }

func (w *window) sum(now time.Time) int64 {
	var total int64
	for i, s := range w.starts {
		if now.Sub(s) < w.span {
			total += w.buckets[i]
		}
	}
	return total
}

// freeIn is roughly how long until the window's total drops below max.
func (w *window) freeIn(now time.Time, max int64) time.Duration {
	size := w.span / time.Duration(len(w.buckets))
	for d := size; d <= w.span; d += size {
		at := now.Add(d)
		var left int64
		for i, s := range w.starts {
			if at.Sub(s) < w.span {
				left += w.buckets[i]
			}
		}
		if left < max {
			return d
		}
	}
	return w.span
}

func newLimiter() *limiter {
	return &limiter{now: time.Now, jobs: map[string]*usage{}}
}

func (l *limiter) get(job string) *usage {
	u := l.jobs[job]
	if u == nil {
		u = &usage{bytes: window{span: 24 * time.Hour}, runs: window{span: time.Hour},
			asks: window{span: time.Hour}, tripped: map[string]time.Time{}}
		l.jobs[job] = u
	}
	return u
}

// errLimit is a refusal: what was exceeded, and when to try again.
type errLimit struct {
	what  string
	retry time.Duration
}

func (e *errLimit) Error() string {
	return fmt.Sprintf("this job token is over its %s; try again in %s", e.what, max(e.retry, time.Second).Round(time.Second))
}

// admission is what a request may do, decided when it arrives.
type admission struct {
	degraded bool // over the daily data quota: no log lines, small requests only
}

// admit takes one request from job's budget. control requests (waiting for
// commands or answers, acks) have their own budget. reserve is the request's
// declared size, counted now so concurrent requests see each other.
func (l *limiter) admit(job string, lim Limits, control bool, reserve int64) (admission, *errLimit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	u := l.get(job)
	if control {
		if ok, wait := u.control.take(now, lim.ControlPerSecond, lim.ControlBurst); !ok {
			return admission{}, l.trip(job, u, now, &errLimit{"control request rate", wait})
		}
		return admission{}, nil
	}
	var a admission
	if lim.BytesPerDay > 0 && u.bytes.sum(now) >= lim.BytesPerDay {
		a.degraded = true
		l.trip(job, u, now, &errLimit{what: "daily data quota"})
		if lim.DegradedBytes > 0 && reserve > lim.DegradedBytes {
			return a, &errLimit{"daily data quota", u.bytes.freeIn(now, lim.BytesPerDay)}
		}
		if ok, wait := u.degraded.take(now, lim.DegradedPerSecond, 1); !ok {
			return a, &errLimit{"daily data quota", wait}
		}
	} else if ok, wait := u.data.take(now, lim.RequestsPerSecond, lim.RequestBurst); !ok {
		return a, l.trip(job, u, now, &errLimit{"request rate", wait})
	}
	if reserve > 0 {
		u.bytes.add(now, reserve)
	}
	return a, nil
}

// addBytes records bytes a request for job sent beyond what it reserved.
func (l *limiter) addBytes(job string, n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.get(job).bytes.add(now, n)
}

// newRun takes one new run from job's budget.
func (l *limiter) newRun(job string, lim Limits) *errLimit {
	return l.count(job, lim.RunsPerHour, "hourly new-run limit", func(u *usage) *window { return &u.runs })
}

// ask takes one question from job's budget.
func (l *limiter) ask(job string, lim Limits) *errLimit {
	return l.count(job, lim.AsksPerHour, "hourly question limit", func(u *usage) *window { return &u.asks })
}

func (l *limiter) count(job string, limit int, what string, w func(*usage) *window) *errLimit {
	if limit <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	u := l.get(job)
	if w(u).sum(now) >= int64(limit) {
		return l.trip(job, u, now, &errLimit{what, w(u).freeIn(now, int64(limit))})
	}
	w(u).add(now, 1)
	return nil
}

// trip records a limit being hit, in the audit log and to onTrip, once an
// hour per job and limit: a token hitting a limit is worth a look, a flood
// of log lines isn't. Called with l.mu held.
func (l *limiter) trip(job string, u *usage, now time.Time, e *errLimit) *errLimit {
	if last, ok := u.tripped[e.what]; !ok || now.Sub(last) > time.Hour {
		u.tripped[e.what] = now
		audit.Log("limit.exceeded", map[string]any{"job": job, "limit": e.what})
		if l.onTrip != nil {
			go l.onTrip(job, e.what)
		}
	}
	return e
}

// refuse writes a 429 for a limit.
func refuse(w http.ResponseWriter, e *errLimit) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(e.retry, time.Second).Seconds()))))
	writeErr(w, http.StatusTooManyRequests, e.Error())
}

// countingBody counts what a handler reads from a request body.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
