// Command pulse is the Pulse CLI. It is pure Go (no cgo) so it can be
// cross-compiled for remote machines.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"pulse/internal/client"
	"pulse/internal/config"
	"pulse/internal/format"
	"pulse/internal/model"
	"pulse/internal/paths"
	"pulse/internal/runner"
)

// version and commit are set at build time (-ldflags -X).
var (
	version = "dev"
	commit  = ""
)

// jsonOut is set by a --json flag anywhere on the command line; read
// commands then print JSON instead of tables.
var jsonOut bool

// splitCommand separates the command, its arguments and a --json flag.
// --json is ours only for read commands: it's never taken from a command
// the user runs (pulse run ./tool --json) or from answers and values.
func splitCommand(argv []string) (cmd string, args []string, asJSON bool) {
	cmd, args = argv[0], argv[1:]
	if cmd == "--json" && len(args) > 0 { // pulse --json ls
		cmd, args, asJSON = args[0], args[1:], true
	}
	if readCommands[cmd] {
		var j bool
		args, j = stripJSON(args)
		asJSON = asJSON || j
	}
	return cmd, args, asJSON
}

var readCommands = map[string]bool{
	"ls": true, "list": true, "show": true, "inbox": true, "alert": true, "alerts": true, "action": true,
	"actions": true, "watch": true, "remote": true, "hub": true, "status": true, "version": true, "doctor": true,
}

