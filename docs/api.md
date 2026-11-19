# HTTP API (v1)

The app listens on `http://127.0.0.1:7777` (another free port if that's taken; see
`~/Library/Application Support/Pulse/server.json`). All routes are under `/api/v1`. Authenticate with
`Authorization: Bearer <token>`. Tokens never go in URLs.

| Token | Where | Can |
|---|---|---|
| admin (`admin.token`) | this Mac only | everything |
| push (`push.token`) | this Mac, or remote | report any job, receive its commands, ask questions |
| job token (`pulse remote token <job>`) | anywhere | the same, for one job only |

Requests with a non-loopback `Host` or a foreign `Origin` are refused. The optional remote listener
(`pulse remote enable`) binds only to a Tailscale or private-network address and serves only the
*push* routes below; the admin token is refused there.

## Push routes

| Method | Path | Body / query | Returns |
|---|---|---|---|
| GET | `/health` | | `{ok, version, api}` |
| POST | `/push/{job}` | JSON object or form (`key=value`); `?new=1` starts a new run | `{job, run}` |
| POST | `/runs/{id}/events` | `{job, sender, meta, events: [{…}], log: ["line"…]}`; empty `events` = heartbeat | `{job, run}` |
| GET | `/runs/{id}/commands` | `?wait=30s&accepts=stop,save_checkpoint` (long-poll) | `{commands: [{id, name}]}` |
| POST | `/runs/{id}/commands/{cid}/ack` | | `{ok}` |
| POST | `/runs/{id}/asks` | `{question, options[], default, timeout_s}` | the question |
| GET | `/asks/{id}` | `?wait=30s` (long-poll until answered) | the question |

Values in events: `status` (running, done, failed, stopped + synonyms), `progress` (`"6/147"`, `"62%"`,
`[6, 147]`; as text a whole number is a percent, `"1"` = 1%; a JSON number ≤ 1 is a fraction, `1` = 100%), `<name>` = `"3/10"` / `"45%"` (bars), `step`, `message`, numbers (metrics), other text
(fields), empty value or `null` (remove that key). `_ts` (unix seconds) and `_seq` (per sender,
for idempotent retries) are optional.

## Admin routes

`GET /runs[?job=&seq=&all=1]`, `GET /runs/{id}`, `GET /runs/{id}/series?key=…&points=[&x=<metric>]`
(with `x`: each point pairs a `key` value with `x`'s latest value at that step; `t` holds the step or seconds),
`GET /runs/{id}/log?tail=`, `POST /runs/{id}/stop|seen|archive`, `POST /runs/{id}/actions/{name}`,
`GET|POST /alerts?job=` (POST takes `expr`, `level`, `action`, `cooldown`, `once`, `ask`, and `answers` as `["Stop=stop", "Keep going"]`; GET returns answers as `{label, do}`), `PATCH|DELETE /alerts/{id}?job=`, `POST /mute?job=&for=`,
`GET /jobs`, `PUT /jobs/config`, `DELETE /jobs?job=`, `PUT /labels?job=` (`{"labels":{…},"units":{…}}`; an empty value removes one), `GET|POST /actions?job=`, `DELETE /actions/{name}?job=`,
`GET /asks`, `POST /asks/{id}/answer`, `GET|PUT|DELETE /watches`, `GET|POST /tokens`,
`DELETE /tokens/{id}`, `POST /tokens/rotate`, `GET|POST /remote`, `GET /stream` (SSE),
`POST /login-link`, `POST /ui/open`.
