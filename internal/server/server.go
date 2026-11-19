// Package server is Pulse's local HTTP API and UI server.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"pulse/internal/alert"
	"pulse/internal/audit"
	"pulse/internal/config"
	"pulse/internal/engine"
	"pulse/internal/model"
	"pulse/internal/paths"
	"pulse/internal/watch"
	"pulse/web"
)

const (
	cookieName   = "pulse_session"
	maxBodyBytes = 8 << 20
)

type scope int

const (
	scopeNone scope = iota
	scopePush
	scopeAdmin
)

// Server serves the API and UI on 127.0.0.1.
type Server struct {
	Version string
	// OpenPage shows a UI page (e.g. "/r/<id>"). The app sets it to open a
	// native window; by default the page opens in the browser.
	OpenPage func(path string) error
	// Watches runs polled sources (nil: watching is unavailable).
	Watches *watch.Manager
	// Limits bound what each job token may do (see Limits).
	Limits Limits
	// OnLimit is told when a job token first hits a limit in an hour (the
	// app turns it into a notification: it may mean a compromised machine).
	OnLimit func(job, what string)

	eng        *engine.Engine
	pushTok    string
	adminTok   string
	sessionTok string

	mu      sync.Mutex
	tokMu   sync.RWMutex // guards pushTok/adminTok (rotation)
	oneTime map[string]time.Time
	subs    map[chan struct{}]struct{} // SSE subscribers
	remote  *remoteState

	ui   fs.FS
	ln   net.Listener
	port int
	lim  *limiter
}

func New(eng *engine.Engine, pushToken, adminToken string) *Server {
	ui, _ := fs.Sub(web.Dist, "dist")
	s := &Server{
		Version:    "dev",
		eng:        eng,
		pushTok:    pushToken,
		adminTok:   adminToken,
		sessionTok: paths.RandomHex(32),
		oneTime:    map[string]time.Time{},
		subs:       map[chan struct{}]struct{}{},
		ui:         ui,
		Limits:     DefaultLimits,
		lim:        newLimiter(),
	}
	s.lim.onTrip = func(job, what string) {
		if s.OnLimit != nil {
			s.OnLimit(job, what)
		}
	}
	s.OpenPage = s.openInBrowser
	eng.OnChange(s.broadcast)
	return s
}

func (s *Server) openInBrowser(path string) error {
	u := s.OneTimeURL(path)
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Run()
	case "linux":
		return exec.Command("xdg-open", u).Run()
	}
	return errors.New("no browser opener on this OS")
}

// Listen binds preferred (e.g. 127.0.0.1:7777), falling back to a random port.
func (s *Server) Listen(preferred string) error {
	ln, err := net.Listen("tcp", preferred)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	s.ln = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	return nil
}

// URL is the base URL the server is reachable at.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

// Serve blocks serving requests.
func (s *Server) Serve() error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(s.ln)
}

// OneTimeURL returns a URL that logs a browser/webview in (sets the session
// cookie) and redirects to next. Valid once, for 2 minutes.
func (s *Server) OneTimeURL(next string) string {
	t := paths.RandomHex(16)
	s.mu.Lock()
	s.oneTime[t] = time.Now().Add(2 * time.Minute)
	s.mu.Unlock()
	return fmt.Sprintf("%s/auth?t=%s&next=%s", s.URL(), t, url.QueryEscape(next))
}

