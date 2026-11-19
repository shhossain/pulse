# Security model

Pulse runs on your Mac and keeps everything there. It has no accounts and sends no telemetry.

**Network.** The app listens on 127.0.0.1. Requests with a non-loopback `Host` (DNS rebinding) or a
foreign `Origin` (cross-site) are refused. The UI signs in with a one-time link that sets an HttpOnly,
SameSite=Strict cookie. Remote access is off by default; when on, it binds only to a Tailscale or
private-network address (never `0.0.0.0` or a public IP), serves only push routes, refuses the admin
token and refuses browsers.

**Hub behind a tunnel (opt-in).** `pulse-hub` runs the same engine without a UI on an always-on
server, for jobs that must report while your Mac sleeps (see [deploy/hub](../deploy/hub/README.md)).
`pulse remote enable --tunnel https://<host>` binds the push listener to loopback only and accepts
only an https public URL; a tunnel (cloudflared) is the only way in. This is the one setup where
Pulse is reachable from the internet, so it changes the trust model:

- *Two gates.* Cloudflare Access (a service token for job machines) refuses everything else at
  Cloudflare's edge; behind it, Pulse still requires a job token and still serves only the push
  routes. The admin API and UI are never routed through the tunnel; administer the hub over SSH.
- *Who can see data.* Cloudflare terminates TLS, so it sees job names, numbers and log lines in
  transit. The hub's provider can read what's stored on its disk. Don't send logs that hold secrets.
- *Reference deployment* (deploy/hub): no inbound ports or public IPv4 (SSH only through the cloud
  provider's identity-aware proxy), no cloud credentials on the VM, Pulse and cloudflared as separate
  unprivileged, sandboxed systemd services, automatic security updates.
- Clients never follow redirects and keep their data when a proxy (not Pulse) refuses a request, so
  an expired Access token delays reports instead of losing them.

**Web UI and phone notifications (opt-in, hub only).** `pulse remote web` opens a read-only UI
through the tunnel to the emails you list. Cloudflare Access signs people in, and Pulse verifies
Access's signed token on every request itself (signature against the team's published keys,
audience, issuer, expiry, email on the list), so a mistake in the Access policy alone lets no one in.
Viewers can look (runs, charts, output, questions) and manage their own notifications. Operators
(`--operator`) can also stop runs, answer questions (in the app, or with a question's buttons on the
notification) and dismiss runs; each is recorded under their verified email (`web.stop`,
`answered by web:<email>`). Nobody can run shell actions, change alerts or settings, or see tokens
from the web: an alert rule's answer that runs a shell action (`Yes=action:deploy`) is refused from
the web and gets no notification buttons; answer it on the hub. Dismissing and hiding are recorded
too (`web.dismiss`, `web.hide`). Stop is never a notification button, so a stray tap can't end a job.
Writes must come from the UI's own pages (`Origin`), pages carry a strict Content-Security-Policy,
and open streams end within 30 seconds of someone losing access. Only the app manifest and icons
(`/pwa/`) are public. Notifications are encrypted for each device (Web Push, RFC 8291), so the push
service relaying them can't read them; the hub only sends to known browser push services, and
drops devices those services reject. Viewers see everything jobs report, so list only people you'd
show your terminal. Access service tokens can be listed as viewers too (`--add-service`, used by
`pulse hub follow` so a Mac's menubar shows the hub's runs): they only ever view, and a hub's runs
shown on a Mac are never passed on through that Mac's own tunnel.

**A compromised job machine.** Assume any machine running a job can be taken over. With its job token
(and its Access service token) an attacker can report anything *about that one job*: fake progress,
mark it done, ignore Stop, ask questions in your name. They can't read anything back (there are no
read routes for job tokens), touch other jobs, change alerts or actions, run commands on the hub,
or reach your other machines: the hub never connects to job machines; they fetch their commands.
Per-token limits keep the damage inside that job. A job over a limit is slowed down, never cut off
from what keeps its runs honest (status, Stop, answers):

| Limit (per job token) | Default | Over it |
|---|---|---|
| data requests (events, pushes) | 50/s, bursts of 200 | HTTP 429 with `Retry-After` |
| control requests (waiting for Stop or answers, acks) | 10/s, bursts of 50, a separate budget | 429 |
| data sent, body and URL (everything stored) | 2 GB a rolling day | degraded: log lines dropped, 1 request/s of at most 16 KB |
| new runs | 120 an hour | 429 |
| questions | 20 an hour | 429; the asker waits for room rather than taking its default |
| notifications, per job (any source, questions aside) | 30 an hour | one "too many" notice, then held for the hour |

Limits apply to job tokens only; this machine's own tokens are trusted, and through a tunnel only
job tokens are accepted at all. The first time in an hour a job hits a limit, it's written to
`audit.log` (`limit.exceeded`) and you get a notification. Clients keep their data on a 429 and retry.
Job-supplied text (names, log lines, questions, notifications) is only ever shown as text, never as
HTML or a link.

If you suspect a job machine: revoke its job token (`pulse remote tokens`, `pulse remote revoke <id>`)
and, behind Cloudflare Access, its service token (Zero Trust → Access → Service auth); give each machine
its own so revoking one leaves the others working. Then delete what it sent if you don't trust it
(`pulse rm <job>`) and check `audit.log` for `limit.exceeded` and token use.

**Tokens.** `admin.token` and `push.token` live in the data folder with mode 0600. Job tokens can only
report one job; only their SHA-256 is stored and the value is shown once. `pulse remote rotate` replaces
the local tokens. The CLI never sends local tokens to a non-loopback `PULSE_URL`.

**Shell commands.** Only you can define commands Pulse runs on your Mac: `pulse action … -- <cmd>`
and `pulse watch --cmd` (admin token, local only), or `pulse.yaml` shell actions, which need explicit
trust per exact set of commands and folder. Reported data can never create or change a command.
Actions run through your login shell, in a folder that must exist, with a timeout and a process-group
kill.

**Data.** Text values with secret-looking keys (`token`, `password`, `api_key`, …) are not stored;
JSON lines read from program output contribute numbers only. Finished runs are deleted after 30 days.

**Audit.** `audit.log` (JSON lines) records shell actions run, trust granted, actions added, tokens
created/revoked/rotated, remote access changes, stops, deletions and answers. It never contains secrets.

Report security issues privately to the maintainers rather than in public issues.
