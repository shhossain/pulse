# Security model

Pulse runs on your Mac and keeps everything there. It has no accounts and sends no telemetry.

**Network.** The app listens on 127.0.0.1. Requests with a non-loopback `Host` (DNS rebinding) or a
foreign `Origin` (cross-site) are refused. The UI signs in with a one-time link that sets an HttpOnly,
SameSite=Strict cookie. Remote access is off by default; when on, it binds only to a Tailscale or
private-network address (never `0.0.0.0` or a public IP), serves only push routes, refuses the admin
token and refuses browsers.

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
