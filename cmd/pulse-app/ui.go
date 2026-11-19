package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"pulse/internal/app"
	"pulse/internal/engine"
	"pulse/internal/model"
	"pulse/internal/server"
	"pulse/internal/trayicon"
)

// runUI starts the menubar UI and blocks until the app quits.
func runUI(eng *engine.Engine, srv *server.Server, notifier *app.SwitchNotifier) {
	ns := notifications.New()
	app := application.New(application.Options{
		Name:        "Pulse",
		Description: "Job monitor in your menubar",
		Services:    []application.Service{application.NewService(ns)},
		Assets:      application.AlphaAssets,
		Mac:         application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})

	tray := app.SystemTray.New()
	tray.SetTemplateIcon(trayicon.Ring(-1, false))
	tray.SetTooltip("Pulse")

	menu := app.NewMenu()
	menu.Add("Open Pulse").OnClick(func(*application.Context) { tray.ShowWindow() })
	menu.AddSeparator()
	menu.Add("Quit Pulse").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "popover",
		Width:           360,
		Height:          480,
		Frameless:       true,
		AlwaysOnTop:     true,
		Hidden:          true,
		DisableResize:   true,
		HideOnEscape:    true,
		HideOnFocusLost: true,
		URL:             srv.OneTimeURL("/popover"),
		Mac:             application.MacWindow{Backdrop: application.MacBackdropTranslucent},
	})
	tray.AttachWindow(win).WindowOffset(5)

	// Run pages open in their own native windows, one per page.
	var pagesMu sync.Mutex
	pages := map[string]*application.WebviewWindow{}
	srv.OpenPage = func(path string) error {
		application.InvokeSync(func() {
			pagesMu.Lock()
			defer pagesMu.Unlock()
			w, ok := pages[path]
			if ok {
				w.SetURL(srv.OneTimeURL(path)) // show the current state (e.g. /j/job's latest run)
			}
			if !ok {
				w = app.Window.NewWithOptions(application.WebviewWindowOptions{
					Title:     "Pulse",
					Width:     1040,
					Height:    760,
					MinWidth:  560,
					MinHeight: 420,
					URL:       srv.OneTimeURL(path),
					Mac: application.MacWindow{
						TitleBar:                application.MacTitleBarHiddenInset,
						InvisibleTitleBarHeight: 44,
						Backdrop:                application.MacBackdropTranslucent,
					},
				})
				pages[path] = w
				w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
					pagesMu.Lock()
					delete(pages, path)
					pagesMu.Unlock()
				})
			}
			win.Hide() // close the popover
			w.Show()
			w.Focus()
			app.Show()
		})
		return nil
	}

	wn := &wailsNotifier{ns: ns}
	notifier.Set(wn)
	ns.OnNotificationResponse(func(res notifications.NotificationResult) {
		if res.Error != nil {
			log.Printf("notification response: %v", res.Error)
			return
		}
		id, _ := res.Response.UserInfo["run"].(string)
		job, _ := res.Response.UserInfo["job"].(string)
		if askID, _ := res.Response.UserInfo["ask"].(string); askID != "" {
			act := res.Response.ActionIdentifier
			switch {
			case strings.HasPrefix(act, "answer:"):
				_, err := eng.Answer(askID, strings.TrimPrefix(act, "answer:"), "notification")
				logIf(err)
				return
			case res.Response.UserText != "": // the text field (Wails: TEXT_REPLY)
				_, err := eng.Answer(askID, res.Response.UserText, "notification")
				logIf(err)
				return
			}
		}
		switch res.Response.ActionIdentifier {
		case "stop":
			if _, err := eng.Stop(id); err != nil {
				log.Printf("stop from notification: %v", err)
			}
		case "mute":
			_ = eng.Mute(job, time.Hour)
		default: // the notification itself, or "Open"
			if id != "" {
				_ = srv.OpenPage("/r/" + id)
				return
			}
			tray.ShowWindow()
		}
	})
	go func() {
		time.Sleep(time.Second)
		ok, err := ns.RequestNotificationAuthorization()
		log.Printf("notification authorization: ok=%v err=%v", ok, err)
	}()

	// Keep the tray in sync with the engine, at most ~3 updates per second.
	dirty := make(chan struct{}, 1)
	eng.OnChange(func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	})
	go func() {
		var lastLabel string
		lastFracs, lastBadge := []float64{-2}, false
		for {
			label, fracs, badge := traySummary(eng.Snapshot())
			if label != lastLabel {
				tray.SetLabel(label)
				lastLabel = label
			}
			if ringsMoved(fracs, lastFracs) || badge != lastBadge {
				tray.SetTemplateIcon(trayicon.Rings(fracs, badge))
				lastFracs, lastBadge = fracs, badge
			}
			<-dirty
			time.Sleep(300 * time.Millisecond)
		}
	}()

	if err := app.Run(); err != nil {
		log.Printf("app: %v", err)
	}
}

