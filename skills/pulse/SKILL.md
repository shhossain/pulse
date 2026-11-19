---
name: pulse
description: Monitor long-running jobs with the Pulse CLI so the user sees live progress, metrics and ETAs in their Mac menubar and gets notified when a job finishes, fails, stalls or needs a decision. Use this whenever you start, launch or babysit anything that runs longer than a minute or two (model training, fine-tuning, evals, builds, test suites, data pipelines, downloads, uploads, migrations, deploys, jobs on a cloud VM or CI), whenever the user asks to track, monitor, watch or "let me know when it's done", wants a progress bar or ETA, or mentions pulse, even if they don't name it. Also use it to check on jobs already running ("how's training going?").
---

# Pulse: monitor long-running jobs

Pulse is a menubar app plus a `pulse` CLI. Anything you report shows up as a progress ring in the menubar, a card in the popover and a page with live charts; the user gets a notification when the job finishes, fails, goes quiet or hits an alert. It runs locally: nothing leaves the Mac unless remote access is turned on.

The point of using it: the user can stop watching your terminal. Report jobs as soon as you start them, give them names a person will recognise, and finish them cleanly.

## First, check it's there

```bash
pulse status        # exit 0 = app running
pulse doctor        # what's wrong, if anything
```

If `pulse` isn't installed, mention it once and carry on without it; don't install it unasked. If the app isn't running, `pulse run` and `pulse push` start it in the background on a Mac, and never fail your command because of Pulse.

## Pick the right way to report

| The job is… | Use |
|---|---|
| a command you start on this machine | `pulse run -n <name> -- <command>` |
| a shell script or loop you write | `pulse push <name> key=value …` from inside it |
| Python code you write | the helper: `import pulse` (`pip install pulse-jobs`) |
| running somewhere you can't change (a VM writing a progress file, S3, CI) | `pulse watch` |
| on another machine that can run `pulse`/`curl` | a job token (`pulse remote token <name>`) |

### `pulse run` (most cases)

```bash
pulse run -n finetune-llama -- python train.py --epochs 10
nohup pulse run -n nightly-etl -- ./etl.sh > etl.log 2>&1 &     # backgrounded is fine
```

- No code changes: progress and metrics are read from the output (tqdm bars, `epoch 3/10`, `step 120/1000`, `loss=0.43`, `acc: 0.91`, Hugging Face Trainer dicts, `::pulse:: {"loss": 0.4}` lines).
- The exit code is the command's own, and the output is unchanged, so wrap commands freely.
- Always pass `-n`: a guessed name like `train` collides across projects.
- CPU, memory, GPU and free disk of the command are recorded automatically as `sys.cpu`, `sys.mem` (GB), `sys.gpu`, `sys.gpu_mem` (GB) and `sys.disk_free` (GB); read them with `pulse show <job> --json` to check a job isn't starved or swapping, and alert on them like any metric (`pulse alert train 'sys.gpu_mem > 70'`).
- A `pulse.yaml` next to the code adds charts, alerts and buttons; see "Project settings".

### `pulse push` (scripts)

```bash
pulse push deploy status=running progress=2/5 current="migrating db"
pulse push deploy progress=5/5 status=done
```

Reserved keys (run `pulse push --help` for the full list):

| Key | Meaning |
|---|---|
| `status=running\|done\|failed\|stopped` | lifecycle (also ok, success, error, cancelled, killed…) |
| `progress=6/147` or `progress=62%` | the main bar, shown first; as text `progress=1` means 1% |
| `<name>=3/10`, `<name>=45%` | an extra bar |
| `step=1200` | x-axis for charts |
| `message="…"` | a status line |
| `<name>=0.43` | a number: a metric, charted after 5 values |
| `<name>=text` | a text field (e.g. `current=file.mp4`) |
| `<name>=` | empty: remove that key from the run |

- End every pushed job with `status=done` or `status=failed`, or it goes "idle" and the user never gets the finished notification.
- `--new` starts a fresh run and closes the previous one.
- `pulse push` exits 0 even if Pulse is unreachable, so it can't break a script. Add `--strict` if you need to know.

### Python helper

```python
import pulse
for batch in pulse.track(loader, name="batch"):   # progress bar
    pulse.log(step=step, loss=loss, lr=lr)        # metrics
@pulse.on("save_checkpoint")                      # a button in Pulse can call this
def _(): trainer.save()
if pulse.ask("Push the model to the hub?", ["yes", "no"], timeout="30m", default="no") == "yes": push()
```

It never raises and never blocks training. Under `pulse run` it joins that run (no double counting). Only rank 0 of a distributed job reports. A script that ends normally is marked done, and an uncaught exception marks it failed.

### `pulse watch` (things that can't report themselves)

The app polls in the background, survives restarts, reports only changes, and skips half-written reads.