func stripJSON(args []string) ([]string, bool) {
	out := args[:0:0]
	found := false
	for i, a := range args {
		if a == "--" { // everything after belongs to the user's command
			return append(out, args[i:]...), found
		}
		if a == "--json" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

const usage = `Pulse: monitor jobs from your menubar.

Everyday:
  pulse run [-n name] -- <command> [args...]   run a command and monitor it
  pulse push <job> key=value ... [--new]       send an update (creates the job)
  pulse watch <job> --cmd|--url|--file <src>   poll something that can't report itself
  pulse ask <job> "<question>" [answers…]      ask in Pulse, wait, print the answer
  pulse inbox | pulse answer <id> <answer>     questions waiting for you

Manage:
  pulse ls [--all] [--json]                    list jobs
  pulse show <job> [--json]                    everything about a job's latest run
  pulse rm <job>                               delete a job and its history
  pulse stop <job|run-id>                      stop a running job
  pulse alert <job> ['<rule>'] [flags]         list or add alert rules
  pulse mute <job> [1h|off]                    silence a job's notifications
  pulse label <job> key="Name" [--unit key=GB] readable names and units for values
  pulse action <job> <name> -- <command>       add a button that runs a command
  pulse do <job> <action>                      run an action now
  pulse apply [-f pulse.yaml]                  load job settings from a file
  pulse trust [-f pulse.yaml]                  allow a file's shell actions
  pulse remote [enable|token <job>|…]          let other machines report jobs
  pulse hub connect <job> --name <machine>    connect a machine to your always-on hub
  pulse open [job] [--browser]                 open a job's page (or the overview)
  pulse status                                 is the Pulse app running?
  pulse doctor                                 check the setup and say what to fix
  pulse completion zsh|bash|fish               shell completion script
  pulse skill [install]                        the coding-agent skill (SKILL.md)
  pulse version

Read commands take --json. Exit status: 0 ok, 1 error, 2 usage error,
3 timed out (pulse ask); pulse run exits with the command's own status.

Examples:
  pulse run -- python train.py
  pulse push deploy status=running step=2/5
  pulse push train epoch=3/10 loss=0.43
  pulse alert train 'val_loss new min'
  pulse alert train 'loss is nan' --run stop

Run "pulse <command> --help" for details (e.g. pulse push --help).
`

func main() {
	client.UserAgent = "pulse/" + version
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args, j := splitCommand(os.Args[1:])
	jsonOut = j
	if cmd == "help" && len(args) > 0 {
		os.Exit(printHelp(args[0]))
	}
	if _, ok := help[cmd]; ok && cmd != "run" && wantsHelp(args) {
		os.Exit(printHelp(cmd))
	}
	var code int
	switch cmd {
	case "run":
		code = cmdRun(args)
	case "push":
		code = cmdPush(args)
	case "ls", "list":
		code = cmdLs(args)
	case "show":
		code = cmdShow(args)
	case "rm":
		code = cmdRm(args)
	case "open":
		code = cmdOpen(args)
	case "stop":
		code = cmdStop(args)
	case "alert", "alerts":
		code = cmdAlert(args)
	case "mute":
		code = cmdMute(args)
	case "apply":
		code = cmdApply(args, false)
	case "trust":
		code = cmdApply(append(args, "--trust-mode"), true)
	case "action", "actions":
		code = cmdAction(args)
	case "do":
		code = cmdDo(args)
	case "label", "labels":
		code = cmdLabel(args)
	case "remote":
		code = cmdRemote(args)
	case "hub":
		code = cmdHub(args)
	case "watch":
		code = cmdWatch(args)
	case "ask":
		code = cmdAsk(args)
	case "inbox":
		code = cmdInbox(args)
	case "answer":
		code = cmdAnswer(args)
	case "status":
		code = cmdStatus()
	case "version", "--version", "-v":
		code = cmdVersion()
	case "doctor":
		code = cmdDoctor()
	case "completion":
		code = cmdCompletion(args)
	case "skill":
		code = cmdSkill(args)
	case "__jobs": // for shell completion
		code = cmdJobNames()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "pulse: unknown command %q\n\n%s", os.Args[1], usage)
		code = 2
	}
	os.Exit(code)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	name := fs.String("n", "", "job name")
	fs.StringVar(name, "name", "", "job name")
	file := fs.String("f", "", "config file")
	quiet := fs.Bool("q", false, "don't print Pulse notes")
	noSystem := fs.Bool("no-system", false, "don't report CPU, memory, GPU and disk use")
	fs.Usage = func() { fmt.Fprint(os.Stderr, help["run"]) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	command := fs.Args()

	explicit := len(command) > 0
	note := func(format string, a ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "pulse: "+format+"\n", a...)
		}
	}
	// A broken or ambiguous pulse.yaml must never stop an explicit command
	// from running: warn and carry on without it.
	cfg, path, found, err := loadConfig(*file)
	if err != nil {
		if !explicit || *file != "" {
			fmt.Fprintln(os.Stderr, "pulse:", err)
			return 2
		}
		note("ignoring %v", err)
		found = false
	}
	var job *config.Job
	if found {
		if len(cfg.Jobs) == 1 {
			job = &cfg.Jobs[0]
		} else {
			for i := range cfg.Jobs {
				if cfg.Jobs[i].Name == *name {
					job = &cfg.Jobs[i]
				}
			}
			if job == nil {
				if !explicit {
					fmt.Fprintf(os.Stderr, "pulse: %s defines several jobs; pick one with -n\n", filepath.Base(path))
					return 2
				}
				note("%s defines several jobs and none is named %q; running without it", filepath.Base(path), *name)
			}
		}
	}
	if !explicit {
		if job == nil || job.Run == "" {
			fs.Usage()
			return 2
		}
		command = []string{"/bin/sh", "-c", job.Run}
	}
	// Name: -n, else the file's name, else guessed (and marked as guessed so
	// two unrelated projects called "train" stay apart).
	inferred := false
	switch {
	case *name != "":
	case job != nil && job.Name != "":
		*name = job.Name
	case !explicit:
		*name, inferred = runner.InferName(strings.Fields(job.Run)), true
	default:
		*name, inferred = runner.InferName(command), true
	}
	if job != nil {
		job.Name = *name // the settings belong to the job actually being run
		c, err := func() (*client.Client, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return client.Connect(ctx, true, 3*time.Second)
		}()
		if err == nil {
			_, err = sendConfig(c, *job, path, true, false)
			var apiErr *client.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.Intercepted:
				note("couldn't apply %s: %v", filepath.Base(path), err)
			case errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 404): // a remote endpoint
				note("%s settings can only be applied on the Mac running Pulse (pulse apply there)", filepath.Base(path))
			case err != nil:
				note("couldn't apply %s: %v", filepath.Base(path), err)
			}
		}
	}
	return runner.Run(runner.Options{Name: *name, Inferred: inferred, Args: command, Quiet: *quiet, NoSystem: *noSystem})
}