// Handler returns the full HTTP handler (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /api/v1"+path, h)
	}
	s.pushRoutes(api)
	api("GET /runs", s.require(scopeAdmin, s.listRuns))
	api("GET /runs/{id}", s.require(scopeAdmin, s.getRun))
	api("GET /runs/{id}/series", s.require(scopeAdmin, s.getSeries))
	api("GET /runs/{id}/log", s.require(scopeAdmin, s.getLog))
	api("POST /runs/{id}/stop", s.require(scopeAdmin, s.stopRun))
	api("POST /runs/{id}/seen", s.require(scopeAdmin, s.runAction(s.eng.MarkSeen)))
	api("POST /runs/{id}/archive", s.require(scopeAdmin, s.runAction(s.eng.Archive)))
	api("POST /runs/{id}/actions/{name}", s.require(scopeAdmin, s.runJobAction))
	api("GET /alerts", s.require(scopeAdmin, s.listAlerts))
	api("POST /alerts", s.require(scopeAdmin, s.addAlert))
	api("PATCH /alerts/{id}", s.require(scopeAdmin, s.patchAlert))
	api("DELETE /alerts/{id}", s.require(scopeAdmin, s.deleteAlert))
	api("POST /mute", s.require(scopeAdmin, s.mute))
	api("PUT /jobs/config", s.require(scopeAdmin, s.applyConfig))
	api("DELETE /jobs", s.require(scopeAdmin, s.deleteJob))
	api("GET /jobs", s.require(scopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		names := s.eng.JobNames()
		if names == nil {
			names = []string{}
		}
		writeJSON(w, map[string]any{"jobs": names})
	}))
	api("GET /actions", s.require(scopeAdmin, s.listActions))
	api("POST /actions", s.require(scopeAdmin, s.addAction))
	api("DELETE /actions/{name}", s.require(scopeAdmin, s.deleteAction))
	api("GET /stream", s.require(scopeAdmin, s.stream))
	api("POST /login-link", s.require(scopeAdmin, s.loginLink))
	api("POST /ui/open", s.require(scopeAdmin, s.uiOpen))
	api("POST /tokens/rotate", s.require(scopeAdmin, s.rotateToken))
	api("GET /tokens", s.require(scopeAdmin, s.listTokens))
	api("POST /tokens", s.require(scopeAdmin, s.createToken))
	api("DELETE /tokens/{id}", s.require(scopeAdmin, s.revokeToken))
	api("PUT /labels", s.require(scopeAdmin, s.setLabels))
	api("GET /asks", s.require(scopeAdmin, s.listAsks))
	api("POST /asks/{id}/answer", s.require(scopeAdmin, s.answerAsk))
	api("GET /watches", s.require(scopeAdmin, s.listWatches))
	api("PUT /watches", s.require(scopeAdmin, s.setWatch))
	api("DELETE /watches", s.require(scopeAdmin, s.removeWatch))
	api("GET /remote", s.require(scopeAdmin, s.remoteStatus))
	api("POST /remote", s.require(scopeAdmin, s.setRemote))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/popover", http.StatusFound)
	})
	mux.HandleFunc("GET /auth", s.auth)
	mux.HandleFunc("GET /popover", s.requireUI(s.page("popover.html")))
	mux.HandleFunc("GET /r/{id}", s.requireUI(s.page("run.html")))
	mux.HandleFunc("GET /j/{job...}", s.requireUI(s.jobPage))
	mux.Handle("GET /assets/", s.assets())
	return s.guard(mux)
}

