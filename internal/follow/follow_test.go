package follow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pulse/internal/model"
)

func TestFollowerStreamsRunsMarkedWithTheHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != "id.access" || r.Header.Get("CF-Access-Client-Secret") != "sec" {
			http.Redirect(w, r, "https://team.cloudflareaccess.com/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\nevent: runs\ndata: {\"runs\":[{\"id\":\"r_1\",\"job\":\"vm\",\"status\":\"running\"}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	got := make(chan []model.Run, 4)
	f := &Follower{Config: Config{URL: srv.URL + "/", ClientID: "id.access"}, Secret: "sec", OnRuns: func(r []model.Run) { got <- r }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)
	select {
	case runs := <-got:
		if len(runs) != 1 || runs[0].Job != "vm" || runs[0].Hub != srv.URL {
			t.Fatalf("runs = %+v", runs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no runs")
	}
}

func TestRefusedFollowerShowsNothing(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Redirect(w, r, "https://team.cloudflareaccess.com/login", http.StatusFound)
	}))
	defer srv.Close()
	shown := false
	f := &Follower{Config: Config{URL: srv.URL, ClientID: "x.access"}, Secret: "bad", OnRuns: func(r []model.Run) { shown = r != nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	f.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if shown || calls < 2 || calls > 3 {
		t.Fatalf("shown=%v calls=%d (want retries with backoff, nothing shown)", shown, calls)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("PULSE_HOME", t.TempDir())
	if _, ok := Load(); ok {
		t.Fatal("following before setup")
	}
	c := Config{URL: "https://hub.example.com", ClientID: "id.access", CFTokenID: "st1"}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	if got, ok := Load(); !ok || got != c {
		t.Fatalf("got %+v %v", got, ok)
	}
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(); ok {
		t.Fatal("still following after Remove")
	}
}

// A hub with nothing changing sends only keep-alives: that's alive, not
// stale. Its runs must not vanish when the connection then drops.
func TestQuietHubStaysShown(t *testing.T) {
	old := StaleAfter
	StaleAfter = 300 * time.Millisecond
	t.Cleanup(func() { StaleAfter = old })
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		first := conns == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if first {
			fmt.Fprint(w, "event: runs\ndata: {\"runs\":[{\"id\":\"r_1\",\"job\":\"vm\",\"status\":\"running\"}]}\n\n")
			w.(http.Flusher).Flush()
			for i := 0; i < 5; i++ { // quiet but alive, past StaleAfter
				time.Sleep(100 * time.Millisecond)
				fmt.Fprint(w, ": keep-alive\n\n")
				w.(http.Flusher).Flush()
			}
			return // then the connection drops
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	var cleared bool
	var cmu sync.Mutex
	f := &Follower{Config: Config{URL: srv.URL, ClientID: "x.access"}, Secret: "s", OnRuns: func(r []model.Run) {
		cmu.Lock()
		if r == nil {
			cleared = true
		}
		cmu.Unlock()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	f.Run(ctx)
	cmu.Lock()
	defer cmu.Unlock()
	if cleared {
		t.Fatal("a quiet but connected hub's runs were hidden on a brief reconnect")
	}
}