func cmdPush(args []string) int {
	var job string
	forceNew, strict := false, false
	fields := map[string]any{}
	for _, a := range args {
		switch {
		case a == "--new":
			forceNew = true
		case a == "--strict":
			strict = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pulse push: unknown flag %s\n", a)
			return 2
		case job == "":
			job = a
		default:
			k, v, ok := strings.Cut(a, "=")
			if !ok || k == "" {
				fmt.Fprintf(os.Stderr, "pulse push: expected key=value, got %q\n", a)
				return 2
			}
			fields[k] = v
		}
	}
	if job == "" {
		fmt.Fprintln(os.Stderr, "usage: pulse push <job> key=value ... [--new] [--strict]")
		return 2
	}

	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "pulse: %v; update not recorded\n", err)
		if strict {
			return 1
		}
		return 0 // never break the caller's script
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, false, 4*time.Second)
	if err != nil {
		return fail(err)
	}
	path := "/api/v1/push/" + url.PathEscape(job)
	if forceNew {
		path += "?new=1"
	}
	var resp model.BatchResponse
	if err := c.Do(ctx, "POST", path, fields, &resp); err != nil {
		return fail(err)
	}
	return 0
}

func cmdLs(args []string) int {
	all, asJSON, hubJSON := false, jsonOut, false
	for _, a := range args {
		switch a {
		case "--all", "-a":
			all = true
		case "--hub":
			hubJSON = true
		default:
			return printHelp("ls") + 2
		}
	}
	c, err := client.Resolve(true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pulse:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out struct {
		Runs []model.Run `json:"runs"`
	}
	path := "/api/v1/runs?hub=1" // a followed hub's runs too, marked "(hub)"
	if all {
		path += "&all=1"
	}
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			err = client.ErrNotRunning
		}
		fmt.Fprintln(os.Stderr, "pulse:", err)
		return 1
	}
	now := time.Now()
	var runs []model.Run
	for _, r := range out.Runs {
		if r.Hub != "" && asJSON && !hubJSON {
			continue // --json keeps its old meaning (this machine's runs); --hub adds a followed hub's
		}
		if all || r.Status == model.Running || now.Sub(r.UpdatedAt) <= 24*time.Hour {
			runs = append(runs, r)
		}
	}
	if asJSON {
		if runs == nil {
			runs = []model.Run{}
		}
		return printJSON(runs)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tRUN\tSTATUS\tPROGRESS\tMETRICS\tTIME")
	shown := 0
	for _, r := range runs {
		prog := "-"
		if p, ok := r.Primary(); ok {
			prog = fmt.Sprintf("%s (%d%%)", format.Progress(p), int(p.Frac()*100))
			if p.ETA != nil && r.Status == model.Running {
				prog += ", ~" + format.Duration(time.Until(*p.ETA)) + " left"
			}
		}
		metrics := format.KeySummary(&r) // raw keys: they're what alerts and --json use
		if metrics == "" {
			metrics = "-"
		}
		when := format.Duration(r.Duration(now))
		if r.Status != model.Running {
			when = format.Duration(now.Sub(r.UpdatedAt)) + " ago"
		}
		job := r.Job
		if r.Hub != "" {
			job += " (hub)" // a followed hub's run: act on it there
		}
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\t%s\n", job, r.Seq, r.Status, prog, metrics, when)
		shown++
	}
	tw.Flush()
	if shown == 0 {
		fmt.Println("\nNo jobs yet. Try: pulse run -- python train.py")
	}
	return 0
}