// guard rejects requests that don't look like they come from this machine's
// own clients: wrong Host (DNS rebinding) or a foreign Origin (cross-site).
// guard rejects requests that don't look like they come from this machine's
// own clients: a non-loopback Host (DNS rebinding) or a foreign Origin
// (cross-site). Any loopback port is fine for Host (SSH tunnels forward
// from another port); Origin must be this server exactly.
func (s *Server) guard(next http.Handler) http.Handler {
	self := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", s.port): true,
		fmt.Sprintf("localhost:%d", s.port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.port != 0 && !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Scheme != "http" || (s.port != 0 && !self[u.Host]) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	switch strings.Trim(h, "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// scopeOf returns what a request may do, and for job tokens the one job it
// may push to.
func (s *Server) scopeOf(r *http.Request) (scope, string) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimPrefix(h, "Bearer ")
		s.tokMu.RLock()
		admin, push := s.adminTok, s.pushTok
		s.tokMu.RUnlock()
		switch {
		case eq(tok, admin):
			return scopeAdmin, ""
		case eq(tok, push):
			return scopePush, ""
		}
		if job, ok := s.eng.LookupToken(tok); ok {
			return scopePush, job
		}
		return scopeNone, ""
	}
	if c, err := r.Cookie(cookieName); err == nil && eq(c.Value, s.sessionTok) {
		return scopeAdmin, ""
	}
	return scopeNone, ""
}

type ctxKey int

const (
	jobLimitKey ctxKey = iota + 1
	admissionKey
)

// jobLimit is the only job a job-scoped token may touch ("" = any).
func jobLimit(r *http.Request) string {
	j, _ := r.Context().Value(jobLimitKey).(string)
	return j
}

// allowJob reports whether the request may act on job, writing a 403 if not.
func allowJob(w http.ResponseWriter, r *http.Request, job string) bool {
	if lim := jobLimit(r); lim != "" && lim != job {
		writeErr(w, http.StatusForbidden, fmt.Sprintf("this token can only report job %q", lim))
		return false
	}
	return true
}

func eq(a, b string) bool {
	return b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) require(min scope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, job := s.scopeOf(r)
		if got == scopeNone {
			writeErr(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		if got < min {
			writeErr(w, http.StatusForbidden, "token does not allow this")
			return
		}
		if job == "" {
			h(w, r)
			return
		}
		// A job token: within its job's limits only. Everything it sends
		// counts, the URL included (push reads values from the query).
		control := r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/ack")
		reserve := int64(len(r.URL.RawQuery)) + max(r.ContentLength, 0)
		adm, e := s.lim.admit(job, s.Limits, control, reserve)
		if e != nil {
			refuse(w, e)
			return
		}
		body := &countingBody{ReadCloser: r.Body}
		r.Body = body
		ctx := context.WithValue(r.Context(), jobLimitKey, job)
		ctx = context.WithValue(ctx, admissionKey, adm)
		h(w, r.WithContext(ctx))
		s.lim.addBytes(job, body.n-max(r.ContentLength, 0)) // a body without a declared length
	}
}

// admissionOf is what the job-token limits allow this request ("" for others).
func admissionOf(r *http.Request) admission {
	a, _ := r.Context().Value(admissionKey).(admission)
	return a
}

func (s *Server) requireUI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sc, _ := s.scopeOf(r); sc != scopeAdmin {
			http.Error(w, "Open Pulse from the menubar to sign in.", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// page serves a built HTML page from web/dist.
func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.ui, name)
		if err != nil {
			http.Error(w, "UI not built: run `npm run build` in web/ and rebuild Pulse.", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

// assets serves hashed JS/CSS bundles; their names change on every build.
// Only files directly inside assets/ are served: no directory listings and
// no way out of the folder (pages need a login; bundles don't).
func (s *Server) assets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(s.ui, "assets/"+name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	})
}

// loginLink returns a one-time URL that signs a browser in (used by `pulse open`).
func (s *Server) loginLink(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/popover"
	}
	writeJSON(w, map[string]string{"url": s.OneTimeURL(next)})
}

func (s *Server) runAction(f func(id string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f(r.PathValue("id")) {
			writeErr(w, http.StatusNotFound, "no such run")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// broadcast wakes every SSE subscriber (non-blocking).
func (s *Server) broadcast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// stream sends the run snapshot whenever it changes (at most 4×/s), plus a
// keep-alive comment every 15s.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	send := func() error {
		b, err := json.Marshal(map[string]any{"runs": s.eng.Snapshot()})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: runs\ndata: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := send(); err != nil {
		return
	}
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if err := send(); err != nil {
				return
			}
			select { // throttle bursts of changes
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("t")
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/popover"
	}
	s.mu.Lock()
	exp, ok := s.oneTime[t]
	delete(s.oneTime, t)
	s.mu.Unlock()
	if !ok || time.Now().After(exp) {
		http.Error(w, "link expired", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sessionTok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "app": "pulse", "version": s.Version, "api": 1})
}

func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	var b model.Batch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if lim := jobLimit(r); lim != "" {
		if b.Job == "" {
			b.Job = lim
		}
		// A job token names its job exactly: never rename it to job#2
		// (the token would then refuse its own run).
		b.Meta.Inferred = false
		if !allowJob(w, r, engine.CleanJobName(b.Job)) {
			return
		}
		existing := s.eng.RunJob(r.PathValue("id"))
		if existing != "" && !allowJob(w, r, existing) {
			return
		}
		if admissionOf(r).degraded && len(b.Log) > 0 {
			b.Log = nil // over the daily data quota: status and metrics only
		}
		if existing == "" && engine.ValidRunID(r.PathValue("id")) {
			if e := s.lim.newRun(lim, s.Limits); e != nil {
				refuse(w, e)
				return
			}
		}
	}
	resp, err := s.eng.ApplyBatch(r.PathValue("id"), b)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, resp)
}

// push accepts a JSON object, a form, or query parameters.
func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("job")
	if strings.TrimSpace(job) == "" {
		writeErr(w, http.StatusBadRequest, "job name required")
		return
	}
	if !allowJob(w, r, engine.CleanJobName(job)) {
		return
	}
	raw := map[string]any{}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A JSON object is read as JSON whatever the Content-Type says
	// (curl -d '{…}' sends it as a form); otherwise it's a form.
	switch trimmed := strings.TrimSpace(string(b)); {
	case strings.HasPrefix(trimmed, "{"):
		if err := json.Unmarshal(b, &raw); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON object: "+err.Error())
			return
		}
	case trimmed != "":
		form, err := url.ParseQuery(trimmed)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "body must be a JSON object or key=value form: "+err.Error())
			return
		}
		for k, v := range form {
			raw[k] = v[len(v)-1]
		}
	}
	forceNew := false
	for k, v := range r.URL.Query() {
		if k == "new" {
			forceNew = v[0] == "1" || v[0] == "true"
			continue
		}
		raw[k] = v[len(v)-1]
	}
	if lim := jobLimit(r); lim != "" && s.eng.PushStartsRun(job, raw, forceNew) {
		if e := s.lim.newRun(lim, s.Limits); e != nil {
			refuse(w, e)
			return
		}
	}
	resp, err := s.eng.Push(job, raw, forceNew)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, resp)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	if job := r.URL.Query().Get("job"); job != "" {
		total := s.eng.CountRuns(job)
		if seq, err := strconv.Atoi(r.URL.Query().Get("seq")); err == nil && seq > 0 {
			runs := []model.Run{}
			if run, ok := s.eng.JobRun(job, seq); ok {
				runs = append(runs, run)
			}
			writeJSON(w, map[string]any{"runs": runs, "total": total, "now": time.Now()})
			return
		}
		runs, err := s.eng.JobRuns(job, 50)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"runs": runs, "total": total, "now": time.Now()})
		return
	}
	all := r.URL.Query().Get("all") == "1"
	writeJSON(w, map[string]any{"runs": s.eng.Snapshot(all), "now": time.Now()})
}

