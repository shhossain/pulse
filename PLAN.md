# Pulse — Plan (v3: enterprise-grade bar)

A macOS menubar app (Go) that monitors any job — ML training, builds, deploys, scripts — with progress bars, metrics, charts, notifications and actions.

App: **Pulse** · CLI: `pulse`

---

## Enterprise-grade bar (added 2026-11-19)

Pulse is an enterprise-grade CLI tool. "Done" for any feature means:

| Area | Requirement | State today |
|---|---|---|
| CLI contract | Stable commands/flags; documented exit codes; `--help` on every command; `--json` on every read command; never breaks the caller's scripts | help ✅, `--json` on ls/show only, exit codes undocumented |
| HTTP API | Versioned (`/api/v1`), documented, backwards compatible | unversioned `/api/…` |
| Data | Versioned schema with migrations; retention + vacuum; export/backup | `CREATE IF NOT EXISTS` only; series grow forever |
| Security | Token scopes, rotation (`pulse token rotate`), audit log, trust for shell actions, no public exposure by default, Host/Origin guard | scopes/trust/guard ✅; no rotation; audit only in pulse.log |
| Reliability | Never lose a run's data on app restart/crash; bounded memory/disk; log rotation | restart-safe ✅; pulse.log unbounded |
| Quality | Unit + e2e tests, CI, the review-change skill on every milestone | unit tests ✅; e2e manual; no CI; M1–M4 reviewed retroactively |
| Docs | README, `pulse help <topic>`, config reference, API reference, shell completions, man page | per-command help ✅ |
| Distribution | Versioned builds (ldflags), Developer ID signing + notarization, Homebrew cask, Linux CLI binaries, update check | ad-hoc signing |
| Privacy | No telemetry; everything stays on the machine unless remote is enabled | ✅ |

## 0. Guiding principle: the user does almost nothing

> One command to start monitoring. Zero config. Zero code changes when possible.
> Pulse infers everything; config only overrides.

| The user does | Pulse does the heavy lifting |
|---|---|
| `pulse run -- python train.py` | Runs it in a pty (so output is live), parses tqdm / `epoch 3/10` / `loss=0.43` / JSON lines, tracks exit code, picks widgets, sets alerts, adds Stop |
| `pulse push train epoch=3/10 loss=0.4` | Auto-creates the job on first push; `3/10` → progress bar, numbers → metrics/charts |
| `pulse watch <github-actions-url>` | Polls the run, maps steps to progress |
| Nothing | Default alerts (done, failed, stale, NaN), sensible thresholds, sections in the popover |

Day-to-day, users need three commands: `run`, `push`, `watch`. Everything else is optional.

---

## 1. Architecture

**Two binaries**, so the CLI can run anywhere (e.g. a Linux GPU box):

| Binary | Contents | Build |
|---|---|---|
| `pulse` (CLI) | Pure Go: client, normalizer, stdout parser, pty runner. No cgo, no UI | Cross-compiled: darwin/linux × amd64/arm64 |
| `Pulse.app` (`pulse-app`) | Wails v3 menubar + HTTP server + store + engine + embedded web UI | macOS only (cgo, Cocoa/WebKit) |

Only `Pulse.app` opens the database.

```
            ┌───────────────────────── Pulse.app (one process) ───────────────────────────┐
pulse run ─►│ API (HTTP, SSE)  ──► Normalizer ──► Store (SQLite) ──► Engine ──► UI        │
pulse push ►│   push / heartbeat                   jobs, runs,        alerts     tray icon  │
helper    ─►│   command long-poll ◄─────────────── series, items      actions    popover    │
            │ Watchers (poll GitHub Actions)                          inbox      /r/<run>   │
            └─────────────────────────────────────────────────────────────────────────────┘
```

### Stack (decided)

