// Package app starts Pulse's core: store, engine, HTTP API and background
// watches. The menubar app (cmd/pulse-app) adds a UI on top; the hub
// (cmd/pulse-hub) runs it alone on an always-on server.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pulse/internal/audit"
	"pulse/internal/client"
	"pulse/internal/engine"
	"pulse/internal/logs"
	"pulse/internal/model"
	"pulse/internal/paths"
	"pulse/internal/rotate"
	"pulse/internal/server"
	"pulse/internal/store"
	"pulse/internal/watch"
)

// ErrAlreadyRunning means another Pulse answers on this data folder.
var ErrAlreadyRunning = errors.New("pulse is already running")

// Options configure Start.
type Options struct {
	Addr    string // preferred listen address (falls back to a free port)
	Version string
}

// App is a running Pulse core.
type App struct {
	Eng      *engine.Engine
	Srv      *server.Server
	Notifier *SwitchNotifier

	st   *store.Store
	stop chan struct{}
}

// Start opens the data folder and starts everything but a UI. Notifications
// queue in Notifier until a destination is set with Notifier.Set.
func Start(opts Options) (*App, error) {
	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		return nil, err
	}
	if lf, err := rotate.Open(filepath.Join(paths.Home(), "pulse.log"), 10<<20); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	}
	if af, err := rotate.Open(paths.AuditLog(), 20<<20); err == nil {
		af.Keep = 10 // a security log: keep ~200 MB of history
		audit.SetOutput(af)
	} else {
		log.Printf("audit log unavailable: %v", err)
	}

	// Single instance: if another Pulse answers, leave it alone.
	// (ResolveLocal: PULSE_URL may name another machine's Pulse, e.g. a hub.)
	if c, err := client.ResolveLocal(false); err == nil && c.Healthy(context.Background()) {
		return nil, fmt.Errorf("%w at %s", ErrAlreadyRunning, c.BaseURL)
	}

	st, err := store.Open(paths.DB())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	a := &App{st: st, stop: make(chan struct{}), Notifier: &SwitchNotifier{}}
	fail := func(err error) (*App, error) { st.Close(); return nil, err }

	pushTok, err := paths.EnsureToken(paths.PushToken())
	if err != nil {
		return fail(fmt.Errorf("push token: %w", err))
	}
	adminTok, err := paths.EnsureToken(paths.AdminToken())
	if err != nil {
		return fail(fmt.Errorf("admin token: %w", err))
	}
	a.Eng, err = engine.New(st, a.Notifier)
	if err != nil {
		return fail(fmt.Errorf("engine: %w", err))
	}
	a.Eng.Logs = logs.New(filepath.Join(paths.Home(), "logs"))
	a.Srv = server.New(a.Eng, pushTok, adminTok)
	a.Srv.Version = opts.Version
	a.Srv.OnLimit = func(job, what string) {
		a.Notifier.Notify(model.Notification{Level: "warn", Job: job,
			Title:    job + ": hit its " + what,
			Subtitle: "Pulse is slowing this job's token down.",
			Body:     "Unexpected? Its machine may be compromised: pulse remote tokens, then pulse remote revoke <id>."})
	}
	if err := a.Srv.Listen(opts.Addr); err != nil {
		return fail(fmt.Errorf("listen: %w", err))
	}
	if err := paths.WriteServerInfo(paths.ServerInfo{URL: a.Srv.URL(), PID: os.Getpid(), Version: opts.Version}); err != nil {
		return fail(fmt.Errorf("write server info: %w", err))
	}

	a.Srv.StartRemote() // re-enable remote access if it was on
	a.Srv.Watches = watch.NewManager(func(job string, raw map[string]any) error {
		_, err := a.Eng.PushFrom(job, raw, false, "watch")
		return err
	}, a.Eng)
	a.Srv.Watches.Start()

	a.Eng.Start(a.stop)
	go func() {
		if err := a.Srv.Serve(); err != nil {
			log.Printf("server: %v", err)
		}
	}()
	log.Printf("Pulse %s listening on %s", opts.Version, a.Srv.URL())
	return a, nil
}

