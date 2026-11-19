package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"pulse/internal/hubctl"
)

// cmdHub connects job machines to a Pulse hub from this computer.
func cmdHub(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	flags := map[string][]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--cloudflare-token-stdin":
			flags[a] = []string{"1"}
		case strings.HasPrefix(a, "--") && i+1 < len(args):
			flags[a] = append(flags[a], args[i+1])
			i++
		case strings.HasPrefix(a, "--"):
			return printHelp("hub") + 2
		default:
			pos = append(pos, a)
		}
	}
	one := func(k string) string {
		if v := flags[k]; len(v) > 0 {
			return v[len(v)-1]
		}
		return ""
	}
	// Ctrl-C cancels: connect then undoes what it made instead of leaving it.
	sig, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithTimeout(sig, 3*time.Minute)
	defer cancel()

	switch sub {
	case "setup":
		c, _ := hubctl.LoadConfig() // update what's there
		if u := one("--url"); u != "" {
			c.URL = strings.TrimRight(u, "/")
		}
		if a := one("--admin"); a != "" {
			c.Admin = strings.Fields(a)
		}
		if cf := one("--cloudflare"); cf != "" {
			parts := strings.Split(cf, "/")
			if len(parts) != 3 {
				return fail(fmt.Errorf("--cloudflare is <account id>/<access app id>/<service-auth policy id>"))
			}
			c.Cloudflare = &hubctl.CloudflareConfig{Account: parts[0], App: parts[1], Policy: parts[2]}
		}
		if err := hubctl.SaveConfig(c); err != nil {
			return fail(err)
		}
		fmt.Printf("Hub: %s (config in %s)\n", c.URL, hubctl.ConfigFile())
		if one("--cloudflare-token-stdin") != "" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
			if err != nil {
				return fail(err)
			}
			where, err := hubctl.SaveCloudflareToken(hubctl.Exec, string(b))
			if err != nil {
				return fail(err)
			}
			fmt.Printf("Cloudflare API token saved in %s.\n", where)
		}
		return 0

	case "connect":
		if len(pos) != 1 || one("--name") == "" {
			return printHelp("hub") + 2
		}
		c, err := hubctl.LoadConfig()
		if err != nil {
			return fail(err)
		}
		var cf *hubctl.Cloudflare
		if c.Cloudflare != nil {
			if t := hubctl.LoadCloudflareToken(hubctl.Exec); t != "" {
				cf = &hubctl.Cloudflare{Token: t, Conf: *c.Cloudflare}
			}
		}
		dest := hubctl.Destination{EnvFile: one("--env-file"), GCPSecret: one("--gcp-secret"),
			GCPProject: one("--gcp-project"), GCPAccount: one("--gcp-account"), GrantTo: one("--grant")}
		if dest.EnvFile == "" && dest.GCPSecret == "" {
			dest.EnvFile = "pulse-" + one("--name") + ".env"
		}
		dest.Ready = hubctl.WaitReady(90 * time.Second)
		fmt.Fprintln(os.Stderr, "Creating the machine's tokens and checking the hub accepts them…")
		m, err := hubctl.Connect(ctx, c, cf, hubctl.Exec, pos[0], one("--name"), dest)
		var notReady hubctl.ReadyErr
		switch {
		case errors.As(err, &notReady):
			fmt.Fprintf(os.Stderr, "pulse: connected, but %v\n", notReady.Err)
		case err != nil:
			return fail(err)
		}
		fmt.Printf("Connected %s: it can report job %q to %s.\n\n", m.Name, m.Job, c.URL)
		fmt.Println("On the machine:")
		fmt.Println("  curl -fsSL https://raw.githubusercontent.com/shhossain/pulse/main/install.sh | sh")
		if strings.HasPrefix(m.Secret, "gcp:") {
			fmt.Printf("  eval \"$(gcloud secrets versions access latest --secret=%s --project=%s)\"\n", dest.GCPSecret, dest.GCPProject)
			if dest.GrantTo == "" {
				fmt.Printf("\n(The machine's service account needs roles/secretmanager.secretAccessor on %s: add --grant <sa> next time.)\n", dest.GCPSecret)
			}
		} else {
			fmt.Printf("  set -a; . ./%s; set +a     # after copying %s there (it's secret: chmod 600, don't commit it)\n", baseName(m.Secret), m.Secret)
		}
		fmt.Printf("  pulse run -n %s -- <command>\n\nCut it off any time: pulse hub disconnect %s\n", shellQuoteArg(m.Job), m.Name)
		return 0

	case "disconnect":
		if len(pos) != 1 {
			return printHelp("hub") + 2
		}
		c, err := hubctl.LoadConfig()
		if err != nil {
			return fail(err)
		}
		var cf *hubctl.Cloudflare
		if c.Cloudflare != nil {
			if t := hubctl.LoadCloudflareToken(hubctl.Exec); t != "" {
				cf = &hubctl.Cloudflare{Token: t, Conf: *c.Cloudflare}
			}
		}
		m, err := hubctl.Disconnect(ctx, c, cf, hubctl.Exec, pos[0])
		if err != nil {
			return fail(err)
		}
		fmt.Printf("Disconnected %s: its job token and Cloudflare token no longer work.\n", m.Name)
		if strings.HasPrefix(m.Secret, "gcp:") {
			fmt.Printf("Its secret is still in Secret Manager (%s); delete it if you're done with it.\n", strings.TrimPrefix(m.Secret, "gcp:"))
		}
		return 0

	case "list", "ls", "":
		c, err := hubctl.LoadConfig()
		if err != nil {
			return fail(err)
		}
		ms, err := hubctl.Machines()
		if err != nil {
			return fail(err)
		}
		if jsonOut {
			return printJSON(map[string]any{"hub": c.URL, "machines": ms})
		}
		fmt.Printf("Hub: %s\n", c.URL)
		if len(ms) == 0 {
			fmt.Println("No machines connected. Connect one: pulse hub connect <job> --name <machine>")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MACHINE\tJOB\tSECRET\tCONNECTED\tEXPIRES")
		var soon []string
		for _, m := range ms {
			exp := "-"
			if !m.CFExpires.IsZero() {
				exp = m.CFExpires.Local().Format("Jan 2 2006")
				if time.Until(m.CFExpires) < 30*24*time.Hour {
					soon = append(soon, m.Name)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.Job, m.Secret, m.Created.Local().Format("Jan 2 15:04"), exp)
		}
		tw.Flush()
		for _, n := range soon {
			fmt.Printf("\n%s's Cloudflare token expires within 30 days: pulse hub disconnect %s, then connect it again.\n", n, n)
		}
		return 0
	}
	return printHelp("hub") + 2
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