func (s *Server) getSeries(w http.ResponseWriter, r *http.Request) {
	if !engine.ValidRunID(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "no such run")
		return
	}
	keys := r.URL.Query()["key"] // repeated: metric names may contain commas
	points, _ := strconv.Atoi(r.URL.Query().Get("points"))
	if points <= 0 || points > 5000 {
		points = 1000
	}
	var set model.SeriesSet
	var err error
	if x := r.URL.Query().Get("x"); x != "" {
		if len(keys) == 0 || len(keys) > 8 {
			writeErr(w, http.StatusBadRequest, "a scatter needs 1–8 key= metrics")
			return
		}
		set, err = s.eng.Scatter(r.PathValue("id"), x, keys, points)
	} else {
		set, err = s.eng.Series(r.PathValue("id"), keys, points)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, set)
}

func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	if !engine.ValidRunID(r.PathValue("id")) { // the id becomes a file name
		writeErr(w, http.StatusNotFound, "no such run")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n <= 0 || n > 10000 {
		n = 500
	}
	var lines []string
	if s.eng.Logs != nil {
		var err error
		if lines, err = s.eng.Logs.Tail(r.PathValue("id"), n); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, map[string]any{"lines": lines})
}

// uiOpen opens a page (popover, run page) for the user; used by `pulse open`
// and by the popover's "Open" buttons.
func (s *Server) uiOpen(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		writeErr(w, http.StatusBadRequest, "path must start with /")
		return
	}
	// Don't open a window onto a 404.
	if job, ok := strings.CutPrefix(path, "/j/"); ok {
		if _, found := s.eng.LatestRun(job); !found {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("no job named %q (see pulse ls)", job))
			return
		}
	}
	if id, ok := strings.CutPrefix(path, "/r/"); ok {
		if _, found := s.eng.Run(id); !found {
			writeErr(w, http.StatusNotFound, "no such run")
			return
		}
	}
	if err := s.OpenPage(path); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.eng.Stop(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]string{"result": res})
}

