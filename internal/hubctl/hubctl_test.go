package hubctl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/follow"
)

const acct = "0123456789abcdef0123456789abcdef"

// fakeCF is Cloudflare's API for service tokens and one service-auth policy.
type fakeCF struct {
	mu        sync.Mutex
	tokens    map[string]bool
	include   []map[string]any
	extra     map[string]any // other policy fields, which must survive edits
	failAllow bool
	n         int
}

func (f *fakeCF) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer cf-api-token" {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "bad token"}}})
			return
		}
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		polPath := "/accounts/" + acct + "/access/apps/app1/policies/pol1"
		switch {
		case r.Method == "POST" && r.URL.Path == "/accounts/"+acct+"/access/service_tokens":
			f.n++
			id := "st" + string(rune('0'+f.n))
			f.tokens[id] = true
			ok(map[string]string{"id": id, "client_id": id + ".access", "client_secret": "cf-secret-" + id + "-0123456789abcdef", "expires_at": "2027-11-20T00:00:00Z"})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/accounts/"+acct+"/access/service_tokens/"):
			delete(f.tokens, filepath.Base(r.URL.Path))
			ok(map[string]any{})
		case r.Method == "GET" && r.URL.Path == polPath:
			p := map[string]any{"id": "pol1", "name": "jobs", "decision": "non_identity", "precedence": 1, "include": f.include}
			for k, v := range f.extra {
				p[k] = v
			}
			ok(p)
		case r.Method == "PUT" && r.URL.Path == polPath:
			if f.failAllow {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "boom"}}})
				return
			}
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			b, _ := json.Marshal(p["include"])
			f.include = nil
			json.Unmarshal(b, &f.include)
			f.extra = map[string]any{}
			for k, v := range p {
				if k != "name" && k != "decision" && k != "precedence" && k != "include" {
					f.extra[k] = v
				}
			}
			ok(map[string]any{})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"success": false})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeRunner is the hub's admin command and gcloud; it records every argv.
type fakeRunner struct {
	mu         sync.Mutex
	argv       [][]string
	stdin      [][]byte
	revoked    []string
	secrets    map[string][]byte
	tokenOut   string // what "remote token" prints (default: JSON)
	revokeGone bool   // the hub no longer has the token
}

func (f *fakeRunner) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, append([]string{name}, args...))
	f.stdin = append(f.stdin, stdin)
	switch name {
	case "ssh-hub":
		cmd := args[len(args)-1]
		switch {
		case strings.Contains(cmd, "'remote' 'token'"):
			if f.tokenOut != "" {
				return []byte(f.tokenOut), nil
			}
			return []byte(`{"id":"t_1234abcd","job":"train","token":"pls_jobsecret","url":"https://pulse.example.com","tunnel":true}`), nil
		case strings.Contains(cmd, "'remote' 'web'"):
			return nil, nil
		case strings.Contains(cmd, "'remote' 'revoke'"):
			id := strings.Trim(strings.Fields(cmd)[3], "'")
			if f.revokeGone {
				return nil, errors.New("ssh-hub: exit status 1: pulse: no token \"" + id + "\"")
			}
			f.revoked = append(f.revoked, id)
			return nil, nil
		}
	case "gcloud":
		if args[0] == "secrets" && (args[1] == "create" || args[1] == "versions") {
			f.secrets[args[2]] = stdin
			if args[1] == "versions" {
				f.secrets[args[3]] = stdin
			}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected command " + name)
}

func setup(t *testing.T) (Config, *Cloudflare, *fakeCF, *fakeRunner) {
	t.Helper()
	t.Setenv("PULSE_HUB_DIR", t.TempDir())
	f := &fakeCF{tokens: map[string]bool{}, include: []map[string]any{{"service_token": map[string]any{"token_id": "existing"}}},
		extra: map[string]any{"session_duration": "24h", "approval_required": false}}
	srv := f.server(t)
	conf := Config{URL: "https://pulse.example.com", Admin: []string{"ssh-hub", "--command"},
		Cloudflare: &CloudflareConfig{Account: acct, App: "app1", Policy: "pol1"}}
	cf := &Cloudflare{Token: "cf-api-token", Conf: *conf.Cloudflare, Base: srv.URL}
	return conf, cf, f, &fakeRunner{secrets: map[string][]byte{}}
}

func secretsIn(argv [][]string) bool {
	for _, a := range argv {
		for _, s := range a {
			if strings.Contains(s, "pls_jobsecret") || strings.Contains(s, "cf-secret-") || strings.Contains(s, "cf-api-token") {
				return true
			}
		}
	}
	return false
}

func TestConnectToEnvFile(t *testing.T) {
	conf, cf, f, r := setup(t)
	env := filepath.Join(t.TempDir(), "gpu1.env")
	m, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: env})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env)
	for _, want := range []string{"PULSE_URL='https://pulse.example.com'", "PULSE_TOKEN='pls_jobsecret'", "PULSE_CF_ACCESS_CLIENT_ID='st1.access'", "PULSE_CF_ACCESS_CLIENT_SECRET='cf-secret-st1-0123456789abcdef'"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("env file lacks %s", want)
		}
	}
	if st, _ := os.Stat(env); st.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v", st.Mode().Perm())
	}
	if len(f.include) != 2 || tokenRule(f.include[1]) != "st1" || tokenRule(f.include[0]) != "existing" {
		t.Errorf("policy include = %v (the existing token must stay)", f.include)
	}
	if m.JobTokenID != "t_1234abcd" || m.CFTokenID != "st1" {
		t.Errorf("machine = %+v", m)
	}
	if secretsIn(r.argv) {
		t.Fatal("a secret appeared in a command line")
	}
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: env + "2"}); err == nil {
		t.Error("connected the same machine twice")
	}

	// Disconnect undoes all of it.
	if _, err := Disconnect(context.Background(), conf, cf, r.run, "gpu1"); err != nil {
		t.Fatal(err)
	}
	if len(r.revoked) != 1 || r.revoked[0] != "t_1234abcd" {
		t.Errorf("revoked %v", r.revoked)
	}
	if f.tokens["st1"] || len(f.include) != 1 {
		t.Errorf("service token left: tokens %v include %v", f.tokens, f.include)
	}
	if _, err := os.Stat(env); err == nil {
		t.Error("the local env file was kept")
	}
	if ms, _ := Machines(); len(ms) != 0 {
		t.Errorf("machines = %v", ms)
	}
}

