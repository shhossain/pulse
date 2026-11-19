// Package hubctl connects job machines to a Pulse hub from your own
// computer: it creates the machine's job token on the hub (through the
// hub's admin command, e.g. SSH), its own Cloudflare Access service token,
// and hands the four values to the machine as one secret (a 0600 env file,
// or a Google Secret Manager secret). Disconnecting undoes all three.
//
// Secrets never go to stdout, logs or command lines: tokens travel through
// pipes and files only.
package hubctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Config is how to reach and administer a hub. Saved in ConfigFile (0600).
type Config struct {
	URL   string   `json:"url"`   // the hub's public URL, e.g. https://pulse.example.com
	Admin []string `json:"admin"` // runs a shell command on the hub; the command is appended as the last argument
	// Cloudflare Access protecting the hub (optional: without it, machines
	// only need a job token).
	Cloudflare *CloudflareConfig `json:"cloudflare,omitempty"`
}

// CloudflareConfig names the Access pieces per-machine service tokens join.
type CloudflareConfig struct {
	Account string `json:"account"` // account ID
	App     string `json:"app"`     // Access application ID
	Policy  string `json:"policy"`  // the application's service-auth policy ID
}

// Machine is one connected machine, recorded so it can be disconnected.
type Machine struct {
	Name       string    `json:"name"`
	Job        string    `json:"job"`
	JobTokenID string    `json:"job_token_id"`
	CFTokenID  string    `json:"cf_token_id,omitempty"`
	CFExpires  time.Time `json:"cf_expires,omitempty"` // the service token stops working then: reconnect before
	Secret     string    `json:"secret"`               // where its values went: a file path or gcp:projects/P/secrets/S
	Created    time.Time `json:"created"`
}

// Dir holds the hub config and machine list.
func Dir() string {
	if d := os.Getenv("PULSE_HUB_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pulse")
}

func ConfigFile() string   { return filepath.Join(Dir(), "hub.json") }
func machinesFile() string { return filepath.Join(Dir(), "hub-machines.json") }

// LoadConfig reads the hub config.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, errors.New("no hub configured: run pulse hub setup first")
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigFile(), err)
	}
	return c, c.Validate()
}

// Validate checks a config.
func (c Config) Validate() error {
	if !strings.HasPrefix(c.URL, "https://") {
		return errors.New("hub URL must be https://")
	}
	if len(c.Admin) == 0 {
		return errors.New("an admin command is required (how to run pulse-admin on the hub)")
	}
	if cf := c.Cloudflare; cf != nil && (!cfID.MatchString(cf.Account) || cf.App == "" || cf.Policy == "") {
		return errors.New("cloudflare needs account, app and policy IDs")
	}
	return nil
}

var cfID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// SaveConfig writes the hub config.
func SaveConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writePrivate(ConfigFile(), b)
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Machines lists connected machines.
func Machines() ([]Machine, error) {
	var ms []Machine
	b, err := os.ReadFile(machinesFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &ms); err != nil {
		return nil, fmt.Errorf("%s: %w", machinesFile(), err)
	}
	return ms, nil
}

func saveMachines(ms []Machine) error {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	b, _ := json.MarshalIndent(ms, "", "  ")
	return writePrivate(machinesFile(), b)
}

// names: job names as the hub cleans them, machine names for token labels.
var (
	// jobRe is what the hub keeps of a job name (engine.CleanJobName).
	jobRe     = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} _.:#/@+-]{0,63}$`)
	machineRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// Values are what a machine needs.
type Values struct {
	URL, Token, AccessID, AccessSecret string
}

// Env renders v as shell exports.
func (v Values) Env() []byte {
	var b bytes.Buffer
	b.WriteString("# Pulse hub credentials for this machine. Secret: keep out of images, repos and logs.\n")
	fmt.Fprintf(&b, "export PULSE_URL=%s\nexport PULSE_TOKEN=%s\n", shellQuote(v.URL), shellQuote(v.Token))
	if v.AccessID != "" {
		fmt.Fprintf(&b, "export PULSE_CF_ACCESS_CLIENT_ID=%s\nexport PULSE_CF_ACCESS_CLIENT_SECRET=%s\n", shellQuote(v.AccessID), shellQuote(v.AccessSecret))
	}
	return b.Bytes()
}

// Runner runs external commands (tests replace it).
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) (stdout []byte, err error)

// Exec is the real Runner. A failing command's stderr is in the error.
func Exec(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return nil, fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return out.Bytes(), nil
}