func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func cmdOpen(args []string) int {
	page, browser := "/popover", false
	for _, a := range args {
		switch {
		case a == "--browser" || a == "-b":
			browser = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pulse open: unknown flag %s\n", a)
			return 2
		default:
			page = "/j/" + url.PathEscape(a)
		}
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	if !browser {
		// Ask the app to show the page in its own window.
		if err := c.Do(ctx, "POST", "/api/v1/ui/open?path="+url.QueryEscape(page), nil, nil); err == nil {
			return 0
		}
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.Do(ctx, "POST", "/api/v1/login-link?next="+url.QueryEscape(page), nil, &out); err != nil {
		fmt.Fprintln(os.Stderr, "pulse:", err)
		return 1
	}
	if err := openBrowser(out.URL); err != nil {
		fmt.Println(out.URL) // no browser opener (e.g. over SSH): print the link
	}
	return 0
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Run()
	case "linux":
		return exec.Command("xdg-open", u).Run()
	}
	return errors.New("unsupported")
}

func cmdStatus() int {
	c, err := client.Resolve(false)
	running := err == nil && c.Healthy(context.Background())
	if jsonOut {
		out := map[string]any{"running": running, "data_dir": paths.Home(), "cli_version": version}
		if running {
			out["url"] = c.BaseURL
		}
		printJSON(out)
	} else if running {
		fmt.Println("Pulse app is running at", c.BaseURL)
		fmt.Println("Data directory:", paths.Home())
	} else {
		fmt.Println("Pulse app is not running.")
		fmt.Println("Data directory:", paths.Home())
	}
	if !running {
		return 1
	}
	return 0
}

func cmdVersion() int {
	v := version
	if commit != "" {
		v += " (" + commit + ")"
	}
	app := ""
	if c, err := client.Resolve(false); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var h struct {
			Version string `json:"version"`
		}
		if c.Do(ctx, "GET", "/api/v1/health", nil, &h) == nil {
			app = h.Version
		}
		cancel()
	}
	if jsonOut {
		return printJSON(map[string]string{"cli": version, "commit": commit, "app": app})
	}
	fmt.Println("pulse", v)
	if app != "" {
		fmt.Println("app  ", app)
	}
	return 0
}

func cmdJobNames() int {
	c, err := client.Resolve(true)
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out struct {
		Jobs []string `json:"jobs"`
	}
	if c.Do(ctx, "GET", "/api/v1/jobs", nil, &out) != nil {
		return 1
	}
	for _, j := range out.Jobs {
		fmt.Println(j)
	}
	return 0
}

// admin connects to the local app with the admin token. These commands only
// work on the Mac running Pulse (remote machines get push access only).
func admin() (*client.Client, context.Context, context.CancelFunc, bool) {
	if u := os.Getenv("PULSE_URL"); u != "" && !isLocalURL(u) {
		fmt.Fprintln(os.Stderr, "pulse: this command only works on the Mac running Pulse (PULSE_URL points at another machine)")
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	c, err := client.Connect(ctx, true, 4*time.Second)
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "pulse:", err)
		return nil, nil, nil, false
	}
	return c, ctx, cancel, true
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "pulse:", err)
	return 1
}

func cmdStop(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pulse stop <job|run-id>")
		return 2
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	run, err := resolveRun(ctx, c, args[0], true)
	if err != nil {
		return fail(err)
	}
	runID, label := run.ID, fmt.Sprintf("%s #%d", run.Job, run.Seq)
	var res struct {
		Result string `json:"result"`
	}
	if err := c.Do(ctx, "POST", "/api/v1/runs/"+url.PathEscape(runID)+"/stop", nil, &res); err != nil {
		return fail(err)
	}
	if res.Result == "stopping" {
		fmt.Printf("Stopping %s (Ctrl-C sent; it will be killed if it doesn't exit within 20s).\n", label)
	} else {
		fmt.Printf("Marked %s as stopped.\n", label)
	}
	return 0
}

