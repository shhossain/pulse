package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"pulse/internal/client"
	"pulse/internal/paths"
)

// cmdDoctor checks the setup and says what to fix. Exit 1 if anything failed.
func cmdDoctor() int {
	type check struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Note string `json:"note,omitempty"`
	}
	var checks []check
	add := func(name string, ok bool, note string) { checks = append(checks, check{name, ok, note}) }

	home := paths.Home()
	if st, err := os.Stat(home); err != nil {
		add("data folder", false, home+" doesn't exist yet: start the Pulse app once")
	} else {
		add("data folder", st.Mode().Perm()&0o077 == 0, fmt.Sprintf("%s (%v)", home, st.Mode().Perm()))
	}
	for _, f := range []string{paths.PushToken(), paths.AdminToken()} {
		st, err := os.Stat(f)
		switch {
		case err != nil:
			add(filepath.Base(f), false, "missing: start the Pulse app once")
		case st.Mode().Perm()&0o077 != 0:
			add(filepath.Base(f), false, fmt.Sprintf("readable by others (%v): chmod 600 %s", st.Mode().Perm(), f))
		default:
			add(filepath.Base(f), true, "private (0600)")
		}
	}
	c, err := client.Resolve(false)
	if err != nil {
		add("app", false, err.Error()+" (open Pulse.app)")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var h struct {
			Version string `json:"version"`
			API     int    `json:"api"`
		}
		err := c.Do(ctx, "GET", "/api/v1/health", nil, &h)
		cancel()
		switch {
		case err != nil:
			add("app", false, "not answering at "+c.BaseURL+" (open Pulse.app)")
		case h.API != 1:
			add("app", false, fmt.Sprintf("API version %d, this CLI speaks 1: update one of them", h.API))
		case h.Version != version && version != "dev":
			add("app", true, fmt.Sprintf("running %s at %s (CLI is %s)", h.Version, c.BaseURL, version))
		default:
			add("app", true, "running "+h.Version+" at "+c.BaseURL)
		}
	}
	if p, err := exec.LookPath("pulse"); err != nil {
		add("pulse on PATH", false, "add the pulse binary to your PATH")
	} else {
		add("pulse on PATH", true, p)
	}
	if u := os.Getenv("PULSE_URL"); u != "" {
		add("PULSE_URL", os.Getenv("PULSE_TOKEN") != "" || isLocalURL(u), u)
	}
	if runtime.GOOS != "darwin" {
		add("platform", true, "CLI only: report to a Mac with PULSE_URL and PULSE_TOKEN")
	}

	failed := 0
	for _, ch := range checks {
		if !ch.OK {
			failed++
		}
	}
	if jsonOut {
		printJSON(checks)
	} else {
		for _, ch := range checks {
			mark := "✓"
			if !ch.OK {
				mark = "✗"
			}
			fmt.Printf("%s %-14s %s\n", mark, ch.Name, ch.Note)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}