func TestConnectUndoesOnFailure(t *testing.T) {
	conf, cf, f, r := setup(t)
	f.failAllow = true
	env := filepath.Join(t.TempDir(), "gpu1.env")
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: env}); err == nil {
		t.Fatal("connected although Access refused")
	}
	if len(r.revoked) != 1 {
		t.Errorf("the job token wasn't revoked: %v", r.revoked)
	}
	if len(f.tokens) != 0 {
		t.Errorf("the service token wasn't deleted: %v", f.tokens)
	}
	if _, err := os.Stat(env); err == nil {
		t.Error("env file written")
	}
	if ms, _ := Machines(); len(ms) != 0 {
		t.Error("machine recorded")
	}
}

func TestConnectToGCPSecretViaStdin(t *testing.T) {
	conf, cf, _, r := setup(t)
	m, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1",
		Destination{GCPSecret: "pulse-gpu1", GCPProject: "proj", GrantTo: "vm@proj.iam.gserviceaccount.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.secrets["pulse-gpu1"]), "PULSE_TOKEN='pls_jobsecret'") {
		t.Fatalf("secret data = %q", r.secrets["pulse-gpu1"])
	}
	if secretsIn(r.argv) {
		t.Fatal("a secret appeared in a command line")
	}
	if m.Secret != "gcp:projects/proj/secrets/pulse-gpu1" {
		t.Errorf("secret ref %q", m.Secret)
	}
	granted := false
	for _, a := range r.argv {
		granted = granted || strings.Contains(strings.Join(a, " "), "add-iam-policy-binding pulse-gpu1 --member=serviceAccount:vm@proj.iam.gserviceaccount.com")
	}
	if !granted {
		t.Error("the VM's service account wasn't granted access")
	}
}

