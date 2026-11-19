package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pulse/internal/audit"
	"pulse/internal/paths"
)

// pushRoutes are the only routes a job (local or remote) needs: report
// progress, receive commands. They're served on both listeners.
func (s *Server) pushRoutes(api func(string, http.HandlerFunc)) {
	api("GET /health", s.health)
	api("POST /push/{job...}", s.require(scopePush, s.push))
	api("POST /runs/{id}/events", s.require(scopePush, s.runEvents))
	api("GET /runs/{id}/commands", s.require(scopePush, s.waitCommands))
	api("POST /runs/{id}/commands/{cid}/ack", s.require(scopePush, s.ackCommand))
	api("POST /runs/{id}/asks", s.require(scopePush, s.createAsk))
	api("GET /asks/{id}", s.require(scopePush, s.waitAsk))
}

// RemoteHandler serves other machines: push routes only, push-scope tokens
// only (the admin token is refused even if it leaks), and no browsers.
func (s *Server) RemoteHandler() http.Handler { return s.remoteHandler("") }

// remoteHandler is RemoteHandler; in tunnel mode (publicURL set: reachable
// from the internet) only job tokens work: this machine's own push token,
// which may report any job without limits, stays on private networks. With
// the web UI on, a person Cloudflare Access signed in (and Pulse verified)
// gets the read-only UI instead.
func (s *Server) remoteHandler(publicURL string) http.Handler {
	tunnel := publicURL != ""
	viewer := s.viewerHandler(publicURL)
	mux := http.NewServeMux()
	s.pushRoutes(func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /api/v1"+path, h)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A job machine sends its job token; a person's browser never does.
		// (Someone's own Access login on a job machine must not turn its
		// pushes into read-only UI requests.)
		if wa := s.webAccess(); tunnel && wa != nil && s.servePublic(w, r) {
			return
		}
		if wa := s.webAccess(); tunnel && wa != nil && r.Header.Get("Authorization") == "" {
			if email, ok := wa.viewer(r); ok {
				ctx, cancel := context.WithCancel(context.WithValue(r.Context(), viewerKey, email))
				defer cancel()
				go s.revokeWhenAccessEnds(ctx, cancel, wa, r)
				viewer.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if r.Header.Get("Origin") != "" || hasForeignCookie(r) {
			writeErr(w, http.StatusForbidden, "browsers can't use the remote endpoint")
			return
		}
		sc, job := s.scopeOf(r)
		switch {
		case sc == scopeAdmin:
			writeErr(w, http.StatusForbidden, "the admin token only works on this Mac; use a push token (pulse remote token <job>)")
			return
		case tunnel && sc == scopePush && job == "":
			writeErr(w, http.StatusForbidden, "only job tokens work through the tunnel (pulse remote token <job>)")
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" { // in JSON, like every Pulse error
			writeErr(w, http.StatusNotFound, "not available remotely: other machines can only report jobs")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// accessCookie is the cookie Cloudflare Access adds to every request it lets
// through to the origin (service tokens included). It isn't a sign of a browser.
const accessCookie = "CF_Authorization"

// hasForeignCookie reports whether r carries cookies other than Access's:
// command-line clients send none, so any other cookie means a browser. It
// reads the raw header: r.Cookies() silently skips cookies it can't parse,
// and an unparseable cookie is still a cookie.
func hasForeignCookie(r *http.Request) bool {
	for _, line := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			name, _, _ := strings.Cut(part, "=")
			if strings.TrimSpace(name) != accessCookie {
				return true
			}
		}
	}
	return false
}

// RemoteConfig is persisted in remote.json.
type RemoteConfig struct {
	Enabled bool   `json:"enabled"`
	Addr    string `json:"addr"` // ip:port
	// PublicURL is set in tunnel mode: the listener binds to loopback only
	// and a tunnel (e.g. cloudflared) forwards this https URL to it.
	PublicURL string `json:"public_url,omitempty"`
	// Web opens the read-only UI through the tunnel to signed-in viewers.
	Web *WebConfig `json:"web,omitempty"`
}

func remoteFile() string { return filepath.Join(paths.Home(), "remote.json") }

func loadRemote() RemoteConfig {
	var c RemoteConfig
	if b, err := os.ReadFile(remoteFile()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveRemote(c RemoteConfig) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(remoteFile(), b, 0o600)
}

var allowedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

var tailnet = allowedNets[0]

// DefaultTunnelAddr is where the push listener binds in tunnel mode unless
// told otherwise: loopback, on a fixed port the tunnel config can name.
const DefaultTunnelAddr = "127.0.0.1:7778"

// tunnelURL validates a tunnel's public URL: https, a host, nothing else
// (no credentials, path, query or fragment). "" means no tunnel.
func tunnelURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	bad := fmt.Errorf("tunnel URL must look like https://pulse.example.com, got %q", raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		!dnsName(u.Hostname()) {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", bad
		}
		if n != 443 { // the default: browsers leave it out of Origin
			host = net.JoinHostPort(host, p)
		}
	}
	return "https://" + host, nil
}

// dnsName reports whether h is a DNS host name a public certificate can
// cover: dot-separated labels of letters, digits and inner hyphens, and not
// an IP address.
func dnsName(h string) bool {
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// PrivateIP reports whether ip is on a private or Tailscale network — the
// only places the remote listener may bind (never 0.0.0.0 or a public IP).
func PrivateIP(ip net.IP) bool {
	for _, n := range allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Candidate is an address the remote listener could use.
type Candidate struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
	Tailscale bool   `json:"tailscale"`
}

// Candidates lists this Mac's private and Tailscale addresses, Tailscale first.
func Candidates() []Candidate {
	var ts, other []Candidate
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !PrivateIP(ipn.IP) {
				continue
			}
			c := Candidate{IP: ipn.IP.String(), Interface: i.Name, Tailscale: tailnet.Contains(ipn.IP)}
			if c.Tailscale {
				ts = append(ts, c)
			} else {
				other = append(other, c)
			}
		}
	}
	return append(ts, other...)
}

type remoteState struct {
	srv       *http.Server
	addr      string
	publicURL string // tunnel mode
	err       string
}

// StartRemote applies the saved remote config (called at startup).
func (s *Server) StartRemote() {
	if c := loadRemote(); c.Enabled {
		if c.PublicURL != "" { // saved by an older version: normalize as it would be now
			u, err := tunnelURL(c.PublicURL)
			if err != nil {
				log.Printf("remote: saved tunnel URL: %v", err)
				return
			}
			c.PublicURL = u
		}
		if c.Web != nil && c.PublicURL != "" {
			if cfg, err := validWeb(*c.Web); err == nil {
				s.mu.Lock()
				s.web = newWebAccess(cfg)
				s.mu.Unlock()
			}
		}
		if err := s.listenRemote(c.Addr, c.PublicURL); err != nil {
			log.Printf("remote: %v", err)
		}
	}
}

// listenRemote serves the push routes on addr. Without publicURL, addr must
// be a Tailscale or private-network address; with one (tunnel mode), it must
// be loopback, so only the local tunnel daemon can reach it.
func (s *Server) listenRemote(addr, publicURL string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be ip:port: %w", err)
	}
	if host == "localhost" { // resolve it here rather than trust the resolver
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	switch {
	case publicURL != "" && (ip == nil || !ip.IsLoopback()):
		return fmt.Errorf("with a tunnel, the listener must be on a loopback address (e.g. 127.0.0.1:7778), not %s", host)
	case publicURL == "" && (ip == nil || !PrivateIP(ip)):
		return fmt.Errorf("%s isn't a Tailscale or private-network address; Pulse never listens publicly", host)
	}
	s.stopRemote()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		s.mu.Lock()
		s.remote = &remoteState{addr: addr, publicURL: publicURL, err: err.Error()}
		s.mu.Unlock()
		return err
	}
	srv := &http.Server{Handler: s.remoteHandler(publicURL), ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes: 64 << 10}
	s.mu.Lock()
	s.remote = &remoteState{srv: srv, addr: ln.Addr().String(), publicURL: publicURL}
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("remote listener: %v", err)
		}
	}()
	log.Printf("remote push listener on http://%s", ln.Addr())
	return nil
}