func cmdAlert(args []string) int {
	usage := func() int {
		fmt.Fprint(os.Stderr, `usage:
  pulse alert <job>                              list rules
  pulse alert <job> '<rule>' [--run stop] [--level info|warn|error] [--cooldown 10m] [--once]
  pulse alert <job> --off <id> | --on <id> | --rm <id>

rules:
  val_loss < 0.3          gpu_util < 0.2         epoch >= 5
  val_loss new min        accuracy new max       loss is nan
  progress >= 90%         eta < 5m               no update for 15m
`)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	job, rest := args[0], args[1:]
	var expr, level, action, cooldown, toggle, id, ask string
	var answers []string
	once := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		next := func() string {
			if i+1 < len(rest) {
				i++
				return rest[i]
			}
			return ""
		}
		switch a {
		case "--run":
			action = next()
		case "--level":
			level = next()
		case "--cooldown":
			cooldown = next()
		case "--ask":
			if ask = next(); strings.TrimSpace(ask) == "" {
				fmt.Fprintln(os.Stderr, "pulse alert: --ask needs a question")
				return 2
			}
		case "--answer":
			answers = append(answers, next())
		case "--once":
			once = true
		case "--off", "--on", "--rm":
			toggle, id = a, next()
		default:
			if strings.HasPrefix(a, "--") || expr != "" {
				return usage()
			}
			expr = a
		}
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	q := "?job=" + url.QueryEscape(job)
	switch {
	case toggle == "--rm":
		if err := c.Do(ctx, "DELETE", "/api/v1/alerts/"+url.PathEscape(id)+q, nil, nil); err != nil {
			return fail(err)
		}
	case toggle != "":
		if err := c.Do(ctx, "PATCH", "/api/v1/alerts/"+url.PathEscape(id)+q, map[string]bool{"enabled": toggle == "--on"}, nil); err != nil {
			return fail(err)
		}
	case expr != "":
		body := map[string]any{"expr": expr, "level": level, "action": action, "cooldown": cooldown, "once": once,
			"ask": ask, "answers": answers}
		var rule struct {
			ID string `json:"id"`
		}
		if err := c.Do(ctx, "POST", "/api/v1/alerts"+q, body, &rule); err != nil {
			return fail(err)
		}
		fmt.Printf("Added %s.\n\n", rule.ID)
	}
	var view struct {
		Rules []struct {
			ID      string `json:"id"`
			Expr    string `json:"expr"`
			Level   string `json:"level"`
			Action  string `json:"action"`
			Builtin bool   `json:"builtin"`
			Enabled bool   `json:"enabled"`
			Ask     string `json:"ask"`
		} `json:"rules"`
		MutedUntil *time.Time `json:"muted_until"`
	}
	if err := c.Do(ctx, "GET", "/api/v1/alerts"+q, nil, &view); err != nil {
		return fail(err)
	}
	if jsonOut {
		return printJSON(view)
	}
	fmt.Printf("Alerts for %s (plus finished / failed / lost, on unless muted):\n", job)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tRULE\tLEVEL\tTHEN\tSTATE")
	for _, r := range view.Rules {
		state := "on"
		if !r.Enabled {
			state = "off"
		}
		if r.Builtin {
			state += " (built in)"
		}
		then := "-"
		if r.Action != "" {
			then = r.Action
		}
		if r.Ask != "" {
			then = "ask: " + r.Ask
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Expr, r.Level, then, state)
	}
	tw.Flush()
	if view.MutedUntil != nil {
		fmt.Printf("\nMuted until %s.\n", view.MutedUntil.Local().Format("15:04"))
	}
	return 0
}

func cmdMute(args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: pulse mute <job> [1h|30m|off]")
		return 2
	}
	dur := "1h"
	if len(args) == 2 {
		dur = args[1]
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	if err := c.Do(ctx, "POST", "/api/v1/mute?job="+url.QueryEscape(args[0])+"&for="+url.QueryEscape(dur), nil, nil); err != nil {
		return fail(err)
	}
	if dur == "off" || dur == "0" {
		fmt.Printf("%s is no longer muted.\n", args[0])
	} else {
		fmt.Printf("Muted %s for %s.\n", args[0], dur)
	}
	return 0
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}
	return 0
}