```bash
pulse watch ego --cmd 'aws s3 cp s3://bucket/run/progress.json -' --every 1m --map progress=done/total
pulse watch ego --cmd 'ssh gpu-box cat /runs/42/progress.json'
pulse watch build --file ./build.log                  # text logs work too (latest lines)
pulse watch https://github.com/org/repo/actions/runs/123456   # uses the user's gh login
pulse watch ls          # check it's healthy (errors show here)
pulse watch rm ego      # stop polling when the job is over
```

- `--cmd` must print one JSON object (or `epoch 3/10 loss=…` text) on stdout. Stderr is ignored. It runs in the folder you ran `pulse watch` from.
- `--map dest=path` renames a field (`gb=stats.gigabytes`), and `dest=a/b` makes a bar from two fields.

### Remote machines

On the Mac:

```bash
pulse remote enable                 # Tailscale/private address only
pulse remote token vm-train         # prints PULSE_URL / PULSE_TOKEN, shown once
```

On the remote machine, export those two variables, then use `pulse run`/`pulse push` there, or `curl -H "Authorization: Bearer $PULSE_TOKEN" -d progress=3/10 "$PULSE_URL/api/v1/push/vm-train"`. The token can only report that one job. Treat it as a secret: keep it out of logs, commits and chat transcripts.

**Cloud VMs and other machines that can't reach the Mac** (no Tailscale, the Mac asleep): use the user's always-on Pulse hub if they have one (a `pulse-hub` server behind a tunnel, see `deploy/hub/README.md`). Its admin connects the machine from their own computer with `pulse hub connect <job> --name <machine>` (creates its tokens, stores them in a 0600 env file or Secret Manager, prints what to run there; `pulse hub disconnect <machine>` cuts it off), or creates the job token by hand on the hub (`pulse-admin remote token <job>`); the job machine then needs `PULSE_URL=https://<hub-host>`, `PULSE_TOKEN`, and, when Cloudflare Access protects the hub, `PULSE_CF_ACCESS_CLIENT_ID` / `PULSE_CF_ACCESS_CLIENT_SECRET`. Install the CLI there with `curl -fsSL https://raw.githubusercontent.com/shhossain/pulse/main/install.sh | sh`. Then plain `pulse run -- <command>` on the VM records progress *and* the built-in `sys.*` metrics; there's no need to write your own CPU/memory reporting or poll a bucket with `pulse watch`. Ask the user for these values; never print or commit the secrets.

## Checking on jobs

```bash
pulse ls --json                 # running runs + latest of each job
pulse show <job> --json         # everything: progress (with ETA), metrics, fields, status, message
pulse show <job>#3              # a specific run
```

Useful fields in `show --json`: `status`, `progress[].cur/total/eta`, `metrics[].value`, `fields[]`, `message`, `exit_code`, `updated_at`. Prefer this over re-reading log files. When you're waiting, check every few minutes (use the ETA), not in a tight loop. The user gets notified anyway.

## Alerts, stop and questions

```bash
pulse alert train 'loss is nan' --run stop     # stop automatically on divergence
pulse alert train 'val_loss new min'
pulse alert etl 'no update for 15m'            # catches hung jobs (push and watch jobs too)
pulse alert train 'loss > 5' --ask "Loss is spiking. Stop?" --answer Stop=stop --answer "Keep going"
pulse stop train                               # Ctrl-C to the process group, then TERM/KILL
answer=$(pulse ask deploy "Deploy v2.3 to prod?" approve reject --timeout 30m)
```

Finished, failed and lost already notify, so add alerts only where they help. `--ask` makes an alert ask the user when it fires, and each `--answer Label=stop|send:<cmd>|action:<name>` does what it says when picked. `pulse ask` is for decisions the user must make while you're not in the conversation (unattended scripts). If you're talking to the user, just ask them. Exit codes: 0 answered, 3 timed out with no `--default`.

## Project settings (`pulse.yaml`)

```yaml
name: finetune-llama
run: python train.py --cfg big.yaml        # plain `pulse run` runs this
widgets:
  charts: [[loss, val_loss], { x: lr, y: loss }, { y: acc, style: points }]  # x: scatter; style: line|points|both
  primary: loss
  ignore: [seed]
  labels: { tok_s: Throughput }               # keys are humanised anyway (lr → Learning rate)
  units: { gpu_mem: GB }
alerts:
  - loss is nan: { run: stop }
  - val_loss new min
actions: { gpu: { shell: nvidia-smi, output: true } }
```

Without a file, `pulse label <job> key="Name" --unit key=GB` sets names and units.

Shell actions need the user's explicit trust (`pulse trust`). Don't trust them on the user's behalf.

## Good manners

- Name jobs for the user (`finetune-llama-7b`, not `job1`), and reuse the name for reruns so runs can be compared.
- Finish what you start: `status=done/failed` for pushed jobs, `pulse watch rm` when a watched job ends.
- Ask before `pulse rm` (it deletes history) and before `pulse remote enable`.
- Read commands take `--json`. Exit codes: 0 ok, 1 error, 2 usage, 3 timeout; `pulse run` exits with the command's own code.
