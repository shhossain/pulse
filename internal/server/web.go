package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"pulse/internal/audit"
	"pulse/internal/cfaccess"
	"pulse/internal/model"
)

// WebConfig lets people open the UI through the tunnel: Cloudflare Access
// signs them in, and Pulse checks Access's token itself before showing
// anything. Viewers can only look; operators can also stop runs and answer
// questions (never run shell actions or change settings).
type WebConfig struct {
	Team      string   `json:"team"`                // Access team domain, e.g. myteam.cloudflareaccess.com
	AUD       string   `json:"aud"`                 // the Access application's audience tag
	Emails    []string `json:"emails"`              // who may look (lowercase), operators included
	Operators []string `json:"operators,omitempty"` // who may also act (lowercase)
	// Services are Access service tokens (client IDs) allowed to look, e.g.
	// a Mac's menubar app following this hub. Never operators.
	Services []string `json:"services,omitempty"`
}

type webAccess struct {
	cfg       WebConfig
	v         *cfaccess.Verifier
	emails    map[string]bool
	operators map[string]bool
	services  map[string]bool
	seenMu    sync.Mutex
	seen      map[string]time.Time // email → last audit entry
}

var (
	serviceIDRe = regexp.MustCompile(`^[0-9a-f]{32}\.access$`)
	teamRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)
	audRe       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validWeb checks and normalizes a WebConfig.