// Close stops the engine and releases the data folder.
func (a *App) Close() {
	close(a.stop)
	if si, err := paths.ReadServerInfo(); err == nil && si.PID == os.Getpid() {
		_ = os.Remove(paths.ServerFile())
	}
	a.st.Close()
}

// SwitchNotifier lets the engine start before the notifier that shows
// notifications exists (the UI's, or the hub's). It also caps notifications
// per job (see NotifyPerHour), so one runaway or compromised job can't bury
// the others.
type SwitchNotifier struct {
	mu    sync.Mutex
	n     engine.Notifier
	once  sync.Once
	queue chan model.Notification

	now  func() time.Time
	sent map[string][]time.Time // job → recent notification times
	held map[string]bool        // job → the "too many" notice went out
}

// NotifyPerHour is how many notifications one job may raise in an hour
// (questions aside). Past it, one notice says the job is being quieted;
// the rest show in the app.
const NotifyPerHour = 30

// allow reports whether a notification for job may be shown now, and
// whether it should be replaced by the one-time "too many" notice.
func (s *SwitchNotifier) allow(n model.Notification) (show, notice bool) {
	job := n.Job
	// Questions always reach the user: a job waits on them (job tokens are
	// limited to a few an hour where they're asked).
	if job == "" || n.AskID != "" {
		return true, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if s.sent == nil {
		s.sent, s.held = map[string][]time.Time{}, map[string]bool{}
	}
	recent := s.sent[job][:0]
	for _, t := range s.sent[job] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) < NotifyPerHour {
		s.sent[job] = append(recent, now)
		s.held[job] = false
		return true, false
	}
	s.sent[job] = recent
	if s.held[job] {
		return false, false
	}
	s.held[job] = true
	return true, true
}

// Set sets where notifications go from now on.
func (s *SwitchNotifier) Set(n engine.Notifier) {
	s.mu.Lock()
	s.n = n
	s.mu.Unlock()
}

// Notify queues the notification; a single goroutine delivers them in
// order, so a slow OS call never holds up the request that caused it (a
// slow response makes the CLI retry).
func (s *SwitchNotifier) Notify(n model.Notification) {
	s.once.Do(func() {
		s.queue = make(chan model.Notification, 256)
		go func() {
			for n := range s.queue {
				show, notice := s.allow(n)
				if !show {
					log.Printf("notification held (%s over %d/hour): %s", n.Job, NotifyPerHour, n.Title)
					continue
				}
				if notice {
					audit.Log("notify.held", map[string]any{"job": n.Job})
					n = model.Notification{Level: "warn", Job: n.Job, RunID: n.RunID,
						Title:    n.Job + ": too many notifications",
						Subtitle: fmt.Sprintf("Pulse is quieting this job for up to an hour (over %d an hour).", NotifyPerHour),
						Body:     "Its runs and questions still show in Pulse."}
				}
				s.mu.Lock()
				inner := s.n
				s.mu.Unlock()
				if inner != nil {
					inner.Notify(n)
				} else {
					log.Printf("notification (no UI yet): %s — %s", n.Title, n.Subtitle)
				}
			}
		}()
	})
	select {
	case s.queue <- n:
	default:
		log.Printf("notification dropped (queue full): %s", n.Title)
	}
}

// Notifiers sends each notification to all of them.
type Notifiers []engine.Notifier

func (ns Notifiers) Notify(n model.Notification) {
	for _, x := range ns {
		x.Notify(n)
	}
}

// LogNotifier writes notifications to the log.
type LogNotifier struct{}

func (LogNotifier) Notify(n model.Notification) {
	log.Printf("NOTIFY [%s] %s | %s | %s", n.Level, n.Title, n.Subtitle, n.Body)
}
