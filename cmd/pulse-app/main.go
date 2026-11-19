// Command pulse-app is the Pulse menubar app: HTTP API + engine + store,
// with a tray icon, popover and notifications. `--headless` runs everything
// except the UI (used for tests and debugging).
package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"pulse/internal/app"
)

// version is set at build time (-ldflags -X main.version=…).
var version = "dev"

func main() {
	headless := flag.Bool("headless", false, "run without the menubar UI")
	addr := flag.String("addr", "127.0.0.1:7777", "preferred listen address (falls back to a free port)")
	flag.Parse()

	a, err := app.Start(app.Options{Addr: *addr, Version: version})
	if errors.Is(err, app.ErrAlreadyRunning) {
		log.Print(err)
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	if *headless {
		a.Notifier.Set(app.LogNotifier{})
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		return
	}
	runUI(a.Eng, a.Srv, a.Notifier)
}