| Part | Choice | Notes |
|---|---|---|
| Menubar | **Wails v3** (pin exact version, currently v3.1.0) — systray + attached window | Validated in M0. Fallback: `fyne.io/systray` + browser page |
| Popover content | The webview **loads `http://127.0.0.1:<port>/popover`** from Pulse's own server | Same origin as the full page → same components, same SSE, no CORS, no bindings |
| UI | Svelte + **uPlot**, built with Vite, embedded via `go:embed` | |
| Live updates | One SSE stream for all runs | Clear write deadline per stream via `http.ResponseController` |
| Storage | SQLite (`modernc.org/sqlite`), WAL mode, single writer goroutine, batched inserts | |
| Notifications | `UNUserNotificationCenter` from the signed `.app` | Needs bundle + signing → validated in M0 |
| Launch at login | `SMAppService` | Not a hand-written LaunchAgent |
| `pulse run` pty | `creack/pty` | |

Files (`~/Library/Application Support/Pulse/`): `pulse.db`, `admin.token` (0600), `push.token` (0600), `server.json` (actual port; default 7777, falls back if taken), `logs/<run>.log`.

---

## 2. Data model

```
Job (name, e.g. "train-gpt")          ← stable identity, holds config
 └── Run (id, #n, status, started/ended, cmd, cwd, host)   ← one execution
      ├── progress  {name: [current, total]}
      ├── series    (metric, x=step|ts, value)
      ├── events    (status changes, messages)
      ├── items     (approvals)
      └── commands  (pending → delivered → acked)
```

**Job naming (inferred):** `pulse run` uses the script name (`train.py` → `train`), `push` uses the given name, `watch` uses repo + workflow. If a run with that name is already active from a *different* command/cwd, the new job becomes `train#2`.

**When does a new Run start?**
- Every `pulse run` → new Run
- `pulse push` → new Run if: no active run, previous run reached `done/failed/lost`, `--new` / `run_id` given, or `step` went backwards
- Otherwise the push updates the active Run

**Statuses:** `running`, `done`, `failed`, `lost` (heartbeats stopped), `idle` (push job with no status and no recent updates), `needs_you` (open approval).

URLs are per run: `/r/<run-id>`; `/j/<job>` redirects to the latest run, with a run picker for comparing.

---

## 3. Getting data in

### 3.1 Normalizer — accept anything

| Input | Becomes |
|---|---|
| `epoch=3/10`, `"epoch": [3,10]` | progress `epoch` |
| `progress=0.62`, `progress=62%` | progress `main` |
| any number (`loss=0.431`) | metric + series |
| `status=` done/failed/running (+ synonyms: success, ok, complete, error, fail…) | status |
| `step=1200` | x-axis (else timestamp) |
| `message` / `msg` | message (other strings are ignored) |
| nested JSON | flattened (`train.loss`) |

**Denylist** (never charted/progress): `pid`, `port`, `code`, `id`, `*_id`, `date`, `time`, `year`, `version`, values that look like dates (`11/19`). The run page shows "detected fields" with a toggle to ignore any.

Canonical event:
```json
{ "job": "train", "run": "r_8f2c", "ts": 1763550000, "status": "running", "step": 1200,
  "progress": { "epoch": [3,10] }, "metrics": { "loss": 0.431 }, "message": "saving" }
```

### 3.2 `pulse run` — must never get in the user's way

- Child runs in a **pty** → line-buffered, keeps colors/tqdm; output mirrored to the terminal unchanged; `PYTHONUNBUFFERED=1` set
- Parser: strip ANSI → split on `\r` **and** `\n` (custom split func, large buffer) → rules below
- Child in its **own process group**; Ctrl-C / SIGTERM forwarded to the group; final status reported before exit
- Updates **coalesced** to ≤ 4/s; final state always sent
- **Heartbeat** every 10s; app marks run `lost` after 3 missed (terminal closed, CLI killed, machine asleep)
- **App not running:** CLI tries `open -g -b <bundle-id>` (macOS), buffers events in memory, retries with backoff, and degrades silently. Exit code of `pulse run` = exit code of the child, always
- Env given to child: `PULSE_URL`, `PULSE_TOKEN` (push scope), `PULSE_JOB`, `PULSE_RUN`
- If the helper library connects for this run, stdout metric parsing is turned off (no double reporting)
- Output saved to `logs/<run>.log`