func cmdShow(args []string) int {
	var target string
	asJSON := jsonOut
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-") || target != "":
			return printHelp("show") + 2
		default:
			target = a
		}
	}
	if target == "" {
		return printHelp("show") + 2
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	r, err := resolveRun(ctx, c, target, false)
	if err != nil {
		return fail(err)
	}
	if asJSON {
		return printJSON(r)
	}
	now := time.Now()
	fmt.Printf("%s #%d  %s", r.Job, r.Seq, r.Status)
	if r.Status == model.Running {
		fmt.Printf(" for %s", format.Duration(r.Duration(now)))
	} else {
		fmt.Printf(", updated %s ago", format.Duration(now.Sub(r.UpdatedAt)))
	}
	fmt.Println()
	if r.Message != "" {
		fmt.Printf("  %s\n", strings.ReplaceAll(r.Message, "\n", "\n  "))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(r.Progress) > 0 {
		fmt.Fprintln(tw, "\nProgress")
		for _, p := range r.Progress {
			eta := ""
			switch {
			case r.Status != model.Running || p.Cur >= p.Total:
			case p.ETA != nil:
				eta = "~" + format.Duration(time.Until(*p.ETA)) + " left"
			default:
				eta = "(ETA once it moves again)"
			}
			fmt.Fprintf(tw, "  %s\t%s/%s\t%d%%\t%s\n", p.Name, format.Num(p.Cur), format.Num(p.Total), int(p.Frac()*100), eta)
		}
	}
	if len(r.Metrics) > 0 {
		fmt.Fprintln(tw, "\nMetrics\t\t")
		for _, m := range r.OrderedMetrics() {
			fmt.Fprintf(tw, "  %s\t%s\t(%d values)\n", m.Name, format.Num(float64(m.Value)), m.N)
		}
	}
	if len(r.Fields) > 0 {
		fmt.Fprintln(tw, "\nFields\t")
		for _, f := range r.Fields {
			fmt.Fprintf(tw, "  %s\t%s\n", f.Name, f.Value)
		}
	}
	fmt.Fprintln(tw, "\nRun\t")
	fmt.Fprintf(tw, "  id\t%s\n  source\t%s\n", r.ID, r.Source)
	if r.Cmd != "" {
		fmt.Fprintf(tw, "  command\t%s\n", r.Cmd)
	}
	if r.Cwd != "" {
		fmt.Fprintf(tw, "  folder\t%s\n", r.Cwd)
	}
	if r.Host != "" {
		fmt.Fprintf(tw, "  host\t%s\n", r.Host)
	}
	fmt.Fprintf(tw, "  started\t%s\n", r.StartedAt.Local().Format("Jan 2 15:04:05"))
	if r.Link != "" {
		fmt.Fprintf(tw, "  link\t%s\n", r.Link)
	}
	if r.ConfigSource != "" {
		fmt.Fprintf(tw, "  config\t%s\n", r.ConfigSource)
	}
	if len(r.Actions) > 0 {
		var names []string
		for _, a := range r.Actions {
			names = append(names, a.Name)
		}
		fmt.Fprintf(tw, "  actions\t%s (pulse do %s <action>)\n", strings.Join(names, ", "), r.Job)
	}
	tw.Flush()
	return 0
}

func cmdRm(args []string) int {
	var job string
	yes := false
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case strings.HasPrefix(a, "-") || job != "":
			return printHelp("rm") + 2
		default:
			job = a
		}
	}
	if job == "" {
		return printHelp("rm") + 2
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	var out struct {
		Total int `json:"total"`
	}
	err := c.Do(ctx, "GET", "/api/v1/runs?job="+url.QueryEscape(job), nil, &out)
	cancel()
	if err != nil {
		return fail(err)
	}
	if out.Total == 0 {
		return fail(fmt.Errorf("no job named %q (see pulse ls)", job))
	}
	if !yes {
		if !isTerminal() {
			return fail(fmt.Errorf("this deletes %d run(s) of %s; pass --yes to confirm", out.Total, job))
		}
		// Asked outside any request deadline; the delete gets its own.
		if !askYes(fmt.Sprintf("Delete %s and its %d run(s), metrics and logs?", job, out.Total)) {
			fmt.Println("Kept.")
			return 0
		}
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	var res struct {
		Runs int `json:"runs_deleted"`
	}
	if err := c.Do(dctx, "DELETE", "/api/v1/jobs?job="+url.QueryEscape(job), nil, &res); err != nil {
		return fail(err)
	}
	fmt.Printf("Deleted %s (%d run(s)).\n", job, res.Runs)
	return 0
}

func cmdLabel(args []string) int {
	if len(args) < 2 {
		return printHelp("label") + 2
	}
	job := args[0]
	labels, units := map[string]string{}, map[string]string{}
	for i := 1; i < len(args); i++ {
		dst := labels
		a := args[i]
		if a == "--unit" {
			if i+1 >= len(args) {
				return printHelp("label") + 2
			}
			i++
			a, dst = args[i], units
		}
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return printHelp("label") + 2
		}
		dst[k] = v
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	if err := c.Do(ctx, "PUT", "/api/v1/labels?job="+url.QueryEscape(job), map[string]any{"labels": labels, "units": units}, nil); err != nil {
		return fail(err)
	}
	fmt.Printf("Updated %d name(s) and %d unit(s) for %s.\n", len(labels), len(units), job)
	return 0
}