func (s *Server) stopRemote() {
	s.mu.Lock()
	r := s.remote
	s.remote = nil
	s.mu.Unlock()
	if r != nil && r.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.srv.Shutdown(ctx); err != nil {
			_ = r.srv.Close() // streams never finish on their own
		}
	}
}

type remoteView struct {
	Enabled    bool        `json:"enabled"`
	URL        string      `json:"url,omitempty"`    // what other machines use (PULSE_URL)
	Listen     string      `json:"listen,omitempty"` // where the listener is bound
	Tunnel     bool        `json:"tunnel,omitempty"`
	Web        *WebConfig  `json:"web,omitempty"` // the read-only web UI, when on
	Error      string      `json:"error,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

func (s *Server) remoteView() remoteView {
	v := remoteView{Candidates: Candidates()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remote != nil {
		v.Enabled = s.remote.srv != nil
		v.Error = s.remote.err
		if s.web != nil {
			cfg := s.web.cfg
			v.Web = &cfg
		}
		if v.Enabled {
			v.Listen = s.remote.addr
			v.URL = "http://" + s.remote.addr
			if s.remote.publicURL != "" {
				v.URL, v.Tunnel = s.remote.publicURL, true
			}
		}
	}
	if v.Candidates == nil {
		v.Candidates = []Candidate{}
	}
	return v
}

func (s *Server) remoteStatus(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.remoteView()) }

// setRemote turns the remote listener on (on addr, default: the Tailscale
// address and this app's port) or off.
func (s *Server) setRemote(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	var in struct {
		Enable    bool   `json:"enable"`
		Addr      string `json:"addr"`
		PublicURL string `json:"public_url"` // tunnel mode
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !in.Enable {
		s.stopRemote()
		s.mu.Lock()
		s.web = nil
		s.mu.Unlock()
		_ = saveRemote(RemoteConfig{})
		audit.Log("remote.disabled", nil)
		writeJSON(w, s.remoteView())
		return
	}
	publicURL, err := tunnelURL(in.PublicURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	addr := in.Addr
	if addr == "" && publicURL != "" {
		addr = DefaultTunnelAddr
	} else if addr == "" {
		cs := Candidates()
		if len(cs) == 0 || !cs[0].Tailscale {
			writeErr(w, http.StatusBadRequest, "no Tailscale address found; start Tailscale, or pass --addr <private-ip>")
			return
		}
		addr = net.JoinHostPort(cs[0].IP, fmt.Sprint(s.port))
	} else if ip := net.ParseIP(strings.Trim(addr, "[]")); ip != nil { // a bare address (IPv4 or IPv6)
		addr = net.JoinHostPort(ip.String(), fmt.Sprint(s.port))
	} else if h, p, err := net.SplitHostPort(addr); err == nil && p == "" {
		addr = net.JoinHostPort(h, fmt.Sprint(s.port))
	}
	saved := RemoteConfig{Enabled: true, Addr: addr, PublicURL: publicURL}
	if prev := loadRemote(); prev.Web != nil && prev.PublicURL == publicURL && publicURL != "" {
		saved.Web = prev.Web // same tunnel: keep who may look
	} else {
		s.mu.Lock()
		s.web = nil // before the new listener starts: no viewer on another origin
		s.mu.Unlock()
	}
	if err := s.listenRemote(addr, publicURL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveRemote(saved); err != nil {
		writeErr(w, http.StatusInternalServerError, "remote access is on but couldn't be saved: "+err.Error())
		return
	}
	audit.Log("remote.enabled", map[string]any{"addr": addr, "public_url": publicURL})
	writeJSON(w, s.remoteView())
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tokens": s.eng.Tokens()})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Job  string `json:"job"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Job) == "" {
		writeErr(w, http.StatusBadRequest, "job is required")
		return
	}
	plain, t, err := s.eng.CreateToken(in.Job, in.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"token": plain, "info": t})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.RevokeToken(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
