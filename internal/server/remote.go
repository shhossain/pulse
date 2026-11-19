package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
func (s *Server) RemoteHandler() http.Handler {
	mux := http.NewServeMux()
	s.pushRoutes(func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /api/v1"+path, h)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Cookie") != "" {
			http.Error(w, "browsers can't use the remote endpoint", http.StatusForbidden)
			return
		}
		if sc, _ := s.scopeOf(r); sc == scopeAdmin {
			writeErr(w, http.StatusForbidden, "the admin token only works on this Mac; use a push token (pulse remote token <job>)")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// RemoteConfig is persisted in remote.json.
type RemoteConfig struct {
	Enabled bool   `json:"enabled"`
	Addr    string `json:"addr"` // ip:port
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
	srv  *http.Server
	addr string
	err  string
}

// StartRemote applies the saved remote config (called at startup).
func (s *Server) StartRemote() {
	if c := loadRemote(); c.Enabled {
		if err := s.listenRemote(c.Addr); err != nil {
			log.Printf("remote: %v", err)
		}
	}
}

func (s *Server) listenRemote(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be ip:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !PrivateIP(ip) {
		return fmt.Errorf("%s isn't a Tailscale or private-network address; Pulse never listens publicly", host)
	}
	s.stopRemote()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		s.mu.Lock()
		s.remote = &remoteState{addr: addr, err: err.Error()}
		s.mu.Unlock()
		return err
	}
	srv := &http.Server{Handler: s.RemoteHandler(), ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.remote = &remoteState{srv: srv, addr: ln.Addr().String()}
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
		_ = r.srv.Shutdown(ctx)
	}
}

type remoteView struct {
	Enabled    bool        `json:"enabled"`
	URL        string      `json:"url,omitempty"`
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
		if v.Enabled {
			v.URL = "http://" + s.remote.addr
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
	var in struct {
		Enable bool   `json:"enable"`
		Addr   string `json:"addr"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !in.Enable {
		s.stopRemote()
		_ = saveRemote(RemoteConfig{})
		audit.Log("remote.disabled", nil)
		writeJSON(w, s.remoteView())
		return
	}
	addr := in.Addr
	if addr == "" {
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
	if err := s.listenRemote(addr); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = saveRemote(RemoteConfig{Enabled: true, Addr: addr})
	audit.Log("remote.enabled", map[string]any{"addr": addr})
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