Parser rules (first match wins):
1. `::pulse:: {json}` — explicit
2. JSON line
3. tqdm: `desc: 45%|███| 450/1000 [01:02<01:15, 7.2it/s]` → progress `desc` + rate/ETA (nested/rich bars: best-effort)
4. `epoch 3/10`, `step 120 of 1000`, `[3/10]` → progress named by keyword
5. `loss=0.43`, `loss: 0.43` → metrics
6. Exit code → `done` / `failed` (+ last stderr lines as message)

### 3.3 `pulse push`

`pulse push <name> k=v …` or `POST /api/push/<name>` with JSON/form. Short timeout (2s), never fails the caller's script (`--strict` to fail).

### 3.4 `pulse watch` (v1: GitHub Actions only)

`pulse watch https://github.com/<o>/<r>/actions/runs/<id>` — polls every 15s, steps → progress, conclusion → done/failed. Uses `gh auth token` if available. Other adapters later.

### 3.5 Python helper (`pulse`; PyPI name TBD, e.g. `pulse-jobs`)

```python
import pulse
pulse.init(link="https://wandb.ai/...")        # optional; reads PULSE_* env
for batch in pulse.track(loader, name="batch"):  # like tqdm
    pulse.log(loss=loss, lr=lr)

@pulse.on("stop")
def _(): trainer.should_stop = True

answer = pulse.ask("Deploy to prod?", ["approve", "reject"], timeout="30m")
```
- Only rank 0 reports (`RANK` / `LOCAL_RANK` / `SLURM_PROCID`)
- Background thread: batched pushes + command long-poll
- Never raises into user code

---

## 4. Config files (optional)

### 4.1 `pulse.yaml` next to the code

```yaml
name: train-gpt
run: python train.py --cfg big.yaml   # `pulse run` with no args runs this
link: https://wandb.ai/me/runs/abc
group: sweep-1

widgets:              # anything omitted is inferred
  progress: [epoch, batch]
  charts:
    - [loss, val_loss]
    - [lr]
  stats: [gpu_util]
  primary: loss
  ignore: [seed]

alerts:
  - val_loss new min
  - loss is nan: { run: stop }

actions:
  gpu: { shell: ssh gpu1 nvidia-smi }
```

- `pulse run` auto-discovers `pulse.yaml` in cwd (or `-f file.yaml`)
- `pulse apply -f jobs.yaml` registers push/watch jobs (one file may hold `jobs: [...]`)
- **One-way:** file → DB on `run` / `apply`. Pulse never writes back. UI marks file-defined settings as "from pulse.yaml"
- **Trust:** the first time a file defines `shell`/`http` actions, Pulse asks to trust it (like `direnv allow`); re-asks when the file changes. Widgets/alerts/links never need trust

### 4.2 Runtime config from the job

`pulse.init(...)` / push field `_config` may set **display, alerts, link only** — never shell/http actions.

**Precedence:** CLI flags > file / runtime config > inferred.

---

## 5. Display

### 5.1 Widget inference

| Data | Widget |
|---|---|
| value with a total | progress bar + ETA |
| number updated ≥ 5 times | chart (sparkline in popover) |
| other number | stat chip |
| shared stem (`loss`, `val_loss`) | one chart, multiple series |

Primary: first progress bar; `loss`-like chart or first chart; chips ranked pinned → most recently changed.

### 5.2 Menubar title — keep it short (notch hides wide items)
- Idle: icon only
- Running: progress ring icon + `62%` (pinned run, else most-progressed)
- Badge: a monochrome dot cut into the ring icon (SF Symbols `.badge` style) while something needs you. The icon stays a template image so macOS tints it; no coloured emoji

### 5.3 Popover (`/popover`, ~360px)
Sections, in order:
1. **Needs you** — approvals, failures not yet seen
2. **Running** — cards
3. **Finished (24h)** — compact rows; older runs auto-archived (still on the full page)

Card: status dot, name, elapsed, up to 3 progress bars ("+N"), up to 3 chips, 1 sparkline, 1–2 action buttons, `⋯`, **Open ↗**.

### 5.4 Run page `/r/<run>`
All bars, full charts (zoom, log scale, smoothing, multi-series, compare with previous runs), metrics table, log tail, timeline, actions, alerts, detected fields, external link.

---

## 6. Notifications

