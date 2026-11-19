# pulse.yaml

Optional per-project settings. Put `pulse.yaml` next to your code: `pulse run` picks it up, and plain
`pulse run` runs its `run:` command. `pulse apply -f <file>` loads one without running anything.
The file is the source of truth; Pulse never writes it.

```yaml
name: train-gpt                    # job name (default: guessed from the command)
run: python train.py --cfg big.yaml
link: https://wandb.ai/me/runs/abc # shown as a button

widgets:                           # anything left out is inferred
  progress: [epoch, batch]         # bar order
  charts:                          # explicit chart groups (max 4 series each)
    - [loss, val_loss]
    - { y: acc, style: points }    # line (default) | points | both
    - { x: lr, y: loss }           # scatter: loss against lr
  stats: [gpu_util]                # metrics shown first
  primary: loss                    # the chart on the popover card
  ignore: [seed]                   # never shown
  labels: { lr: Learning rate, tok_s: Throughput }  # display names
  units: { gpu_mem: GB, tok_s: tok/s }   # shown after the value as reported

alerts:
  - val_loss new min
  - loss is nan: { run: stop, level: error }
  - no update for 15m
  - progress >= 90%: { once: true }
  - "loss > 5": { ask: "Loss is spiking. Stop?", answers: [Stop=stop, Save first=action:checkpoint, Keep going] }

actions:
  gpu:        { shell: ssh gpu1 nvidia-smi, output: true }
  checkpoint: { send: save_checkpoint, when: running }
  wandb:      { open: "https://wandb.ai/me/runs/abc" }
```

Several jobs in one file:

```yaml
jobs:
  - name: deploy
    alerts: [no update for 10m]
  - name: backup
    alerts: ["duration_s > 3600"]
```

## Charts

Each entry in `charts:` is a list of metrics drawn together over steps (or time, when the job
logs no steps), or a map:

| Key | Meaning |
|---|---|
| `y` | a metric or a list of up to 4 |
| `x` | another metric to plot `y` against: a scatter. Each `y` value is paired with the latest `x` value at the same step (or moment). An `x` that spans two orders of magnitude or more, like a learning rate, is drawn on a log scale |
| `style` | `line` (default for charts over steps), `points` (default for scatters) or `both`, dots joined by a line in the order they happened |
| `name` | the chart's title |

A scatter is an extra view: its metrics keep their own charts over steps. Every chart also has a
line / dots / both switch; the choice is remembered in that browser.

## Labels and units

Keys are shown readably without any setup: `val_loss` → "Val loss", `gpu_util` → "GPU util",
`lr` → "Learning rate", `clip_pct` → "Clip %". `labels:` overrides a name and `units:` adds a
unit to values. The same can be set without a file: `pulse label <job> lr="Learning rate" --unit gpu_mem=GB`
(file values win).

## Alerts

| Rule | Fires when |
|---|---|
| `<metric> < 0.3` (also `>`, `<=`, `>=`, `==`, `!=`) | the value crosses the threshold (10 min cooldown) |
| `<bar> >= 5` | a progress bar's current value crosses it |
| `progress >= 90%` | the main bar's percentage crosses it |
| `<metric> new min` / `new max` / `new best` | a new best value (after a few values; 10 min cooldown) |
| `<metric> is nan`, `any is nan` | a value becomes NaN or ±Inf |
| `eta < 5m` | the estimate drops below |
| `no update for 15m` | nothing reported for that long |
| `no progress for 30m` | the main bar hasn't moved |

Options: `level` (info, warn, error), `run: stop`, `cooldown` (e.g. `30s`, `10m`; `0` = none), `once`.

`ask` turns the notification into a question, in the popover and the notification itself, while
the run is going. `answers` lists up to 6 buttons as `Label=what`, where *what* is `stop`,
`send:<command>` (sent to the job, for jobs that listen for commands), `action:<name>` or
nothing. Without `answers` the buttons are *Stop* and *Keep going*. The question isn't asked
again while one from the same rule is open.
Built in, can be turned off: *stale* (much quieter than the job's usual rhythm), *NaN*, and *disk*
(less than 2 GB free where a `pulse run` job runs).

## System metrics

`pulse run` records what the command uses every 5 seconds, as metrics of its own:

| Key | Shown as | What |
|---|---|---|
| `sys.cpu` | CPU, % | the command and everything it starts; 100% is one core |
| `sys.mem` | Memory, GB | resident memory of the same processes |
| `sys.gpu` | GPU, % | NVIDIA (via `nvidia-smi`): the GPUs the job's processes use; nothing while it isn't on one; all GPUs when the driver lists no processes at all (inside containers). Apple Silicon: the whole GPU |
| `sys.gpu_mem` | GPU memory, GB | NVIDIA: what the job's processes hold (else the GPUs' total use). Apple Silicon: GPU memory in use |
| `sys.disk_free` | Free disk, GB | free space on the disk holding the job's folder |

They're shown apart from the job's own numbers (a *System* row on the card and a *System* section
on the run page) and work in alerts like any metric (`pulse alert train 'sys.mem > 60'`).
`pulse run --no-system` turns them off. Samples don't count as the job reporting, so *stale* and
`no update for …` still catch a job that hangs with its process alive. Only processes the command
starts are counted: a job handed to another machine or a daemon (`docker run`, `ssh`, `srun`) shows
the client's small share; run `pulse` inside it instead.

## Actions

| Kind | Does |
|---|---|
| `shell: <command>` | runs on this Mac through your login shell, in the folder of `pulse.yaml`, with `PULSE_JOB`, `PULSE_RUN`, `PULSE_RUN_STATUS`, `PULSE_RUN_CWD`; 2 min timeout |
| `send: <name>` | sends a command to the running job (handled with `@pulse.on("<name>")` in Python) |
| `open: <url>` | opens a URL |

Options: `confirm` (ask first), `when` (`running` or `finished`), `output` (show the command's output).

Shell actions from a file need trust: the first time (and whenever the commands or the folder change)
Pulse asks; `pulse trust` reviews them.
