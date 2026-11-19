package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"pulse/internal/client"
	"pulse/internal/config"
	"pulse/internal/model"
)

type applyResult struct {
	Job       string          `json:"job"`
	Alerts    int             `json:"alerts"`
	Actions   int             `json:"actions"`
	Untrusted []config.Action `json:"untrusted"`
}

// askYes asks a yes/no question on the terminal (replaceable in tests).
var askYes = func(question string) bool {
	fmt.Fprint(os.Stderr, question+" [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// isTerminal reports whether stdin is interactive (replaceable in tests).
var isTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// sendConfig applies one job from a config file. When shell actions need
// trust it asks (if ask and we're on a terminal) unless yes is set. The
// question is asked outside any request deadline, and the follow-up request
// gets its own timeout, so a slow answer still counts.
func sendConfig(c *client.Client, j config.Job, path string, ask, yes bool) (applyResult, error) {
	put := func(trust bool) (applyResult, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var res applyResult // fresh each time: omitted fields must not carry over
		err := c.Do(ctx, "PUT", "/api/v1/jobs/config", map[string]any{"job": j, "source": path, "trust": trust}, &res)
		return res, err
	}
	res, err := put(yes)
	if err != nil || len(res.Untrusted) == 0 {
		return res, err
	}
	if !ask || !isTerminal() {
		fmt.Fprintf(os.Stderr, "pulse: %s has shell actions that aren't trusted yet; run `pulse trust` to allow them.\n", filepath.Base(path))
		return res, nil
	}
	fmt.Fprintf(os.Stderr, "\n%s defines shell actions for %q that run on this Mac:\n", filepath.Base(path), res.Job)
	for _, a := range res.Untrusted {
		fmt.Fprintf(os.Stderr, "  %-12s %s\n", a.Name, a.Shell)
	}
	if !askYes("Trust these commands?") {
		fmt.Fprintln(os.Stderr, "Not trusted; the other settings were applied. Run `pulse trust` later to allow them.")
		return res, nil
	}
	if res, err = put(true); err != nil {
		return res, err
	}
	fmt.Fprintln(os.Stderr, "Trusted. (Pulse asks again if they change.)")
	return res, nil
}

// loadConfig finds and parses the config for this folder (or -f).
func loadConfig(path string) (config.File, string, bool, error) {
	if path == "" {
		cwd, _ := os.Getwd()
		p, ok := config.Find(cwd)
		if !ok {
			return config.File{}, "", false, nil
		}
		path = p
	}
	f, err := config.Load(path)
	return f, f.Path, true, err
}

func cmdApply(args []string, trustMode bool) int {
	path, yes := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--file":
			if i+1 < len(args) {
				i++
				path = args[i]
			}
		case "--yes", "-y":
			yes = true
		case "--trust-mode":
		default:
			if path == "" && !strings.HasPrefix(args[i], "-") {
				path = args[i]
				continue
			}
			return printHelp(map[bool]string{true: "trust", false: "apply"}[trustMode]) + 2
		}
	}
	f, p, found, err := loadConfig(path)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("no pulse.yaml here (use -f <file>)"))
	}
	c, _, cancel, ok := admin()
	if !ok {
		return 1
	}
	cancel()
	for _, j := range f.Jobs {
		if j.Name == "" {
			return fail(fmt.Errorf("%s: set a job name (name: ...) to apply it", filepath.Base(p)))
		}
		res, err := sendConfig(c, j, p, true, yes)
		if err != nil {
			return fail(err)
		}
		msg := fmt.Sprintf("%s: %d alert(s), %d action(s)", res.Job, res.Alerts, res.Actions)
		if len(res.Untrusted) > 0 {
			msg += fmt.Sprintf(", %d shell action(s) not trusted", len(res.Untrusted))
		}
		fmt.Println(msg)
	}
	return 0
}