### v1 defaults (per job, can be turned off)
| Alert | Rule |
|---|---|
| Completed | run → `done`; includes duration + primary metrics |
| Failed | run → `failed`; includes last message/stderr |
| Lost | heartbeats stopped (`pulse run` / helper runs) |
| Stale | no update for `max(2m, 10 × median interval)` — only after ≥ 5 updates; at most once per episode |
| NaN | any metric becomes NaN/Inf |

Push jobs without a status never alert as stale — they go `idle`.

**Sleep/wake:** on a wall-clock jump (wake), reset all stale/lost timers before evaluating.

### v1 custom rules
`pulse alert <job> '<metric> <op> <value>'` · `'<metric> new min|max'` · `'progress >= 90%'` · `--run <action>`

Evaluation: on every event + 15s ticker. Fire once per crossing, with cooldown.

Delivery: levels (info/success/warn/error), notification buttons (**Open**, **Stop** if running, **Mute 1h**), respects Focus, history in popover.

---

## 7. Actions & items

### 7.1 Actions (you → job)

v1 built-ins:
- All runs: Open page, Open link, Copy run ID, Copy command, Mute, Archive
- `pulse run` / helper runs: **Stop** (SIGINT to group → SIGKILL after 10s), Kill
- GitHub watch: Cancel run, Open

v1 custom (needs trust if from a file):
```bash
pulse action train gpu -- ssh gpu1 nvidia-smi         # shell
pulse action train checkpoint --send save_checkpoint  # command to the job (helper)
```
Flags: `--confirm`, `--when 'status == running'`, `--show-output`.

**Command channel:** `GET /api/runs/<id>/commands?wait=30s` (long-poll by the CLI runner and helper). Commands have IDs, are idempotent, and are acked via `POST /api/runs/<id>/commands/<cid>/ack`. Stop for `pulse run` jobs is delivered to the runner, which signals the process group locally.

### 7.2 Items (job → you) — v1: approvals only
`pulse.ask(...)` / `pulse ask <job> "Deploy?" approve reject` → shown in **Needs you** + actionable notification. Answer via popover, notification, or `pulse answer <id> approve`. Optional timeout with default answer.

### 7.3 Security
- **Two token scopes:**
  - **push** token: create jobs/runs, push events, heartbeat, read own commands, create items. Safe to copy to remote machines
  - **admin** token: everything else (actions, answers, config, delete). Local only, used by the CLI on this Mac
- Browser/webview auth: app opens a one-time URL → sets a `SameSite=Strict`, `HttpOnly` cookie
- Reject requests whose `Host`/`Origin` isn't `127.0.0.1`/`localhost` (DNS rebinding, cross-site)
- Remote access off by default. `pulse remote enable` binds an extra listener to the **Tailscale IP only** (never `0.0.0.0`) that accepts push-scope only; `pulse remote` prints `export PULSE_URL=… PULSE_TOKEN=…` to paste on the remote box
- Shell/http actions defined only via local CLI or trusted file — never via pushed data
- Audit log of every action (who/what triggered, result)

---

## 8. CLI

```
# everyday
pulse run [-n name] [-f pulse.yaml] -- <cmd>
pulse push <name> k=v ... [--new] [--strict]
pulse watch <github-actions-url> [-n name]

# manage
pulse ls [--all]                     pulse open [job|run]
pulse stop <job|run>                 pulse rm <job>
pulse pin <job>                      pulse mute <job> [1h]
pulse alert <job> '<rule>' [--run a] pulse action <job> <label> ...
pulse do <job> <action>              pulse ask <job> "<q>" <opts...>
pulse inbox                          pulse answer <item> <value>
pulse apply -f <file>                pulse trust [file]
pulse remote [enable|disable]        pulse status   (is the app running? port? version)
```

---

## 9. HTTP API (127.0.0.1:<port>)

