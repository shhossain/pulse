package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pulse/internal/audit"
	"pulse/internal/cfaccess"
)

// WebConfig lets people open the UI through the tunnel: Cloudflare Access
// signs them in, and Pulse checks Access's token itself before showing
// anything. Viewers can only look (no stop, answers, settings).
type WebConfig struct {
	Team   string   `json:"team"`   // Access team domain, e.g. myteam.cloudflareaccess.com
	AUD    string   `json:"aud"`    // the Access application's audience tag
	Emails []string `json:"emails"` // who may look (lowercase)
}

type webAccess struct {
	cfg    WebConfig
	v      *cfaccess.Verifier
	emails map[string]bool
	seenMu sync.Mutex
	seen   map[string]time.Time // email → last audit entry
}

var (
	teamRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)
	audRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
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
	var emails []string
	seen := map[string]bool{}
	for _, e := range c.Emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e || a.Name != "" {
			return c, fmt.Errorf("%q isn't an email address", e)
		}
		if !seen[e] {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	if len(emails) == 0 {
		return c, fmt.Errorf("name at least one email that may open the UI")
	}
	sort.Strings(emails)
	c.Emails = emails
	return c, nil
}

func newWebAccess(c WebConfig) *webAccess {
	w := &webAccess{cfg: c, v: &cfaccess.Verifier{Team: c.Team, AUD: c.AUD}, emails: map[string]bool{}, seen: map[string]time.Time{}}
	for _, e := range c.Emails {
		w.emails[e] = true
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
	if err != nil || id.Email == "" || !wa.emails[id.Email] {
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
		writeJSON(w, map[string]any{"remote": true, "read_only": true, "email": viewerEmail(r)})
	})
	// The one thing a viewer may change: their own notifications.
	api("/web/push", s.pushStatus)
	writes := map[string]http.HandlerFunc{
		"/api/v1/web/push":             s.pushSubscribe,
		"/api/v1/web/push/unsubscribe": s.pushUnsubscribe,
		"/api/v1/web/push/test":        s.pushTest,
	}
	for path, h := range writes {
		mux.HandleFunc("POST "+path, h)
	}
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
			if r.Method != http.MethodPost || writes[r.URL.Path] == nil || r.Header.Get("Origin") != origin {
				writeErr(w, http.StatusForbidden, "the web UI is read-only")
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
		Enable bool `json:"enable"`
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
	audit.Log("web.enabled", map[string]any{"team": cfg.Team, "emails": cfg.Emails})
	writeJSON(w, s.remoteView())
}