// waitCommands long-polls for commands for a run (stop, …).
func (s *Server) waitCommands(w http.ResponseWriter, r *http.Request) {
	if jobLimit(r) != "" && !allowJob(w, r, s.eng.RunJob(r.PathValue("id"))) {
		return
	}
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), 55*time.Second)
	var accepts []string
	if a := r.URL.Query().Get("accepts"); a != "" {
		accepts = strings.Split(a, ",")
	}
	cmds := s.eng.WaitCommands(r.Context(), r.PathValue("id"), wait, accepts...)
	writeJSON(w, map[string]any{"commands": cmds})
}

func (s *Server) ackCommand(w http.ResponseWriter, r *http.Request) {
	if jobLimit(r) != "" && !allowJob(w, r, s.eng.RunJob(r.PathValue("id"))) {
		return
	}
	s.eng.AckCommand(r.PathValue("id"), r.PathValue("cid"))
	writeJSON(w, map[string]bool{"ok": true})
}

func jobParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	job := r.URL.Query().Get("job")
	if strings.TrimSpace(job) == "" {
		writeErr(w, http.StatusBadRequest, "job is required")
		return "", false
	}
	return engine.CleanJobName(job), true
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	if job, ok := jobParam(w, r); ok {
		writeJSON(w, s.eng.Alerts(job))
	}
}

func (s *Server) addAlert(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	var in struct {
		Expr     string   `json:"expr"`
		Level    string   `json:"level"`
		Action   string   `json:"action"`
		Cooldown string   `json:"cooldown"`
		Once     bool     `json:"once"`
		Ask      string   `json:"ask"`
		Answers  []string `json:"answers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	rule, err := alert.Parse(in.Expr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Level != "" {
		rule.Level = in.Level
	}
	rule.Action, rule.Once, rule.Ask = in.Action, in.Once, strings.TrimSpace(in.Ask)
	for _, a := range in.Answers {
		ans, err := alert.ParseAnswer(a)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rule.Answers = append(rule.Answers, ans)
	}
	if rule.Ask != "" && len(rule.Answers) == 0 {
		rule.Answers = alert.DefaultAnswers()
	}
	if in.Cooldown != "" {
		d, err := alert.ParseCooldown(in.Cooldown)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rule.CooldownS = d.Seconds()
	}
	rule, err = s.eng.AddAlert(job, rule)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, rule)
}

func (s *Server) patchAlert(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.eng.SetAlertEnabled(job, r.PathValue("id"), in.Enabled); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteAlert(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	if err := s.eng.RemoveAlert(job, r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// mute silences a job: ?job=X&for=1h (for=0 or off unmutes).
func (s *Server) mute(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	var d time.Duration
	if f := r.URL.Query().Get("for"); f != "" && f != "0" && f != "off" {
		var err error
		if d, err = alert.ParseDuration(f); err != nil {
			writeErr(w, http.StatusBadRequest, "bad duration (use e.g. 1h, 30m, 1d)")
			return
		}
	}
	if err := s.eng.Mute(job, d); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, s.eng.Alerts(job))
}

// applyConfig loads a pulse.yaml job (admin only: shell actions in it will
// run on this Mac, so trust can only be granted locally).
func (s *Server) applyConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Job    config.Job `json:"job"`
		Source string     `json:"source"`
		Trust  bool       `json:"trust"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.eng.ApplyConfig(in.Job, in.Source, in.Trust)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, res)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	if s.Watches != nil {
		_ = s.Watches.Remove(job) // a deleted job stops being polled
	}
	n, err := s.eng.DeleteJob(job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int{"runs_deleted": n})
}

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	if job, ok := jobParam(w, r); ok {
		acts := s.eng.Actions(job)
		if acts == nil {
			acts = []config.Action{}
		}
		writeJSON(w, map[string]any{"actions": acts})
	}
}