func validWeb(c WebConfig) (WebConfig, error) {
	c.Team = strings.ToLower(strings.TrimSpace(c.Team))
	c.AUD = strings.ToLower(strings.TrimSpace(c.AUD))
	if !teamRe.MatchString(c.Team) {
		return c, fmt.Errorf("team must be your Access team domain, like myteam.cloudflareaccess.com")
	}
	if !audRe.MatchString(c.AUD) {
		return c, fmt.Errorf("aud must be the Access application's audience tag (64 hex characters)")
	}
	clean := func(list []string) ([]string, error) {
		var out []string
		seen := map[string]bool{}
		for _, e := range list {
			e = strings.ToLower(strings.TrimSpace(e))
			if a, err := mail.ParseAddress(e); err != nil || a.Address != e || a.Name != "" {
				return nil, fmt.Errorf("%q isn't an email address", e)
			}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	ops, err := clean(c.Operators)
	if err != nil {
		return c, err
	}
	emails, err := clean(append(append([]string{}, c.Emails...), ops...)) // operators may look too
	if err != nil {
		return c, err
	}
	var svcs []string
	for _, id := range c.Services {
		id = strings.ToLower(strings.TrimSpace(id))
		if !serviceIDRe.MatchString(id) {
			return c, fmt.Errorf("%q isn't an Access service token client ID (<32 hex>.access)", id)
		}
		if !slices.Contains(svcs, id) {
			svcs = append(svcs, id)
		}
	}
	sort.Strings(svcs)
	if len(emails) == 0 {
		return c, fmt.Errorf("name at least one email that may open the UI")
	}
	c.Emails, c.Operators, c.Services = emails, ops, svcs
	return c, nil
}

func newWebAccess(c WebConfig) *webAccess {
	w := &webAccess{cfg: c, v: &cfaccess.Verifier{Team: c.Team, AUD: c.AUD},
		emails: map[string]bool{}, operators: map[string]bool{}, services: map[string]bool{}, seen: map[string]time.Time{}}
	for _, s := range c.Services {
		w.services[s] = true
	}
	for _, e := range c.Emails {
		w.emails[e] = true
	}
	for _, e := range c.Operators {
		w.operators[e] = true
	}
	return w
}

// viewer returns the signed-in person allowed to look, if r carries one.
func (wa *webAccess) viewer(r *http.Request) (string, bool) {
	tok := r.Header.Get(cfaccess.Header)
	if tok == "" {
		return "", false
	}
	id, err := wa.v.Verify(r.Context(), tok)
	if err != nil {
		return "", false
	}
	if id.Email == "" && wa.services[strings.ToLower(id.Service)] {
		id.Email = "service:" + strings.ToLower(id.Service) // a listed service: views only
	} else if id.Email == "" || !wa.emails[id.Email] {
		return "", false
	}
	wa.seenMu.Lock()
	if time.Since(wa.seen[id.Email]) > time.Hour {
		wa.seen[id.Email] = time.Now()
		audit.Log("web.viewer", map[string]any{"email": id.Email})
	}
	wa.seenMu.Unlock()
	return id.Email, true
}

func (s *Server) webAccess() *webAccess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.web
}

// viewerHandler serves the UI read-only to a signed-in viewer: pages,
// assets and the read API. Everything else is refused.
func (s *Server) viewerHandler(publicURL string) http.Handler {
	mux := http.NewServeMux()
	api := func(path string, h http.HandlerFunc) { mux.HandleFunc("GET /api/v1"+path, h) }
	api("/runs", s.listRuns)
	api("/runs/{id}", s.getRun)
	api("/runs/{id}/series", s.getSeries)
	api("/runs/{id}/log", s.getLog)
	api("/stream", s.stream)
	api("/asks", s.listAsks)
	api("/session", func(w http.ResponseWriter, r *http.Request) {
		op := s.isOperator(r)
		writeJSON(w, map[string]any{"remote": true, "read_only": !op, "can_act": op, "email": viewerEmail(r)})
	})
	// Operators: stop runs and answer questions, recorded under their
	// verified email (never a name the browser picks).
	mux.HandleFunc("POST /api/v1/runs/{id}/stop", s.operator(func(w http.ResponseWriter, r *http.Request) {
		res, err := s.eng.Stop(r.PathValue("id"))
		audit.Log("web.stop", map[string]any{"email": viewerEmail(r), "run": r.PathValue("id"), "result": res, "ok": err == nil})
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]string{"result": res})
	}))
	audited := func(event string, f func(string) bool) http.HandlerFunc {
		return s.operator(func(w http.ResponseWriter, r *http.Request) {
			audit.Log(event, map[string]any{"email": viewerEmail(r), "run": r.PathValue("id")})
			s.runAction(f)(w, r)
		})
	}
	mux.HandleFunc("POST /api/v1/runs/{id}/seen", audited("web.dismiss", s.eng.MarkSeen))
	mux.HandleFunc("POST /api/v1/runs/{id}/archive", audited("web.hide", s.eng.Archive))
	mux.HandleFunc("POST /api/v1/asks/{id}/answer", s.operator(s.webAnswer))
	// Any viewer: their own notifications.
	api("/web/push", s.pushStatus)
	mux.HandleFunc("POST /api/v1/web/push", s.pushSubscribe)
	mux.HandleFunc("POST /api/v1/web/push/unsubscribe", s.pushUnsubscribe)
	mux.HandleFunc("POST /api/v1/web/push/test", s.pushTest)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/popover", http.StatusFound)
	})
	mux.HandleFunc("GET /sw.js", s.staticFile("sw.js", "text/javascript; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /popover", s.page("popover.html"))
	mux.HandleFunc("GET /r/{id}", s.page("run.html"))
	mux.HandleFunc("GET /j/{job...}", s.jobPage)
	mux.Handle("GET /assets/", s.assets())

	origin := strings.TrimRight(publicURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if o := r.Header.Get("Origin"); o != "" && o != origin { // another site, through the viewer's browser
			writeErr(w, http.StatusForbidden, "forbidden origin")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Writes only from this site's own pages: a browser always sends
			// Origin on a POST, so a missing one isn't a browser we trust.
			if r.Method != http.MethodPost || r.Header.Get("Origin") != origin {
				writeErr(w, http.StatusForbidden, "writes must come from this site's own pages")
				return
			}
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			writeErr(w, http.StatusNotFound, "not available in the web UI")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

const viewerKey ctxKey = 100

// runsShell reports whether answering a with answer would run a shell action
// (an alert rule's "Label=action:<name>").
func runsShell(a model.Ask, answer string) bool {
	return strings.HasPrefix(a.Then[answer], "action:")
}

// anyShell reports whether any of a's answers runs a shell action.
func anyShell(a model.Ask) bool {
	for _, do := range a.Then {
		if strings.HasPrefix(do, "action:") {
			return true
		}
	}
	return false
}

// isOperator reports whether the signed-in viewer may act.
func (s *Server) isOperator(r *http.Request) bool {
	wa := s.webAccess()
	return wa != nil && wa.operators[viewerEmail(r)]
}

// operator lets only operators through to h.
func (s *Server) operator(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isOperator(r) {
			writeErr(w, http.StatusForbidden, "view only: ask the hub's admin to make you an operator")
			return
		}
		h(w, r)
	}
}

// webAnswer answers a question for an operator, as "web:<their email>".
func (s *Server) webAnswer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// An alert rule can tie an answer to a shell action; those are answered
	// on the hub, never from the web.
	if ask, ok := s.eng.GetAsk(r.PathValue("id")); ok && runsShell(ask, strings.TrimSpace(in.Answer)) {
		writeErr(w, http.StatusForbidden, "this answer runs a shell action on the hub: answer it there (pulse answer)")
		return
	}
	a, err := s.eng.Answer(r.PathValue("id"), in.Answer, "web:"+viewerEmail(r))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, a)
}