// shellQuote quotes s for the hub's shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// admin runs `pulse-admin <args>` on the hub and returns its stdout.
func (c Config) admin(ctx context.Context, run Runner, args ...string) ([]byte, error) {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	argv := append(append([]string{}, c.Admin[1:]...), "pulse-admin "+strings.Join(q, " "))
	return run(ctx, nil, c.Admin[0], argv...)
}

// Destination is where a machine's values go.
type Destination struct {
	EnvFile    string // a 0600 file on this computer, to copy to the machine
	GCPSecret  string // a Secret Manager secret name
	GCPProject string
	GCPAccount string // gcloud account (optional)
	GrantTo    string // a service account allowed to read the secret (the VM's)

	// Ready, if set, is called with the new values once stored, to wait
	// until they work (Cloudflare takes a little while to honor a new
	// service token). Its error is reported, not fatal.
	Ready func(ctx context.Context, v Values) error
}

// ReadyErr is set on the Machine Connect returns when Ready failed.
type ReadyErr struct{ Err error }

func (e ReadyErr) Error() string { return e.Err.Error() }

// WaitReady polls the hub's health endpoint with v until Cloudflare lets it
// through and Pulse answers, for up to timeout.
func WaitReady(timeout time.Duration) func(ctx context.Context, v Values) error {
	return func(ctx context.Context, v Values) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		hc := &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for {
			req, _ := http.NewRequestWithContext(ctx, "GET", v.URL+"/api/v1/health", nil)
			req.Header.Set("User-Agent", "pulse-hub-connect")
			if v.AccessID != "" {
				req.Header.Set("CF-Access-Client-Id", v.AccessID)
				req.Header.Set("CF-Access-Client-Secret", v.AccessSecret)
			}
			if resp, err := hc.Do(req); err == nil {
				ok := resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
				resp.Body.Close()
				if ok {
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return errors.New("the hub doesn't accept the new credentials yet; Cloudflare can take a minute: try again shortly")
			case <-time.After(3 * time.Second):
			}
		}
	}
}

