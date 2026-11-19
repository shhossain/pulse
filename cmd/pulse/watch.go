package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"pulse/internal/format"
)

func cmdWatch(args []string) int {
	if len(args) == 0 {
		return printHelp("watch") + 2
	}
	switch args[0] {
	case "ls", "list":
		return watchList()
	case "rm", "remove":
		if len(args) != 2 {
			return printHelp("watch") + 2
		}
		c, ctx, cancel, ok := admin()
		if !ok {
			return 1
		}
		defer cancel()
		if err := c.Do(ctx, "DELETE", "/api/v1/watches?job="+url.QueryEscape(args[1]), nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("Stopped watching %s.\n", args[1])
		return 0
	}

	spec := map[string]any{}
	mapping := map[string]string{}
	job := ""
	rest := args
	if strings.HasPrefix(args[0], "https://github.com/") { // pulse watch <run url> [-n name]
		spec["kind"], spec["source"] = "url", args[0]
		rest = args[1:]
	} else {
		job, rest = args[0], args[1:]
	}
	for i := 0; i < len(rest); i++ {
		next := func() (string, bool) {
			if i+1 < len(rest) {
				i++
				return rest[i], true
			}
			return "", false
		}
		flag := rest[i]
		v, ok := next()
		if !ok {
			return printHelp("watch") + 2
		}
		switch flag {
		case "--cmd":
			spec["kind"], spec["source"] = "cmd", v
		case "--url":
			spec["kind"], spec["source"] = "url", v
		case "--file":
			abs, err := filepath.Abs(v)
			if err != nil {
				return fail(err)
			}
			spec["kind"], spec["source"] = "file", abs
		case "--every":
			d, err := time.ParseDuration(v)
			if err != nil {
				return fail(fmt.Errorf("--every: use a duration like 30s or 5m"))
			}
			spec["every_s"] = d.Seconds()
		case "--map":
			k, path, ok := strings.Cut(v, "=")
			if !ok {
				return fail(fmt.Errorf("--map takes key=path, e.g. progress=stats.done/stats.total"))
			}
			mapping[k] = path
		case "-n", "--name":
			job = v
		default:
			return printHelp("watch") + 2
		}
	}
	if spec["kind"] == nil {
		return printHelp("watch") + 2
	}
	spec["job"] = job
	if len(mapping) > 0 {
		spec["map"] = mapping
	}
	if cwd, err := os.Getwd(); err == nil {
		spec["dir"] = cwd // commands run in the folder you set them up from
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	var out struct {
		Job string `json:"job"`
	}
	if err := c.Do(ctx, "PUT", "/api/v1/watches", spec, &out); err != nil {
		return fail(err)
	}
	fmt.Printf("Watching %s. Pulse polls it in the background (also after restarts).\n", out.Job)
	fmt.Println("Check on it: pulse watch ls   ·   stop: pulse watch rm " + shellQuoteArg(out.Job))
	return 0
}

func watchList() int {
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	var out struct {
		Watches []struct {
			Job       string     `json:"job"`
			Kind      string     `json:"kind"`
			Source    string     `json:"source"`
			EveryS    float64    `json:"every_s"`
			LastPoll  *time.Time `json:"last_poll"`
			LastPush  *time.Time `json:"last_push"`
			LastError string     `json:"last_error"`
			Failures  int        `json:"failures"`
		} `json:"watches"`
	}
	if err := c.Do(ctx, "GET", "/api/v1/watches", nil, &out); err != nil {
		return fail(err)
	}
	if jsonOut {
		return printJSON(out.Watches)
	}
	if len(out.Watches) == 0 {
		fmt.Println("Nothing is being watched. Example: pulse watch train --file ./progress.json")
		return 0
	}
	ago := func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return format.Duration(time.Since(*t)) + " ago"
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tSOURCE\tEVERY\tLAST CHANGE\tSTATUS")
	for _, w := range out.Watches {
		src := w.Kind + " " + w.Source
		if r := []rune(src); len(r) > 50 {
			src = string(r[:49]) + "…"
		}
		status := "ok"
		if w.LastError != "" {
			status = fmt.Sprintf("%s (%d in a row)", w.LastError, w.Failures)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", w.Job, src, time.Duration(w.EveryS*float64(time.Second)), ago(w.LastPush), status)
	}
	tw.Flush()
	return 0
}