// revokeWhenAccessEnds ends a viewer's request (an open stream, really) once
// they may no longer look: the web UI was turned off or its list changed,
// or their Access token expired. Checked every 30 seconds.
func (s *Server) revokeWhenAccessEnds(ctx context.Context, cancel func(), wa *webAccess, r *http.Request) {
	t := time.NewTicker(viewerRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.webAccess() != wa {
				cancel()
				return
			}
			if _, ok := wa.viewer(r); !ok {
				cancel()
				return
			}
		}
	}
}

var viewerRecheck = 30 * time.Second

func viewerEmail(r *http.Request) string {
	e, _ := r.Context().Value(viewerKey).(string)
	return e
}

// publicFiles are served through the tunnel without signing in (Cloudflare
// Access bypasses /pwa/ for them): Chrome fetches an app's manifest and icons
// without cookies when installing it. They reveal nothing but the name Pulse.
var publicFiles = map[string]string{
	"/pwa/manifest.webmanifest":  "application/manifest+json",
	"/pwa/icon-192.png":          "image/png",
	"/pwa/icon-512.png":          "image/png",
	"/pwa/icon-maskable-512.png": "image/png",
}

func (s *Server) servePublic(w http.ResponseWriter, r *http.Request) bool {
	ctype, ok := publicFiles[r.URL.Path]
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.staticFile(strings.TrimPrefix(r.URL.Path, "/"), ctype, "public, max-age=86400")(w, r)
	return true
}

// staticFile serves one file from the built UI.
func (s *Server) staticFile(name, ctype, cache string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.ui, name)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", cache)
		_, _ = w.Write(b)
	}
}

// setWeb turns the web UI through the tunnel on (with who may look) or off.
func (s *Server) setWeb(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	var in struct {
		Enable        bool   `json:"enable"`
		AddService    string `json:"add_service"` // change only the services of the current config
		RemoveService string `json:"remove_service"`
		WebConfig
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	c := loadRemote()
	if !in.Enable {
		c.Web = nil
		s.mu.Lock()
		s.web = nil
		s.mu.Unlock()
		_ = saveRemote(c)
		audit.Log("web.disabled", nil)
		writeJSON(w, s.remoteView())
		return
	}
	if !c.Enabled || c.PublicURL == "" {
		writeErr(w, http.StatusBadRequest, "the web UI needs a tunnel: pulse remote enable --tunnel https://<host> first")
		return
	}
	if in.AddService != "" || in.RemoveService != "" {
		if c.Web == nil {
			writeErr(w, http.StatusBadRequest, "turn the web UI on first (pulse remote web --team … --aud … --email …)")
			return
		}
		next := *c.Web
		next.Services = slices.DeleteFunc(slices.Clone(next.Services), func(s string) bool {
			return strings.EqualFold(s, in.RemoveService) || strings.EqualFold(s, in.AddService)
		})
		if in.AddService != "" {
			next.Services = append(next.Services, in.AddService)
		}
		in.WebConfig = next
	} else if in.Services == nil && c.Web != nil {
		// Reconfiguring who may look keeps the computers following this hub.
		in.Services = c.Web.Services
	}
	cfg, err := validWeb(in.WebConfig)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c.Web = &cfg
	if err := saveRemote(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.web = newWebAccess(cfg)
	s.mu.Unlock()
	audit.Log("web.enabled", map[string]any{"team": cfg.Team, "emails": cfg.Emails, "operators": cfg.Operators, "services": cfg.Services})
	writeJSON(w, s.remoteView())
}