// Connect creates a machine's credentials and stores them at dest. On any
// failure it undoes what it created.
func Connect(ctx context.Context, c Config, cf *Cloudflare, run Runner, job, machine string, dest Destination) (m Machine, err error) {
	if !jobRe.MatchString(job) {
		return m, fmt.Errorf("job %q: up to 64 letters, digits, spaces and . _ : # / @ + - only", job)
	}
	if !machineRe.MatchString(machine) {
		return m, fmt.Errorf("machine %q: letters, digits and . _ - only", machine)
	}
	if (dest.EnvFile == "") == (dest.GCPSecret == "") {
		return m, errors.New("choose one destination: an env file or a GCP secret")
	}
	unlock, err := lock()
	if err != nil {
		return m, err
	}
	defer unlock()
	ms, err := Machines()
	if err != nil {
		return m, err
	}
	gcpRef := ""
	if dest.GCPSecret != "" {
		gcpRef = "gcp:projects/" + dest.GCPProject + "/secrets/" + dest.GCPSecret
	}
	if dest.EnvFile != "" {
		if dest.EnvFile, err = filepath.Abs(dest.EnvFile); err != nil {
			return m, err
		}
	}
	for _, x := range ms {
		switch {
		case x.Name == machine:
			return m, fmt.Errorf("%s is already connected (to %s): pulse hub disconnect %s first", machine, x.Job, machine)
		case gcpRef != "" && x.Secret == gcpRef:
			return m, fmt.Errorf("secret %s already holds %s's credentials: give each machine its own", dest.GCPSecret, x.Name)
		}
	}
	if dest.EnvFile != "" {
		if _, err := os.Stat(dest.EnvFile); err == nil {
			return m, fmt.Errorf("%s exists: not overwriting it", dest.EnvFile)
		}
	}
	if c.Cloudflare != nil && cf == nil {
		return m, errors.New("the hub is behind Cloudflare Access: a Cloudflare API token is needed (pulse hub setup --cloudflare-token-stdin)")
	}
	m = Machine{Name: machine, Job: job, Created: time.Now().UTC()}
	var undo []func()
	defer func() {
		var notReady ReadyErr // connected, just not confirmed: keep it
		if err != nil && !errors.As(err, &notReady) {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()

	// 1. The job token, on the hub.
	out, err := c.admin(ctx, run, "--json", "remote", "token", job, "--name", machine)
	if err != nil {
		return m, fmt.Errorf("creating the job token on the hub: %w", err)
	}
	var tok struct {
		ID, Job, Token string
	}
	// Login banners can come before the JSON on stdout: take the JSON object.
	if i, j := bytes.IndexByte(out, '{'), bytes.LastIndexByte(out, '}'); i >= 0 && j > i {
		_ = json.Unmarshal(out[i:j+1], &tok)
	}
	if tok.ID == "" || !strings.HasPrefix(tok.Token, "pls_") {
		// A token may have been made all the same (an older hub prints text):
		// revoke it rather than leave it live and untracked.
		if id := tokenIDRe.Find(out); id != nil {
			_, _ = c.admin(context.Background(), run, "remote", "revoke", string(id))
		}
		return m, errors.New("couldn't read the job token the hub made (an older pulse there? it needs v0.3.1+); nothing was left behind")
	}
	m.JobTokenID = tok.ID
	undo = append(undo, func() { _, _ = c.admin(context.Background(), run, "remote", "revoke", tok.ID) })
	vals := Values{URL: c.URL, Token: tok.Token}

	// 2. Its own Cloudflare service token, allowed through Access.
	if c.Cloudflare != nil {
		st, err := cf.CreateServiceToken(ctx, "pulse: "+machine+" ("+job+")")
		if err != nil {
			return m, fmt.Errorf("creating the Cloudflare service token: %w", err)
		}
		m.CFTokenID, m.CFExpires = st.ID, st.ExpiresAt
		undo = append(undo, func() { _ = cf.DeleteServiceToken(context.Background(), st.ID) })
		if err := cf.AllowServiceToken(ctx, st.ID); err != nil {
			return m, fmt.Errorf("allowing it through Access: %w", err)
		}
		undo = append(undo, func() { _ = cf.DisallowServiceToken(context.Background(), st.ID) })
		vals.AccessID, vals.AccessSecret = st.ClientID, st.ClientSecret
	}

	// 3. The values, to one place only.
	if dest.EnvFile != "" {
		if err := writePrivate(dest.EnvFile, vals.Env()); err != nil {
			return m, err
		}
		undo = append(undo, func() { _ = os.Remove(dest.EnvFile) })
		m.Secret = dest.EnvFile
	} else {
		ref, err := putGCPSecret(ctx, run, dest, vals.Env(), "pulse-machine", machine)
		if err != nil {
			return m, err
		}
		m.Secret = ref
	}
	ms = append(ms, m)
	if err := saveMachines(ms); err != nil {
		return m, err
	}
	if dest.Ready != nil {
		if rerr := dest.Ready(ctx, vals); rerr != nil {
			return m, ReadyErr{rerr} // connected; just not confirmed yet
		}
	}
	return m, nil
}

func gcloudArgs(dest Destination, args ...string) []string {
	if dest.GCPProject != "" {
		args = append(args, "--project="+dest.GCPProject)
	}
	if dest.GCPAccount != "" {
		args = append(args, "--account="+dest.GCPAccount)
	}
	return append(args, "--quiet")
}

var secretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

func putGCPSecret(ctx context.Context, run Runner, dest Destination, data []byte, labelKey, labelValue string) (string, error) {
	if !secretRe.MatchString(dest.GCPSecret) {
		return "", fmt.Errorf("secret name %q: letters, digits, _ and - only", dest.GCPSecret)
	}
	if dest.GCPProject == "" {
		return "", errors.New("--gcp-project is required with --gcp-secret")
	}
	label := strings.ToLower(regexp.MustCompile(`[^a-z0-9_-]`).ReplaceAllString(strings.ToLower(labelValue), "-"))
	if _, err := run(ctx, data, "gcloud", gcloudArgs(dest, "secrets", "create", dest.GCPSecret,
		"--replication-policy=automatic", "--data-file=-", "--labels="+labelKey+"="+label)...); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return "", fmt.Errorf("creating the secret: %w", err)
		}
		if _, err := run(ctx, data, "gcloud", gcloudArgs(dest, "secrets", "versions", "add", dest.GCPSecret, "--data-file=-")...); err != nil {
			return "", fmt.Errorf("adding a secret version: %w", err)
		}
	}
	if dest.GrantTo != "" {
		if _, err := run(ctx, nil, "gcloud", gcloudArgs(dest, "secrets", "add-iam-policy-binding", dest.GCPSecret,
			"--member=serviceAccount:"+dest.GrantTo, "--role=roles/secretmanager.secretAccessor")...); err != nil {
			return "", fmt.Errorf("letting %s read the secret: %w", dest.GrantTo, err)
		}
	}
	return "gcp:projects/" + dest.GCPProject + "/secrets/" + dest.GCPSecret, nil
}