func cmdAction(args []string) int {
	if len(args) == 0 {
		return printHelp("action") + 2
	}
	job := args[0]
	rest := args[1:]
	a := config.Action{}
	var rm string
	var cmdArgs []string
	for i := 0; i < len(rest); i++ {
		next := func() string {
			if i+1 < len(rest) {
				i++
				return rest[i]
			}
			return ""
		}
		switch x := rest[i]; x {
		case "--":
			cmdArgs = rest[i+1:]
			i = len(rest)
		case "--send":
			a.Send = next()
		case "--open":
			a.Open = next()
		case "--confirm":
			a.Confirm = true
		case "--output":
			a.Output = true
		case "--when":
			a.When = next()
		case "--rm":
			rm = next()
		default:
			if strings.HasPrefix(x, "-") || a.Name != "" {
				return printHelp("action") + 2
			}
			a.Name = x
		}
	}
	if len(cmdArgs) > 0 {
		a.Shell = shellCommand(cmdArgs)
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	q := "?job=" + url.QueryEscape(job)
	switch {
	case rm != "":
		if err := c.Do(ctx, "DELETE", "/api/v1/actions/"+url.PathEscape(rm)+q, nil, nil); err != nil {
			return fail(err)
		}
	case a.Name != "":
		if err := c.Do(ctx, "POST", "/api/v1/actions"+q, a, nil); err != nil {
			return fail(err)
		}
	}
	var out struct {
		Actions []config.Action `json:"actions"`
	}
	if err := c.Do(ctx, "GET", "/api/v1/actions"+q, nil, &out); err != nil {
		return fail(err)
	}
	if jsonOut {
		return printJSON(out.Actions)
	}
	if len(out.Actions) == 0 {
		fmt.Printf("%s has no actions. Add one: pulse action %s <name> -- <command>\n", job, job)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tDOES\tFROM")
	for _, x := range out.Actions {
		from := "cli"
		if x.Source == "file" {
			from = "pulse.yaml"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", x.Name, x.Kind(), x.Shell+x.Send+x.Open, from)
	}
	tw.Flush()
	return 0
}

func cmdDo(args []string) int {
	if len(args) != 2 {
		return printHelp("do") + 2
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	run, err := resolveRun(ctx, c, args[0], false)
	if err != nil {
		return fail(err)
	}
	actx, acancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer acancel()
	var res struct {
		Kind  string `json:"kind"`
		URL   string `json:"url"`
		Sent  bool   `json:"sent"`
		Shell *struct {
			ExitCode int    `json:"exit_code"`
			Output   string `json:"output"`
			TimedOut bool   `json:"timed_out"`
		} `json:"shell"`
	}
	if err := c.WithTimeout(3*time.Minute).Do(actx, "POST", "/api/v1/runs/"+url.PathEscape(run.ID)+"/actions/"+url.PathEscape(args[1])+"?by=cli", nil, &res); err != nil {
		return fail(err)
	}
	switch {
	case res.Shell != nil:
		fmt.Print(res.Shell.Output)
		if res.Shell.TimedOut {
			fmt.Fprintln(os.Stderr, "pulse: timed out after 2 minutes")
		}
		return res.Shell.ExitCode
	case res.URL != "":
		if err := openBrowser(res.URL); err != nil {
			fmt.Println(res.URL)
		}
	case res.Sent:
		fmt.Printf("Sent %q to %s #%d.\n", args[1], run.Job, run.Seq)
	}
	return 0
}

var (
	runIDRe  = regexp.MustCompile(`^r_[0-9a-f]{12}$`)
	jobSeqRe = regexp.MustCompile(`^(.+)#([0-9]+)$`)
)

// shellCommand turns the words after "--" into one shell command. A single
// word is used as written (so "pulse action j x -- 'ls | wc -l'" keeps its
// pipe); several words are quoted one by one, so the shell sees exactly the
// arguments the user's own shell produced.
func shellCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>()*?[]{}~#!") {
			q[i] = a
		} else {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

type runList struct {
	Runs  []model.Run `json:"runs"`
	Total int         `json:"total"`
}

// resolveRun turns "job", "job#3" or a run id into a run. A job whose name
// itself contains "#" is matched first. With running=true only an
// unfinished run qualifies.
func resolveRun(ctx context.Context, c *client.Client, target string, running bool) (model.Run, error) {
	if runIDRe.MatchString(target) {
		var r model.Run
		if err := c.Do(ctx, "GET", "/api/v1/runs/"+url.PathEscape(target), nil, &r); err == nil {
			return r, nil
		}
	}
	pick := func(job string, seq int) (model.Run, bool, error) {
		q := "/api/v1/runs?job=" + url.QueryEscape(job)
		if seq > 0 {
			q += "&seq=" + strconv.Itoa(seq)
		}
		var out runList
		if err := c.Do(ctx, "GET", q, nil, &out); err != nil {
			return model.Run{}, false, err
		}
		for _, r := range out.Runs {
			if running && r.Status != model.Running && r.Status != model.Idle {
				continue
			}
			return r, true, nil
		}
		return model.Run{}, len(out.Runs) > 0, nil
	}
	r, exists, err := pick(target, 0)
	if err != nil {
		return r, err
	}
	if r.ID != "" {
		return r, nil
	}
	if !exists {
		if m := jobSeqRe.FindStringSubmatch(target); m != nil {
			seq, _ := strconv.Atoi(m[2])
			if r, found, err := pick(m[1], seq); err != nil || r.ID != "" {
				return r, err
			} else if found && running {
				return r, fmt.Errorf("%s isn't running", target)
			}
			return model.Run{}, fmt.Errorf("%s not found", target)
		}
		return model.Run{}, fmt.Errorf("no job named %q (see pulse ls)", target)
	}
	return model.Run{}, fmt.Errorf("%s has no running run", target)
}