| Method & path | Scope | Purpose |
|---|---|---|
| `POST /api/push/<job>` | push | event (JSON/form); creates job/run if needed; returns `{run}` |
| `POST /api/runs/<id>/events` | push | `{job, meta, events[]}`. Run ids are made by the client, so the first batch creates the run and retries are safe. An empty `events` list is a heartbeat |
| `GET /api/runs/<id>/commands?wait=30s` | push | long-poll commands |
| `POST /api/runs/<id>/commands/<cid>/ack` | push | ack |
| `POST /api/items` | push | create approval |
| `GET /api/items/<id>?wait=30s` | push | wait for answer |
| `GET /api/jobs`, `GET /api/runs/<id>` | admin | state + layout |
| `GET /api/runs/<id>/series?keys=…&points=500` | admin | LTTB-downsampled series |
| `POST /api/runs/<id>/stop` | admin | stop |
| `POST /api/jobs/<job>/actions/<a>` | admin | run action |
| `POST /api/items/<id>/answer` | admin | answer |
| `PUT /api/jobs/<job>/config` | admin | apply config (alerts, actions, widgets, pin, mute) |
| `DELETE /api/jobs/<job>` | admin | remove |
| `GET /api/stream` | admin | SSE: runs, notifications, items |
| `GET /popover`, `/r/<run>`, `/j/<job>` | cookie | UI |

---

## 10. Repo layout

```
cmd/pulse/            CLI (pure Go, cross-compiled)
cmd/pulse-app/        Wails app entry (macOS)
internal/model/       Job, Run, Event, Widget, Alert, Action, Item
internal/normalize/   any input → Event (shared by CLI and app)
internal/parse/       stdout parser (shared)
internal/client/      HTTP client, offline buffer, discovery of server.json
internal/runner/      pty runner, process groups, heartbeat, command poll
internal/server/      HTTP API, auth, SSE
internal/store/       SQLite, series, LTTB, retention
internal/engine/      alerts, actions, commands, items, stale/lost ticker
internal/layout/      widget inference
internal/watch/       github adapter
internal/app/         Wails tray, popover window, notifications, login item
web/                  Svelte + uPlot (Popover, RunPage, Bar, Chip, Sparkline, Chart)
sdk/python/pulse/     helper
```

---

## 11. Milestones (risk first, value early)

| # | Milestone | Done when |
|---|---|---|
| **M0** ✅ | **Spike** (2–3 days): ad-hoc-signed `Pulse.app` (LSUIElement), Wails tray + attached window loading a local URL, a notification with an action button, CLI symlink | All four work on this Mac. If Wails tray fails → switch to fallback now |
| **M1** ✅ | Core loop: store, normalizer, parser, server (auth, push, runs, heartbeat), `pulse run` (pty) + `pulse push` + `pulse ls`, done/failed/lost notifications | `pulse run -- python train.py` → notification when it finishes or crashes |
| **M2** ✅ | Popover: sections, cards with bars + chips, title ring, SSE live | Glance at progress in the menubar |
| **M3** ✅ | Charts + run page: uPlot, LTTB, log tail, compare runs | Live loss curve |
| **M4** ✅ | Alerts: stale, NaN, thresholds, new min/max; Stop (popover + notification) | Get pinged when loss goes NaN, stop from the notification |
| **M5** ✅ | Config: `pulse.yaml`, `apply`, trust; actions (shell/send/open); feedback fixes from real use | Per-project setup in one file |
| **R** | Retroactive reviews of M1–M4 with the review-change skill; fix what holds | Findings logged and fixed or explained |
| **M6** ✅ | **Remote + watch** (moved up after real use). First move the API to `/api/v1` (no legacy aliases: nothing is published yet), because remote machines will hard-code URLs. Then: token-scoped push URLs, Tailscale-bound listener, `pulse remote`; `pulse watch --cmd/--url/--file` with JSON field mapping; GitHub Actions adapter | A cloud VM reports with one curl; an S3 progress file is watched without a poller |
| **M7** ✅ | Python helper + command channel + approvals (`pulse.ask`) | `pulse.ask()` approved from the menubar |
| **M8** | **Hardening** to the enterprise bar: schema migrations, retention, token rotation, audit log, log rotation, `--json` everywhere, exit-code contract, completions, README/docs, CI, e2e test suite | Every row of the bar table green except distribution |
| **M9** | Packaging: versioned builds, Developer ID signing, notarization, Homebrew cask (`binary` stanza for CLI), Linux CLI binaries, SMAppService login item | `brew install --cask pulse-app` |

