# CLI reference

Generated from `pulse help <command>` by scripts/gen-cli-docs.sh.

```
Pulse: monitor jobs from your menubar.

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
```

## pulse run

```
usage: pulse run [-n name] [-f pulse.yaml] [-q] [--no-system] [-- <command> [args...]]

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
```

## pulse push

```
usage: pulse push <job> key=value ... [--new] [--strict]

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
```

## pulse watch

```
usage:
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
```

## pulse ls

```
usage: pulse ls [--all] [--json]

List jobs: every running run plus the latest run of each job (last 24h).
  --all    include older runs
  --json   machine-readable output (all fields)
```

## pulse show

```
usage: pulse show <job|run-id> [--json]

Everything about a job's latest run (or a specific run): status, every
progress bar with ETA, every metric and text field, command, and actions.
```

## pulse open

```
usage: pulse open [job] [--browser]

Open a job's page in a Pulse window (or the overview with no job).
  --browser   open in your web browser instead
```

## pulse stop

```
usage: pulse stop <job | job#3 | run-id>

Stop a running job. For "pulse run" jobs this sends Ctrl-C to the whole
process group (SIGTERM after 10s, SIGKILL after 20s). Jobs fed by
"pulse push" can't be signalled, so they're just marked stopped.
```

## pulse rm

```
usage: pulse rm <job> [--yes]

Delete a job and all its runs, metrics and logs from Pulse.
```

## pulse alert

```
usage:
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
```

## pulse mute

```
usage: pulse mute <job> [1h|30m|off]

Silence a job's notifications (default 1 hour).
```

## pulse action

```
usage:
  pulse action <job>                                        list actions
  pulse action <job> <name> [flags] -- <command...>         run a shell command (on this Mac)
  pulse action <job> <name> --send <command> [flags]        send a command (a name like save_checkpoint) to the
                                                            running job; it needs the Pulse helper in the job
  pulse action <job> <name> --open <url> [flags]            open a URL
  pulse action <job> --rm <name>

Flags: --confirm (ask first)  --when running|finished  --output (show output)

The command runs in the job's folder with PULSE_JOB, PULSE_RUN,
PULSE_RUN_STATUS and PULSE_RUN_CWD set.
```

## pulse do

```
usage: pulse do <job|run-id> <action>

Run one of a job's actions now (on its latest run).
```

## pulse apply

```
usage: pulse apply [-f pulse.yaml] [--yes]

Load job settings from a config file (default ./pulse.yaml): name, charts,
alerts, actions, link. Use a "jobs:" list to define several jobs. The file
is the source of truth; run apply again after editing it ("pulse run"
applies ./pulse.yaml automatically).

Shell actions run commands on this Mac, so the first time a file defines
them (and whenever they change) Pulse asks you to trust them.
  --yes   trust shell actions without asking
```

## pulse trust

```
usage: pulse trust [-f pulse.yaml] [--yes]

Review and allow the shell actions in a config file.
```

## pulse ask

```
usage: pulse ask <job> "<question>" [answer…] [--timeout 30m] [--default <answer>]

Ask a question in Pulse and wait for the answer, which is printed (for
scripts: answer=$(pulse ask deploy "Deploy v2.3?" approve reject)).
With no answers listed, any text is accepted. Exit status: 0 answered,
3 timed out with no --default, 1 error.
```

## pulse inbox

```
usage: pulse inbox [--json]

Questions jobs are waiting on (from pulse ask or the Python helper's pulse.ask).
```

## pulse answer

```
usage: pulse answer <id> <answer>

Answer a question from "pulse inbox".
```

## pulse remote

```
usage:
  pulse remote                         is remote access on? which addresses can it use?
  pulse remote enable [--addr <ip>]    accept pushes from other machines (default: this Mac's Tailscale address)
  pulse remote enable --tunnel <https-url> [--addr 127.0.0.1:<port>]
                                       accept them through a tunnel (e.g. Cloudflare Tunnel) that
                                       forwards <https-url> to a loopback port (default 127.0.0.1:7778)
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
```

## pulse skill

```
usage:
  pulse skill                      print the coding-agent skill (SKILL.md)
  pulse skill install              install it for Claude Code (~/.claude/skills/pulse)
  pulse skill install --project    install it into this project (.claude/skills/pulse)
  pulse skill install --dir <dir>  install it somewhere else (other agents)

The skill teaches coding agents (Claude Code, Codex, …) to report the jobs
they start through Pulse. It always matches this CLI's version.
```

## pulse status

```
usage: pulse status

Is the Pulse app running, and where is its data?
```