func TestNamesCantInjectIntoTheHubShell(t *testing.T) {
	conf, cf, _, r := setup(t)
	for _, bad := range [][2]string{{"x'; rm -rf / #", "m"}, {"train", "m; reboot"}, {"$(id)", "m"}, {"train", "../etc"}} {
		if _, err := Connect(context.Background(), conf, cf, r.run, bad[0], bad[1], Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err == nil {
			t.Errorf("accepted job %q machine %q", bad[0], bad[1])
		}
	}
	if len(r.argv) != 0 {
		t.Fatalf("ran commands for bad names: %v", r.argv)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestNoCloudflareTokenNoConnect(t *testing.T) {
	conf, _, _, r := setup(t)
	if _, err := Connect(context.Background(), conf, nil, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err == nil || !strings.Contains(err.Error(), "Cloudflare API token") {
		t.Fatalf("err = %v", err)
	}
	if len(r.argv) != 0 {
		t.Fatal("created a job token before knowing Cloudflare would work")
	}
}

func TestReadyFailureStillConnects(t *testing.T) {
	conf, cf, _, r := setup(t)
	env := filepath.Join(t.TempDir(), "gpu1.env")
	_, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: env,
		Ready: func(context.Context, Values) error { return errors.New("not yet") }})
	var nr ReadyErr
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v", err)
	}
	if ms, _ := Machines(); len(ms) != 1 || len(r.revoked) != 0 {
		t.Fatalf("a slow Cloudflare must not undo the connection: machines %v revoked %v", ms, r.revoked)
	}
}

func TestWaitReady(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 || r.Header.Get("CF-Access-Client-Id") != "id.access" {
			http.Redirect(w, r, "https://team.cloudflareaccess.com/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	if err := WaitReady(20*time.Second)(context.Background(), Values{URL: srv.URL, AccessID: "id.access", AccessSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestDisconnectAfterAManualRevoke(t *testing.T) {
	conf, cf, f, r := setup(t)
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err != nil {
		t.Fatal(err)
	}
	r.revokeGone = true // someone ran pulse-admin remote revoke by hand
	if _, err := Disconnect(context.Background(), conf, cf, r.run, "gpu1"); err != nil {
		t.Fatalf("disconnect after a manual revoke: %v", err)
	}
	if len(f.tokens) != 0 || len(f.include) != 1 {
		t.Fatalf("Cloudflare token left live: %v %v", f.tokens, f.include)
	}
}

func TestDefaultEnvFileIsRemovedFromWhereItWasWritten(t *testing.T) {
	conf, cf, _, r := setup(t)
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(wd) })
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: "pulse-gpu1.env"}); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	os.Chdir(other) // disconnect from somewhere else
	os.WriteFile(filepath.Join(other, "pulse-gpu1.env"), []byte("unrelated"), 0o600)
	if _, err := Disconnect(context.Background(), conf, cf, r.run, "gpu1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pulse-gpu1.env")); err == nil {
		t.Error("the env file connect wrote is still there")
	}
	if _, err := os.Stat(filepath.Join(other, "pulse-gpu1.env")); err != nil {
		t.Error("an unrelated file with the same name was deleted")
	}
}

func TestTwoMachinesCantShareASecret(t *testing.T) {
	conf, cf, _, r := setup(t)
	d := Destination{GCPSecret: "pulse-shared", GCPProject: "proj"}
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", d); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(context.Background(), conf, cf, r.run, "eval", "gpu2", d); err == nil || !strings.Contains(err.Error(), "gpu1") {
		t.Fatalf("second machine on the same secret: %v", err)
	}
}

func TestBannerBeforeJSONAndOldHubText(t *testing.T) {
	conf, cf, f, r := setup(t)
	r.tokenOut = "Welcome to pulse-hub!\n" + `{"id":"t_1234abcd","job":"train","token":"pls_jobsecret"}` + "\n"
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err != nil {
		t.Fatalf("a login banner before the JSON: %v", err)
	}
	r.tokenOut = "Token t_9999abcd can only report job \"eval\". It's shown once.\n  export PULSE_TOKEN=pls_x\n"
	if _, err := Connect(context.Background(), conf, cf, r.run, "eval", "gpu2", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err == nil {
		t.Fatal("an older hub's text accepted")
	}
	if len(r.revoked) != 1 || r.revoked[0] != "t_9999abcd" {
		t.Fatalf("the token an older hub made must be revoked, not left untracked: %v", r.revoked)
	}
	if len(f.tokens) != 1 {
		t.Fatalf("no Cloudflare token should exist for the failed machine: %v", f.tokens)
	}
}

func TestPolicyKeepsItsOtherSettings(t *testing.T) {
	conf, cf, f, r := setup(t)
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err != nil {
		t.Fatal(err)
	}
	if f.extra["session_duration"] != "24h" || f.extra["approval_required"] != false {
		t.Fatalf("policy settings lost: %v", f.extra)
	}
	if _, ok := f.extra["id"]; ok {
		t.Error("sent the read-only id back")
	}
}

func TestLastRuleDoesntBlockDisconnect(t *testing.T) {
	conf, cf, f, r := setup(t)
	f.include = nil // the machine's token will be the only rule
	if _, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")}); err != nil {
		t.Fatal(err)
	}
	if _, err := Disconnect(context.Background(), conf, cf, r.run, "gpu1"); err != nil {
		t.Fatalf("disconnecting the only service token: %v", err)
	}
	if len(f.tokens) != 0 {
		t.Fatal("its token must still be deleted")
	}
}

