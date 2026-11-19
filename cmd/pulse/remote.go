package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type remoteView struct {
	Enabled    bool   `json:"enabled"`
	URL        string `json:"url"`
	Listen     string `json:"listen"`
	Tunnel     bool   `json:"tunnel"`
	Error      string `json:"error"`
	Candidates []struct {
		IP        string `json:"ip"`
		Interface string `json:"interface"`
		Tailscale bool   `json:"tailscale"`
	} `json:"candidates"`
}

func cmdRemote(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	c, ctx, cancel, ok := admin()
	if !ok {
		return 1
	}
	defer cancel()
	switch sub {
	case "", "status":
		var v remoteView
		if err := c.Do(ctx, "GET", "/api/v1/remote", nil, &v); err != nil {
			return fail(err)
		}
		if jsonOut {
			return printJSON(v)
		}
		printRemote(v)
	case "enable":
		addr, tunnel := "", ""
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--addr" && i+1 < len(args):
				i++
				addr = args[i]
			case args[i] == "--tunnel" && i+1 < len(args):
				i++
				tunnel = args[i]
			default:
				return printHelp("remote") + 2
			}
		}
		var v remoteView
		if err := c.Do(ctx, "POST", "/api/v1/remote", map[string]any{"enable": true, "addr": addr, "public_url": tunnel}, &v); err != nil {
			return fail(err)
		}
		printRemote(v)
	case "disable":
		if err := c.Do(ctx, "POST", "/api/v1/remote", map[string]any{"enable": false}, nil); err != nil {
			return fail(err)
		}
		fmt.Println("Remote access is off.")
	case "token":
		if len(args) < 1 {
			return printHelp("remote") + 2
		}
		job, name := args[0], ""
		if len(args) == 3 && args[1] == "--name" {
			name = args[2]
		} else if len(args) != 1 {
			return printHelp("remote") + 2
		}
		var out struct {
			Token string `json:"token"`
			Info  struct {
				ID  string `json:"id"`
				Job string `json:"job"`
			} `json:"info"`
		}
		if err := c.Do(ctx, "POST", "/api/v1/tokens", map[string]string{"job": job, "name": name}, &out); err != nil {
			return fail(err)
		}
		var v remoteView
		_ = c.Do(ctx, "GET", "/api/v1/remote", nil, &v)
		base := v.URL
		if base == "" {
			base = "http://<this-mac>:7777"
		}
		fmt.Printf("Token %s can only report job %q. It's shown once — store it like a password.\n\n", out.Info.ID, out.Info.Job)
		fmt.Printf("On the other machine:\n  export PULSE_URL=%s\n  export PULSE_TOKEN=%s\n", base, out.Token)
		if v.Tunnel {
			fmt.Println("  # if Cloudflare Access protects this URL, also its service token:")
			fmt.Println("  export PULSE_CF_ACCESS_CLIENT_ID=<id>.access  PULSE_CF_ACCESS_CLIENT_SECRET=<secret>")
		}
		fmt.Printf("  pulse run -n %s -- <command>        # or: pulse push %s progress=3/10\n\n", shellQuoteArg(out.Info.Job), shellQuoteArg(out.Info.Job))
		access := ""
		if v.Tunnel {
			access = "-H \"CF-Access-Client-Id: $PULSE_CF_ACCESS_CLIENT_ID\" -H \"CF-Access-Client-Secret: $PULSE_CF_ACCESS_CLIENT_SECRET\" \\\n       "
		}
		fmt.Printf("Without the pulse CLI:\n  curl -fsS -H \"Authorization: Bearer $PULSE_TOKEN\" %s-d status=running -d progress=3/10 \\\n       \"$PULSE_URL/api/v1/push/%s\"\n", access, url.PathEscape(out.Info.Job))
		if !v.Enabled {
			fmt.Println("\nRemote access is off. Turn it on with: pulse remote enable")
		}
	case "tokens":
		var out struct {
			Tokens []struct {
				ID       string     `json:"id"`
				Job      string     `json:"job"`
				Name     string     `json:"name"`
				Created  time.Time  `json:"created"`
				LastUsed *time.Time `json:"last_used"`
			} `json:"tokens"`
		}
		if err := c.Do(ctx, "GET", "/api/v1/tokens", nil, &out); err != nil {
			return fail(err)
		}
		if jsonOut {
			return printJSON(out.Tokens)
		}
		if len(out.Tokens) == 0 {
			fmt.Println("No job tokens. Create one: pulse remote token <job>")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tJOB\tNAME\tCREATED\tLAST USED")
		for _, t := range out.Tokens {
			used := "never"
			if t.LastUsed != nil {
				used = t.LastUsed.Local().Format("Jan 2 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Job, t.Name, t.Created.Local().Format("Jan 2 15:04"), used)
		}
		tw.Flush()
	case "rotate":
		if len(args) != 1 || (args[0] != "push" && args[0] != "admin") {
			return printHelp("remote") + 2
		}
		if err := c.Do(ctx, "POST", "/api/v1/tokens/rotate", map[string]string{"which": args[0]}, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("Rotated the %s token. Anything started with the old one must be restarted.\n", args[0])
	case "revoke":
		if len(args) != 1 {
			return printHelp("remote") + 2
		}
		if err := c.Do(ctx, "DELETE", "/api/v1/tokens/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("Revoked %s.\n", args[0])
	default:
		return printHelp("remote") + 2
	}
	return 0
}

func printRemote(v remoteView) {
	switch {
	case v.Enabled && v.Tunnel:
		fmt.Printf("Remote access is on through a tunnel: %s\n", v.URL)
		fmt.Printf("The tunnel must forward to http://%s (listening on loopback only).\n", v.Listen)
		fmt.Println("Other machines push with a job token: pulse remote token <job>")
	case v.Enabled:
		fmt.Printf("Remote access is on: %s\n", v.URL)
		fmt.Println("Other machines push with a job token: pulse remote token <job>")
	case v.Error != "":
		fmt.Printf("Remote access couldn't start: %s\n", v.Error)
	default:
		fmt.Println("Remote access is off.")
	}
	if len(v.Candidates) > 0 && !v.Enabled {
		fmt.Println("\nAddresses it can use:")
		for _, c := range v.Candidates {
			kind := "private network"
			if c.Tailscale {
				kind = "Tailscale (recommended)"
			}
			fmt.Printf("  %-16s %s, %s\n", c.IP, c.Interface, kind)
		}
		fmt.Println("\nTurn it on: pulse remote enable            (Tailscale address)")
		fmt.Println("        or: pulse remote enable --addr <ip>")
		fmt.Println("        or: pulse remote enable --tunnel https://<public-host>   (behind e.g. Cloudflare Tunnel)")
	} else if len(v.Candidates) == 0 && !v.Enabled {
		fmt.Println("No Tailscale or private-network address found. Pulse never listens on public addresses.")
	}
}

func shellQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"\\$`;&|<>()*?[]{}~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