// traySummary picks what the menubar shows. Running runs with progress get
// a ring each, the furthest along outermost; with more than fit, the ones
// furthest behind are kept. The label is the furthest-behind run's percent
// (when everything is done), followed by "·N" when N runs are going. Runs
// without progress count toward N but have no ring; when none has progress
// the icon is an empty ring, labeled "N▶" when several run. No running runs
// is the idle ring (fracs nil). badge is true while a failed/lost run is
// waiting in "Needs you".
func traySummary(runs []model.Run) (label string, fracs []float64, badge bool) {
	running := 0
	for i := range runs {
		r := &runs[i]
		if r.NeedsAttention() {
			badge = true
		}
		if r.Status != model.Running {
			continue
		}
		running++
		if p, ok := r.Primary(); ok {
			fracs = append(fracs, p.Frac())
		}
	}
	switch {
	case running == 0:
		return "", nil, badge
	case len(fracs) == 0 && running == 1:
		return "", []float64{0}, badge
	case len(fracs) == 0:
		return fmt.Sprintf("%d▶", running), []float64{0}, badge
	}
	slices.Sort(fracs)
	if len(fracs) > trayicon.MaxRings {
		fracs = fracs[:trayicon.MaxRings]
	}
	label = fmt.Sprintf("%d%%", int(math.Floor(fracs[0]*100+1e-9))) // 0.29*100 is 28.999…
	if running > 1 {
		label += fmt.Sprintf("·%d", running)
	}
	slices.Reverse(fracs) // outermost first
	return label, fracs, badge
}

// ringsMoved reports whether the icon for fracs looks different from the one
// for last: a ring was added or removed, or one moved by at least half a percent.
func ringsMoved(fracs, last []float64) bool {
	if len(fracs) != len(last) {
		return true
	}
	for i := range fracs {
		if math.Abs(fracs[i]-last[i]) >= 0.005 {
			return true
		}
	}
	return false
}

func logIf(err error) {
	if err != nil {
		log.Printf("notification: %v", err)
	}
}

type wailsNotifier struct {
	ns    *notifications.NotificationService
	once  sync.Once
	mu    sync.Mutex
	known map[string]bool // registered question categories
}

// askCategory registers (once) a category whose buttons are the answers.
func (w *wailsNotifier) askCategory(options []string) string {
	sum := sha256.Sum256([]byte(strings.Join(options, "\x00"))) // options may contain any character
	id := "ask:" + hex.EncodeToString(sum[:8])
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.known == nil {
		w.known = map[string]bool{}
	}
	if w.known[id] {
		return id
	}
	cat := notifications.NotificationCategory{ID: id}
	if len(options) == 0 {
		cat.HasReplyField, cat.ReplyPlaceholder, cat.ReplyButtonTitle = true, "Your answer", "Answer"
		cat.Actions = []notifications.NotificationAction{{ID: "open", Title: "Open"}}
	}
	for _, o := range options {
		cat.Actions = append(cat.Actions, notifications.NotificationAction{ID: "answer:" + o, Title: o})
	}
	if err := w.ns.RegisterNotificationCategory(cat); err != nil {
		log.Printf("register question category: %v", err)
		return id // not cached: try again next time
	}
	w.known[id] = true
	return id
}

func (w *wailsNotifier) Notify(n model.Notification) {
	w.once.Do(func() {
		for _, c := range []notifications.NotificationCategory{
			{ID: "run", Actions: []notifications.NotificationAction{
				{ID: "open", Title: "Open"},
				{ID: "mute", Title: "Mute for 1 Hour"},
			}},
			{ID: "run-live", Actions: []notifications.NotificationAction{
				{ID: "open", Title: "Open"},
				{ID: "stop", Title: "Stop", Destructive: true},
				{ID: "mute", Title: "Mute for 1 Hour"},
			}},
		} {
			if err := w.ns.RegisterNotificationCategory(c); err != nil {
				log.Printf("register notification category: %v", err)
			}
		}
	})
	category := "run"
	if n.CanStop {
		category = "run-live"
	}
	data := map[string]any{"run": n.RunID, "job": n.Job}
	if n.AskID != "" {
		category = w.askCategory(n.AskOptions)
		data["ask"] = n.AskID
	}
	opts := notifications.NotificationOptions{
		ID:         fmt.Sprintf("%s-%d", n.ID, time.Now().UnixNano()),
		Title:      n.Title,
		Subtitle:   n.Subtitle,
		Body:       n.Body,
		CategoryID: category,
		ThreadID:   n.Job, // group a job's notifications together
		Data:       data,
	}
	err := w.ns.SendNotificationWithActions(opts)
	if err != nil {
		log.Printf("notify: %v", err)
	}
}