func TestJobNamesTheHubAccepts(t *testing.T) {
	for _, ok := range []string{"nightly backup", "train #2", "ml@gpu1", "größe", "a/b:c+d"} {
		if !jobRe.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", " lead", strings.Repeat("x", 65), "x'y", "x;y", "x$y", "x`y"} {
		if jobRe.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOnlyOneHubCommandAtATime(t *testing.T) {
	t.Setenv("PULSE_HUB_DIR", t.TempDir())
	unlock, err := lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(); err == nil {
		t.Fatal("a second command got the lock")
	}
	unlock()
	if u, err := lock(); err != nil {
		t.Fatalf("after unlock: %v", err)
	} else {
		u()
	}
}

func TestExpiryIsRecorded(t *testing.T) {
	conf, cf, _, r := setup(t)
	m, err := Connect(context.Background(), conf, cf, r.run, "train", "gpu1", Destination{EnvFile: filepath.Join(t.TempDir(), "e")})
	if err != nil || m.CFExpires.Year() != 2027 {
		t.Fatalf("expiry %v %v", m.CFExpires, err)
	}
}

func TestFollowAndUnfollow(t *testing.T) {
	conf, cf, f, r := setup(t)
	t.Setenv("PULSE_HOME", t.TempDir())
	fc, err := Follow(context.Background(), conf, cf, r.run, "mac menubar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fc.ClientID != "st1.access" || LoadSecret(r.run, FollowService, fc.ClientID) != "cf-secret-st1-0123456789abcdef" {
		t.Fatalf("config %+v / secret not stored", fc)
	}
	added := false
	for _, a := range r.argv {
		added = added || strings.Contains(strings.Join(a, " "), "'remote' 'web' '--add-service' 'st1.access'")
	}
	if !added || len(f.include) != 2 {
		t.Fatalf("hub viewer / Access rule not added: %v %v", added, f.include)
	}
	if secretsIn(r.argv) {
		t.Fatal("a secret appeared in a command line")
	}
	if _, err := Follow(context.Background(), conf, cf, r.run, "mac", nil); err == nil {
		t.Fatal("followed twice")
	}
	if _, err := Unfollow(context.Background(), conf, cf, r.run); err != nil {
		t.Fatal(err)
	}
	if len(f.tokens) != 0 || len(f.include) != 1 || LoadSecret(r.run, FollowService, "st1.access") != "" {
		t.Fatalf("left behind: tokens %v include %v", f.tokens, f.include)
	}
}

func TestFollowUndoesOnFailure(t *testing.T) {
	conf, cf, f, r := setup(t)
	t.Setenv("PULSE_HOME", t.TempDir())
	conf.Admin = []string{"no-such-admin"} // the hub step fails
	if _, err := Follow(context.Background(), conf, cf, r.run, "mac", nil); err == nil {
		t.Fatal("followed although the hub refused")
	}
	if len(f.tokens) != 0 || len(f.include) != 1 {
		t.Fatalf("Cloudflare token left: %v %v", f.tokens, f.include)
	}
}

func TestFollowNotYetReadyKeepsItsSetup(t *testing.T) {
	conf, cf, f, r := setup(t)
	t.Setenv("PULSE_HOME", t.TempDir())
	_, err := Follow(context.Background(), conf, cf, r.run, "mac", func(context.Context, follow.Config, string) error {
		return errors.New("Cloudflare is slow")
	})
	var nr ReadyErr
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := follow.Load(); !ok || len(f.tokens) != 1 || LoadSecret(r.run, FollowService, "st1.access") == "" {
		t.Fatal("a slow Cloudflare undid the follow while saying it worked")
	}
}

func TestOldCloudflareTokenFileStillWorks(t *testing.T) {
	t.Setenv("PULSE_HUB_DIR", t.TempDir())
	t.Setenv("PULSE_CLOUDFLARE_API_TOKEN", "")
	os.WriteFile(filepath.Join(Dir(), "cloudflare-api-token"), []byte("oldTokenFromV031_0123456789abcdef\n"), 0o600)
	if got := LoadCloudflareToken(Exec); got != "oldTokenFromV031_0123456789abcdef" {
		t.Fatalf("token from the old location: %q", got)
	}
}