func (s *Server) addAction(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	var a config.Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.eng.AddAction(job, a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteAction(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	if err := s.eng.RemoveAction(job, r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) runJobAction(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "ui"
	}
	res, err := s.eng.RunAction(r.Context(), r.PathValue("id"), r.PathValue("name"), by)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if res.URL != "" && by != "cli" {
		_ = openURL(res.URL)
	}
	writeJSON(w, res)
}

func openURL(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Run()
	case "linux":
		return exec.Command("xdg-open", u).Run()
	}
	return errors.New("no browser opener on this OS")
}

// jobPage redirects /j/<job> to the job's latest run.
func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.eng.LatestRun(r.PathValue("job")) // memory, else the store
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/r/"+url.PathEscape(id), http.StatusFound)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.eng.Run(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, run)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		return
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) listWatches(w http.ResponseWriter, r *http.Request) {
	if s.Watches == nil {
		writeJSON(w, map[string]any{"watches": []watch.Status{}})
		return
	}
	writeJSON(w, map[string]any{"watches": s.Watches.List()})
}

// setWatch adds or replaces a job's watcher. Admin only: a command watcher
// runs shell commands on this Mac.
func (s *Server) setWatch(w http.ResponseWriter, r *http.Request) {
	if s.Watches == nil {
		writeErr(w, http.StatusServiceUnavailable, "watching isn't available")
		return
	}
	var spec watch.Spec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if spec.Job == "" && spec.Kind == watch.KindURL {
		spec.Job = watch.GitHubJobName(spec.Source)
	}
	if strings.TrimSpace(spec.Job) == "" {
		writeErr(w, http.StatusBadRequest, "job is required")
		return
	}
	spec.Job = engine.CleanJobName(spec.Job)
	if err := s.Watches.Set(spec); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"job": spec.Job})
}

func (s *Server) removeWatch(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	if s.Watches == nil {
		writeErr(w, http.StatusNotFound, "no watcher")
		return
	}
	if err := s.Watches.Remove(job); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// createAsk opens a question on a run (the job then waits with waitAsk).
func (s *Server) createAsk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !allowJob(w, r, s.eng.RunJob(id)) {
		return
	}
	if lim := jobLimit(r); lim != "" {
		if e := s.lim.ask(lim, s.Limits); e != nil {
			refuse(w, e)
			return
		}
	}
	var in struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
		Default  string   `json:"default"`
		TimeoutS float64  `json:"timeout_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	a, err := s.eng.Ask(id, in.Question, in.Options, in.Default, time.Duration(in.TimeoutS*float64(time.Second)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, a)
}

// waitAsk long-polls a question until it's answered (?wait=30s).
func (s *Server) waitAsk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.eng.GetAsk(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such question")
		return
	}
	if !allowJob(w, r, a.Job) {
		return
	}
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), 55*time.Second)
	a, _ = s.eng.WaitAsk(r.Context(), id, wait)
	writeJSON(w, a)
}

func (s *Server) listAsks(w http.ResponseWriter, r *http.Request) {
	asks := s.eng.OpenAsks()
	if asks == nil {
		asks = []model.Ask{}
	}
	writeJSON(w, map[string]any{"asks": asks})
}

func (s *Server) answerAsk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answer string `json:"answer"`
		By     string `json:"by"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.By == "" {
		in.By = "ui"
	}
	a, err := s.eng.Answer(r.PathValue("id"), in.Answer, in.By)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, a)
}

// rotateToken replaces the push or admin token. The old one stops working at
// once; jobs started with it must be restarted (or re-read the file).
func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Which string `json:"which"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var path string
	switch in.Which {
	case "push":
		path = paths.PushToken()
	case "admin":
		path = paths.AdminToken()
	default:
		writeErr(w, http.StatusBadRequest, `which must be "push" or "admin"`)
		return
	}
	s.tokMu.Lock() // file and memory change together (concurrent rotations)
	tok, err := paths.RotateToken(path)
	if err == nil {
		if in.Which == "push" {
			s.pushTok = tok
		} else {
			s.adminTok = tok
		}
	}
	s.tokMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log("token.rotated", map[string]any{"which": in.Which})
	writeJSON(w, map[string]string{"rotated": in.Which, "file": path})
}

// setLabels sets display names and units for a job's keys (empty removes).
func (s *Server) setLabels(w http.ResponseWriter, r *http.Request) {
	job, ok := jobParam(w, r)
	if !ok {
		return
	}
	var in struct {
		Labels map[string]string `json:"labels"`
		Units  map[string]string `json:"units"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.eng.SetLabels(job, in.Labels, in.Units); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
