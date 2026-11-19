// Command pulse-app is the Pulse menubar app: HTTP API + engine + store,
// with a tray icon, popover and notifications. `--headless` runs everything
// except the UI (used for tests and debugging).
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

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

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

func main() {
	headless := flag.Bool("headless", false, "run without the menubar UI")
	addr := flag.String("addr", "127.0.0.1:7777", "preferred listen address (falls back to a free port)")
	flag.Parse()

	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		log.Fatal(err)
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
	if c, err := client.Resolve(false); err == nil && c.Healthy(context.Background()) {
		log.Printf("Pulse is already running at %s", c.BaseURL)
		return
	}

	st, err := store.Open(paths.DB())
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()
	pushTok, err := paths.EnsureToken(paths.PushToken())
	if err != nil {
		log.Fatalf("push token: %v", err)
	}
	adminTok, err := paths.EnsureToken(paths.AdminToken())
	if err != nil {
		log.Fatalf("admin token: %v", err)
	}

	notifier := &switchNotifier{}
	eng, err := engine.New(st, notifier)
	if err != nil {
		log.Fatalf("engine: %v", err)
	}
	eng.Logs = logs.New(filepath.Join(paths.Home(), "logs"))
	srv := server.New(eng, pushTok, adminTok)
	srv.Version = version
	if err := srv.Listen(*addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
	if err := paths.WriteServerInfo(paths.ServerInfo{URL: srv.URL(), PID: os.Getpid(), Version: version}); err != nil {
		log.Fatalf("write server info: %v", err)
	}
	defer removeServerInfo()

	srv.StartRemote() // re-enable remote access if it was on
	srv.Watches = watch.NewManager(func(job string, raw map[string]any) error {
		_, err := eng.PushFrom(job, raw, false, "watch")
		return err
	}, eng)
	srv.Watches.Start()

	stop := make(chan struct{})
	eng.Start(stop)
	go func() {
		if err := srv.Serve(); err != nil {
			log.Printf("server: %v", err)
		}
	}()
	log.Printf("Pulse %s listening on %s", version, srv.URL())

	if *headless {
		notifier.set(logNotifier{})
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		close(stop)
		return
	}
	runUI(eng, srv, notifier)
	close(stop)
}

// removeServerInfo deletes server.json if it still points at this process.
func removeServerInfo() {
	if si, err := paths.ReadServerInfo(); err == nil && si.PID == os.Getpid() {
		_ = os.Remove(paths.ServerFile())
	}
}

// switchNotifier lets the engine start before the UI's notifier exists.
type switchNotifier struct {
	mu    sync.Mutex
	n     engine.Notifier
	once  sync.Once
	queue chan model.Notification
}

func (s *switchNotifier) set(n engine.Notifier) {
	s.mu.Lock()
	s.n = n
	s.mu.Unlock()
}

// Notify queues the notification; a single goroutine delivers them in
// order, so a slow OS call never holds up the request that caused it (a
// slow response makes the CLI retry).
func (s *switchNotifier) Notify(n model.Notification) {
	s.once.Do(func() {
		s.queue = make(chan model.Notification, 256)
		go func() {
			for n := range s.queue {
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

type logNotifier struct{}

func (logNotifier) Notify(n model.Notification) {
	log.Printf("NOTIFY [%s] %s | %s | %s", n.Level, n.Title, n.Subtitle, n.Body)
}
