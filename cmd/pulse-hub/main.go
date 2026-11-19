// Command pulse-hub runs Pulse without a UI on an always-on server (Linux or
// macOS), so jobs can report and be watched while your own computer sleeps.
// It is the menubar app's core: the same engine, API, alerts and watches.
// Administer it on the server itself with the pulse CLI (same PULSE_HOME);
// expose only the push listener to other machines, e.g.
//
//	pulse remote enable --tunnel https://pulse.example.com
//
// and point a tunnel (cloudflared) at the loopback address it prints.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"pulse/internal/app"
)

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "listen address for the local API (loopback only)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pulse-hub", version)
		return
	}

	if host, _, err := net.SplitHostPort(*addr); err != nil || (host != "localhost" && !isLoopback(host)) {
		log.Fatalf("--addr %s: the local API must listen on loopback (e.g. 127.0.0.1:7777); "+
			"other machines use the push listener (pulse remote enable --tunnel …)", *addr)
	}

	a, err := app.Start(app.Options{Addr: *addr, Version: version})
	if errors.Is(err, app.ErrAlreadyRunning) {
		log.Fatal(err) // under systemd: fail loudly rather than exit 0
	}
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	a.Notifier.Set(app.LogNotifier{})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	s := <-sigs
	log.Printf("pulse-hub: %v, shutting down", s)
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