// Disconnect revokes a machine's job token and Cloudflare service token and
// forgets it. The copy of its values on the machine stops working; a GCP
// secret is left for you to delete (it may hold other versions).
func Disconnect(ctx context.Context, c Config, cf *Cloudflare, run Runner, machine string) (Machine, error) {
	unlock, err := lock()
	if err != nil {
		return Machine{}, err
	}
	defer unlock()
	ms, err := Machines()
	if err != nil {
		return Machine{}, err
	}
	i := -1
	for j, x := range ms {
		if x.Name == machine {
			i = j
		}
	}
	if i < 0 {
		return Machine{}, fmt.Errorf("no connected machine named %s (see pulse hub list)", machine)
	}
	m := ms[i]
	// Already revoked (by hand, or by an earlier attempt that failed later
	// on): carry on to the rest.
	if _, err := c.admin(ctx, run, "remote", "revoke", m.JobTokenID); err != nil &&
		!strings.Contains(err.Error(), "no token") {
		return m, fmt.Errorf("revoking the job token: %w", err)
	}
	if m.CFTokenID != "" {
		if cf == nil {
			return m, errors.New("a Cloudflare API token is needed to remove its service token")
		}
		// If it's the policy's only rule, deleting the token is what cuts it off.
		if err := cf.DisallowServiceToken(ctx, m.CFTokenID); err != nil && !errors.Is(err, errLastRule) {
			return m, fmt.Errorf("removing it from Access: %w", err)
		}
		if err := cf.DeleteServiceToken(ctx, m.CFTokenID); err != nil && !strings.Contains(err.Error(), "not found") {
			return m, fmt.Errorf("deleting its service token: %w", err)
		}
	}
	if filepath.IsAbs(m.Secret) {
		_ = os.Remove(m.Secret) // the local copy connect wrote, if still here
	}
	ms = append(ms[:i], ms[i+1:]...)
	return m, saveMachines(ms)
}

// CloudflareTokenFile is where the API token is kept off macOS (0600).
func CloudflareTokenFile() string { return filepath.Join(Dir(), "cloudflare-api-token") }

const keychainService = "pulse-cloudflare-api-token"

var (
	apiTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20,200}$`)
	tokenIDRe  = regexp.MustCompile(`\bt_[0-9a-f]{8,}\b`)
)

// lock keeps two pulse hub commands from editing the machine list and the
// Access policy at once (each is a read-modify-write).
func lock() (func(), error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(Dir(), "hub.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(f); err != nil {
		f.Close()
		return nil, errors.New("another pulse hub command is running: try again when it's done")
	}
	return func() { f.Close() }, nil
}

// SaveCloudflareToken stores the API token: the macOS Keychain, else a 0600 file.
func SaveCloudflareToken(run Runner, token string) (where string, err error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("empty token")
	}
	if runtime.GOOS == "darwin" && os.Getenv("PULSE_HUB_DIR") == "" {
		// security -i reads its commands from stdin (not the terminal), so the
		// token never appears in a command line or a prompt.
		if !apiTokenRe.MatchString(token) {
			return "", errors.New("that doesn't look like a Cloudflare API token")
		}
		cmd := fmt.Sprintf("add-generic-password -U -a pulse -s %s -w \"%s\"\n", keychainService, token)
		if _, err := run(context.Background(), []byte(cmd), "security", "-i"); err != nil {
			return "", err
		}
		if LoadCloudflareToken(run) != token {
			return "", errors.New("the Keychain didn't keep the token")
		}
		return "the macOS Keychain (" + keychainService + ")", nil
	}
	return CloudflareTokenFile(), writePrivate(CloudflareTokenFile(), []byte(token+"\n"))
}

// LoadCloudflareToken reads the API token ("" if none): PULSE_CLOUDFLARE_API_TOKEN,
// the Keychain, or the file.
func LoadCloudflareToken(run Runner) string {
	if t := strings.TrimSpace(os.Getenv("PULSE_CLOUDFLARE_API_TOKEN")); t != "" {
		return t
	}
	if runtime.GOOS == "darwin" && os.Getenv("PULSE_HUB_DIR") == "" {
		if out, err := run(context.Background(), nil, "security", "find-generic-password", "-a", "pulse", "-s", keychainService, "-w"); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	b, _ := os.ReadFile(CloudflareTokenFile())
	return strings.TrimSpace(string(b))
}
