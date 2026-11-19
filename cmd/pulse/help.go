package main

import (
	"fmt"
	"os"
)

// help holds per-command usage, shown by `pulse <cmd> --help` and `pulse help <cmd>`.
var help = map[string]string{
	"run": `usage: pulse run [-n name] [-f pulse.yaml] [-q] [--no-system] [-- <command> [args...]]

Run a command and monitor it. Output is shown unchanged; Pulse reads
progress bars (tqdm), "epoch 3/10", "loss=0.43", JSON lines and
"::pulse:: {json}" lines from it. Exit code is the command's own.

With no command, runs the "run:" command from ./pulse.yaml.
If ./pulse.yaml exists, its settings (name, charts, alerts, actions) apply.
Every few seconds Pulse also records what the command uses: CPU and memory
(with everything it starts), GPU load and memory (NVIDIA via nvidia-smi, or
Apple Silicon), and free disk space in the current folder.

  -n name       job name (default: from pulse.yaml, else guessed from the command)
  -f file       config file (default: ./pulse.yaml if present)
  -q            don't print Pulse's own notes
  --no-system   don't record CPU, memory, GPU and disk use
`,
	"push": `usage: pulse push <job> key=value ... [--new] [--strict]

Send an update for <job>; the job is created on first push.

Values:
  status=running|done|failed|stopped   lifecycle (also ok, success, error, cancelled…)
  progress=6/147  progress=62%          the main progress bar (always shown first)
  <name>=3/10     <name>=45%            any other progress bar
  step=1200                             x-axis for charts (step=5/147 is also a bar)
  message="…"                           a short status line
  <name>=0.43                           a number: a metric, charted after 5 values
  <name>=text                           other text: shown as a field (current=clip_042.mp4)
  <name>=                               empty: remove that bar, metric or field from the run

Flags:
  --new      start a new run; the previous unfinished one is closed as stopped
  --strict   exit 1 if Pulse can't be reached (default: warn, exit 0 — never breaks scripts)

Finish a run with status=done (or failed / stopped). A run that stops getting
updates goes "idle" after 10 minutes; for a notification, add an alert:
  pulse alert <job> 'no update for 15m'

HTTP (same thing, e.g. from another tool):
  curl -X POST -H "Authorization: Bearer $(cat ~/Library/Application\ Support/Pulse/push.token)" \
       -d status=running -d progress=6/147 http://127.0.0.1:7777/api/v1/push/<job>
`,
	"ls": `usage: pulse ls [--all] [--json [--hub]]

List jobs: every running run plus the latest run of each job (last 24h).
When this Mac follows a hub (pulse hub follow), its runs are listed too,
marked "(hub)"; act on them at the hub.
  --all    include older runs
  --json   machine-readable output (all fields): this machine's runs
  --hub    with --json, a followed hub's runs too (each with "hub": its URL)
`,
	"show": `usage: pulse show <job|run-id> [--json]

Everything about a job's latest run (or a specific run): status, every
progress bar with ETA, every metric and text field, command, and actions.
`,
	"stop": `usage: pulse stop <job | job#3 | run-id>

Stop a running job. For "pulse run" jobs this sends Ctrl-C to the whole
process group (SIGTERM after 10s, SIGKILL after 20s). Jobs fed by
"pulse push" can't be signalled, so they're just marked stopped.
`,
	"rm": `usage: pulse rm <job> [--yes]

Delete a job and all its runs, metrics and logs from Pulse.
`,
	"alert": `usage:
  pulse alert <job>                              list rules
  pulse alert <job> '<rule>' [--run stop] [--level info|warn|error] [--cooldown 10m] [--once]
  pulse alert <job> '<rule>' --ask "<question>" [--answer "Label=stop|send:<cmd>|action:<name>|none"]…
  pulse alert <job> --off <id> | --on <id> | --rm <id>

Rules:
  val_loss < 0.3          gpu_util < 0.2         epoch >= 5
  val_loss new min        accuracy new max       loss is nan
  progress >= 90%         eta < 5m               no update for 15m
  no progress for 30m

Built in (can be turned off): stale (no updates for much longer than usual)
and any metric becoming NaN/Inf. Finished / failed / lost always notify.

Ask instead of notify: when the rule fires, Pulse asks the question and the
answer does something. With no --answer: "Stop" (stops the run) and "Keep going".
  pulse alert train 'val_loss < 0.3' --ask "Good enough. Stop early?" \
      --answer "Stop now=stop" --answer "Save checkpoint=send:save_checkpoint" --answer "Keep going"
`,
	"label": `usage: pulse label <job> key="Name" … [--unit key=unit …]

Readable names and units for a job's values (bars, metrics, fields), shown in
the menubar, pages and notifications. Without one, keys are made readable
automatically (gpu_util → "GPU util", lr → "Learning rate"). An empty value
removes a name or unit. pulse.yaml can set them too (widgets.labels/units);
the file wins. The CLI and --json keep the raw keys.

  pulse label upload gb="Uploaded" clip_pct="Clips done" --unit gb=GB --unit hours=h
`,
	"mute": `usage: pulse mute <job> [1h|30m|off]

Silence a job's notifications (default 1 hour).
`,
	"open": `usage: pulse open [job] [--browser]

Open a job's page in a Pulse window (or the overview with no job).
  --browser   open in your web browser instead
`,
	"apply": `usage: pulse apply [-f pulse.yaml] [--yes]

Load job settings from a config file (default ./pulse.yaml): name, charts,
alerts, actions, link. Use a "jobs:" list to define several jobs. The file
is the source of truth; run apply again after editing it ("pulse run"
applies ./pulse.yaml automatically).

Shell actions run commands on this Mac, so the first time a file defines
them (and whenever they change) Pulse asks you to trust them.
  --yes   trust shell actions without asking
`,
	"trust": `usage: pulse trust [-f pulse.yaml] [--yes]

Review and allow the shell actions in a config file.
`,
	"action": `usage:
  pulse action <job>                                        list actions
  pulse action <job> <name> [flags] -- <command...>         run a shell command (on this Mac)
  pulse action <job> <name> --send <command> [flags]        send a command (a name like save_checkpoint) to the
                                                            running job; it needs the Pulse helper in the job
  pulse action <job> <name> --open <url> [flags]            open a URL
  pulse action <job> --rm <name>

Flags: --confirm (ask first)  --when running|finished  --output (show output)

The command runs in the job's folder with PULSE_JOB, PULSE_RUN,
PULSE_RUN_STATUS and PULSE_RUN_CWD set.
`,
	"do": `usage: pulse do <job|run-id> <action>

Run one of a job's actions now (on its latest run).
`,
	"remote": `usage:
  pulse remote                         is remote access on? which addresses can it use?
  pulse remote enable [--addr <ip>]    accept pushes from other machines (default: this Mac's Tailscale address)
  pulse remote enable --tunnel <https-url> [--addr 127.0.0.1:<port>]
                                       accept them through a tunnel (e.g. Cloudflare Tunnel) that
                                       forwards <https-url> to a loopback port (default 127.0.0.1:7778)
  pulse remote web --team <team>.cloudflareaccess.com --aud <tag> --email <a@…> [--operator <b@…>]
                                       open the web UI through the tunnel to these people, signed
                                       in by Cloudflare Access (checked by Pulse); --email views,
                                       --operator also stops runs and answers questions
  pulse remote web --add-service <id>.access | --remove-service <id>.access
                                       let an Access service token view (read-only), e.g. a Mac
                                       following this hub (pulse hub follow does this for you)
  pulse remote web off
  pulse remote disable
  pulse remote token <job> [--name x]  create a token that can only report <job> (shown once)
  pulse remote tokens                  list tokens (never their values)
  pulse remote revoke <id>
  pulse remote rotate push|admin       replace this Mac's own push or admin token

Remote machines can only report progress and receive Stop; the admin API,
the UI and shell actions stay local. Pulse only listens on Tailscale or
private-network addresses, never publicly; with --tunnel it listens on
loopback only and the tunnel decides who can reach it (put Cloudflare
Access in front). Tokens go in the Authorization header, never in URLs.
`,
	"hub": `usage:
  pulse hub setup --url https://<hub> --admin '<command that runs a shell command on the hub>'
                  [--cloudflare <account>/<access app>/<service-auth policy>] [--cloudflare-token-stdin]
  pulse hub connect <job> --name <machine> [--env-file <path>]
  pulse hub connect <job> --name <machine> --gcp-secret <name> --gcp-project <p> [--grant <vm service account>]
  pulse hub disconnect <machine>
  pulse hub list
  pulse hub follow | unfollow          show the hub's runs in this Mac's menubar (view only)

Connect a job machine to your always-on Pulse hub from this computer. connect
creates the machine's own job token on the hub (through --admin, e.g.
gcloud compute ssh … --command) and, when Cloudflare Access protects the hub,
its own Access service token; then it stores the four values in one secret: a
0600 env file (default ./pulse-<machine>.env) or a Google Secret Manager
secret, and prints the lines to run on the machine. Secrets never appear on
screen or in command lines. disconnect revokes both tokens.

The Cloudflare API token (Access: Service Tokens and Apps and Policies, Edit)
is read from stdin once and kept in the macOS Keychain (elsewhere a 0600 file),
or from PULSE_CLOUDFLARE_API_TOKEN.
`,
	"watch": `usage:
  pulse watch <job> --cmd '<command>' [--every 60s] [--map key=path ...]
  pulse watch <job> --url <http(s) url> [--every 60s] [--map ...]
  pulse watch <job> --file <path> [--every 60s] [--map ...]
  pulse watch <github-actions-run-url> [-n name]
  pulse watch ls
  pulse watch rm <job>

Pulse polls the source in the background (it keeps going after restarts)
and reports what it finds as <job>'s progress. The source should print a
JSON object; plain "epoch 3/10 loss=0.4" text works too. Only changes are
reported, and a read that isn't valid yet (a half-written file) is skipped.

  --map progress=stats.done/stats.total   combine two fields into a bar
  --map gb=transfer.gigabytes             rename a field

Examples:
  pulse watch ego --cmd 'aws s3 cp s3://bucket/run/progress.json -' --every 1m
  pulse watch ego --cmd 'ssh gpu1 cat /runs/42/progress.json'
  pulse watch https://github.com/acme/app/actions/runs/123456   (uses your gh login)

Commands run on this Mac, in the folder you ran "pulse watch" from.
`,
	"ask": `usage: pulse ask <job> "<question>" [answer…] [--timeout 30m] [--default <answer>]

Ask a question in Pulse and wait for the answer, which is printed (for
scripts: answer=$(pulse ask deploy "Deploy v2.3?" approve reject)).
With no answers listed, any text is accepted. Exit status: 0 answered,
3 timed out with no --default, 1 error.
`,
	"inbox": `usage: pulse inbox [--json]

Questions jobs are waiting on (from pulse ask or the Python helper's pulse.ask).
`,
	"answer": `usage: pulse answer <id> <answer>

Answer a question from "pulse inbox".
`,
	"skill": `usage:
  pulse skill                      print the coding-agent skill (SKILL.md)
  pulse skill install              install it for Claude Code (~/.claude/skills/pulse)
  pulse skill install --project    install it into this project (.claude/skills/pulse)
  pulse skill install --dir <dir>  install it somewhere else (other agents)

The skill teaches coding agents (Claude Code, Codex, …) to report the jobs
they start through Pulse. It always matches this CLI's version.
`,
	"status": `usage: pulse status

Is the Pulse app running, and where is its data?
`,
}

// wantsHelp reports whether args ask for help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func printHelp(cmd string) int {
	if h, ok := help[cmd]; ok {
		fmt.Print(h)
		return 0
	}
	fmt.Fprintf(os.Stderr, "pulse: no help for %q\n", cmd)
	return 2
}