**Later:** pid/docker/file/http watchers, WebSocket sources, pause/resume, rerun with captured env, HTTP actions, more item types, plateau/`for`/group rules, burst grouping, Slack/ntfy, daily digest.

---

## 12. Status & dev notes

- **M0 done (2026-11-19):** Wails v3.1.0 tray + attached window loading `http://127.0.0.1:<port>/popover`, notifications with action buttons, ad-hoc signed bundle, all verified on macOS 26.
- **M1 done (2026-11-19):** `pulse run` / `push` / `ls` / `status`; done/failed/lost notifications; offline buffering; tokens + Host/Origin guard. Verified end to end.
- Build: `./scripts/build-app.sh` → `build/Pulse.app` + `build/pulse`.
- Test without UI: `PULSE_HOME=/tmp/x pulse-app --headless --addr 127.0.0.1:7788`; notifications go to the log.
- `pulse run` jobs shorter than 10s finish silently (failures always notify).
- **M2 done (2026-11-19):** Svelte popover (`web/`, built into `web/dist`, embedded with `go:embed`), live over SSE (`/api/stream`); Needs you / Running / Finished; ETA from each bar's recent rate; Dismiss / Hide / Copy command; `pulse open` opens the popover in a browser via a one-time link.
- **M3 done (2026-11-19):** run page `/r/<id>` (opens in a native Pulse window from the popover, notifications, or `pulse open <job>`; `--browser` for the browser). uPlot charts grouped by stem (`loss` + `val_loss`), LTTB-downsampled, smoothing, log scale, drag-zoom, compare up to 3 earlier runs (dashed/dotted per run), live log tail (`pulse run` ships output, progress-bar redraws excluded), metrics table. Popover cards: sparkline of the headline metric + Open.
- Chart colours: Apple system blue/orange/purple/teal, validated per mode (dark: `#0a84ff #d97500 #bf5af2 #20a0b5`).
- Job naming: the `#2` suffix applies only to *inferred* names from a different folder/host; `-n name` and same-folder sweeps share the job.
- **M4 done (2026-11-19):** alert rules (`internal/alert`): thresholds, `new min/max`, `is nan`, `progress >= N%`, `eta < D`, `no update/progress for D`; built-in stale (rhythm-based) and NaN; edge-triggered with cooldowns (thresholds 10m, new best 10m); `--run stop`. Stop: long-poll command channel → runner sends SIGINT to the process group, SIGTERM at 10s, SIGKILL at 20s; new `stopped` status (no notification). Push runs can be marked stopped. Mute per job. Notification buttons: Open / Stop / Mute for 1 Hour. CLI: `pulse stop`, `pulse alert`, `pulse mute`. Run page: Alerts section (toggle, add, remove, mute) and two-step Stop; popover cards: hover Stop.
- `pulse run` restores default SIGINT for the child, so Stop/Ctrl-C work even when started in the background (`nohup pulse run … &`).
- Dev convenience: `~/.local/bin/pulse` → `build/pulse`.
- **M6 done:** `/api/v1` only; job-scoped push tokens (hash stored); remote listener on Tailscale/private addresses (push routes only, admin refused); `pulse watch` (cmd/url/file/GitHub) run by the app.
- **M7 done:** Python helper `sdk/python` (`pulse-jobs`, stdlib only): `log`, `progress`, `track`, `on`, `ask`; per-sender sequence numbers; commands delivered only to pollers that accept them; `helper_attached` turns off output parsing in `pulse run` (a line or two parsed before the helper connects can still count); persisted questions (`pulse ask`, `inbox`, `answer`, notification buttons, popover).
- Web UI: `cd web && pnpm install && pnpm run build` (the build script does this). Versions are pinned in `web/package.json`.

## 13. Risks

- **Wails v3 tray + window** — M0 validates; fallback ready
- **Notifications need a signed bundle** — M0 validates with ad-hoc signing; Developer ID in M8
- **Parser false positives** — conservative rules, denylist, detected-fields toggle, explicit `::pulse::`
- **Remote exposure** — push-scope only, Tailscale-bound listener, off by default
- **Retention** — finished runs kept 30 days (configurable); old series downsampled
