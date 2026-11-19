import atexit
import json
import math
import os
import platform
import re
import secrets
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

_FLUSH_EVERY = 0.25
_HEARTBEAT_EVERY = 10.0
_MAX_PENDING = 20000


def _rank_zero():
    for var in ("RANK", "LOCAL_RANK", "SLURM_PROCID", "OMPI_COMM_WORLD_RANK", "PMI_RANK"):
        v = os.environ.get(var)
        if v not in (None, "", "0"):
            return False
    return True


def _home():
    if os.environ.get("PULSE_HOME"):
        return os.environ["PULSE_HOME"]
    if sys.platform == "darwin":
        return os.path.expanduser("~/Library/Application Support/Pulse")
    return os.path.expanduser("~/.local/share/pulse")


def _discover():
    """(base_url, token) or (None, None)."""
    url, token = os.environ.get("PULSE_URL"), os.environ.get("PULSE_TOKEN")
    if url:
        host = urllib.parse.urlparse(url).hostname
        if not token and host in ("127.0.0.1", "localhost", "::1"):
            token = _local_token()  # this Mac's own app; never sent elsewhere
        return url.rstrip("/"), token
    try:
        with open(os.path.join(_home(), "server.json")) as f:
            url = json.load(f)["url"]
        return url.rstrip("/"), _local_token()
    except Exception:
        return None, None


def _local_token():
    try:
        with open(os.path.join(_home(), "push.token")) as f:
            return f.read().strip()
    except Exception:
        return None


def _number(v):
    """JSON-safe value: NaN/Inf as strings Pulse understands."""
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        if isinstance(v, float) and not math.isfinite(v):
            return "NaN" if math.isnan(v) else ("Infinity" if v > 0 else "-Infinity")
        return v
    try:  # numpy / torch scalars
        return _number(v.item())
    except Exception:
        return None


_UNITS = {"ms": 0.001, "s": 1, "sec": 1, "second": 1, "seconds": 1, "m": 60, "min": 60, "mins": 60,
          "minute": 60, "minutes": 60, "h": 3600, "hr": 3600, "hour": 3600, "hours": 3600,
          "d": 86400, "day": 86400, "days": 86400}


def _parse_duration(s):
    """Seconds from 90, "30s", "15m", "1h30m", "2 hours", "1d" (a bare number is seconds)."""
    if s is None:
        return 0.0
    if isinstance(s, (int, float)):
        return float(s)
    text = str(s).strip().lower()
    if re.fullmatch(r"\d+(\.\d+)?", text):
        return float(text)
    parts = re.findall(r"(\d+(?:\.\d+)?)\s*([a-z]+)", text)
    if not parts or re.sub(r"[\d.\s]+[a-z]+", "", text).strip() or any(u not in _UNITS for _, u in parts):
        raise ValueError("timeout like 30s, 15m, 1h30m or 2d, got %r" % s)
    return sum(float(n) * _UNITS[u] for n, u in parts)


class _Client:
    def __init__(self):
        self.enabled = _rank_zero()
        self.url, self.token = _discover()
        self.run = os.environ.get("PULSE_RUN") or ("r_" + secrets.token_hex(6))
        self.under_runner = bool(os.environ.get("PULSE_RUN"))
        self.job = os.environ.get("PULSE_JOB") or _guess_job()
        self.sender = "py-" + secrets.token_hex(4)
        # Under `pulse run` the runner heartbeats for the run (so it can be
        # stopped and lost); on its own the helper heartbeats itself.
        self.meta = {"source": "run" if self.under_runner else "push", "helper": True,
                     "cwd": os.getcwd(), "host": platform.node(), "pid": os.getpid(),
                     "heartbeat": True, "accepts": []}
        self.flush_lock = threading.Lock()
        self.finished = False
        self.handlers = {}
        self.lock = threading.Lock()
        self.pending = []
        self.seq = 0
        self.last_ok = 0.0
        self.warned = False
        self.started = False
        self.closed = False

    # -- public operations -------------------------------------------------

    def add(self, event):
        if not self.enabled or self.url is None:
            return
        with self.lock:
            self.seq += 1
            event["_seq"] = self.seq
            event.setdefault("_ts", time.time())
            self.pending.append(event)
            if len(self.pending) > _MAX_PENDING:
                del self.pending[: len(self.pending) - _MAX_PENDING]
        self._start()

    def _start(self):
        if self.started:
            return
        self.started = True
        threading.Thread(target=self._loop, name="pulse-sender", daemon=True).start()
        atexit.register(self.close)

    # -- transport -----------------------------------------------------------

    def _request(self, method, path, body=None, timeout=5.0):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.url + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None

    def _flush(self):
        with self.flush_lock:  # one flush at a time: the loop, ask() and exit all flush
            return self._flush_locked()

    def _flush_locked(self):
        with self.lock:
            batch = self.pending[:500]
        due = time.time() - self.last_ok >= _HEARTBEAT_EVERY
        if not batch and not due:
            return True
        body = {"job": self.job, "sender": self.sender, "meta": self.meta, "events": batch}
        try:
            self._request("POST", "/api/v1/runs/%s/events" % urllib.parse.quote(self.run), body)
        except urllib.error.HTTPError as e:
            if e.code == 401:  # token rotated: pick up the new one (this Mac's app)
                url, token = _discover()
                if url and token:
                    self.url, self.token = url, token
            elif 400 <= e.code < 500:  # rejected: don't retry forever
                with self.lock:
                    del self.pending[: len(batch)]
            self._warn("Pulse rejected an update (HTTP %d)" % e.code)
            return False
        except Exception as e:
            self._warn("can't reach Pulse (%s)" % e.__class__.__name__)
            return False
        with self.lock:
            del self.pending[: len(batch)]
        self.last_ok = time.time()
        return True

    def _loop(self):
        threading.Thread(target=self._commands, name="pulse-commands", daemon=True).start()
        backoff = _FLUSH_EVERY
        while not self.closed:
            ok = self._flush()
            backoff = _FLUSH_EVERY if ok else min(backoff * 2, 15.0)
            time.sleep(backoff)

    def _commands(self):
        while not self.closed:
            names = list(self.handlers)
            if not names:
                time.sleep(1)
                continue
            try:
                q = "/api/v1/runs/%s/commands?wait=30s&accepts=%s" % (
                    urllib.parse.quote(self.run), urllib.parse.quote(",".join(names)))
                out = self._request("GET", q, timeout=45) or {}
            except Exception:
                time.sleep(3)
                continue
            for cmd in out.get("commands") or []:
                fn = self.handlers.get(cmd.get("name"))
                if fn is not None:
                    try:
                        fn()
                    except Exception as e:  # never kill the job over a handler
                        print("pulse: handler %r failed: %s" % (cmd.get("name"), e), file=sys.stderr)
                try:
                    self._request("POST", "/api/v1/runs/%s/commands/%s/ack" % (
                        urllib.parse.quote(self.run), urllib.parse.quote(cmd["id"])), {})
                except Exception:
                    pass

    def _warn(self, msg):
        if not self.warned:
            self.warned = True
            print("pulse: %s; your program continues without it." % msg, file=sys.stderr)

    def close(self, final=None):
        if final:
            self.finished = True
            self.add(final)
        elif not self.under_runner and self.started and not self.finished:
            # The script ended without done()/fail(): a normal exit is done
            # (otherwise the run would go "lost").
            self.finished = True
            self.add({"status": "done"})
        deadline = time.time() + 3
        while self.pending and time.time() < deadline:
            if not self._flush():
                time.sleep(0.2)
        self.closed = True


def _guess_job():
    main = sys.modules.get("__main__")
    path = getattr(main, "__file__", None) if main else None
    if path:
        return os.path.splitext(os.path.basename(path))[0]
    return "python"


_client = None
_client_lock = threading.Lock()


def _c():
    global _client
    with _client_lock:
        if _client is None:
            _client = _Client()
        return _client


# -- public API ------------------------------------------------------------------


def init(job=None, link=None):
    """Optional: name the job (default: PULSE_JOB, else the script name)."""
    c = _c()
    if job:
        c.job = str(job)
    if link:
        c.add({"link": str(link)})
    if not c.under_runner:
        c.add({"status": "running"})


def log(step=None, **values):
    """Report numbers (metrics), "3/10" strings (bars) or text (fields)."""
    event = {}
    if step is not None:
        event["step"] = _number(step)
    for k, v in values.items():
        n = _number(v)
        event[str(k)] = n if n is not None else str(v)
    if event:
        _c().add(event)


def progress(name, current, total):
    """Set a progress bar: progress("epoch", 3, 10). Use name="main" for the headline bar."""
    key = "progress" if name in (None, "main") else "progress." + str(name)
    _c().add({key: [_number(current), _number(total)]})


def track(iterable, name="main", total=None, every=0.25):
    """Iterate while reporting progress (like tqdm); reports at most every `every` seconds."""
    if total is None:
        try:
            total = len(iterable)
        except TypeError:
            total = None
    last, i = 0.0, 0
    for i, item in enumerate(iterable, 1):
        yield item
        now = time.time()
        if total and (now - last >= every or i == total):
            progress(name, i, total)
            last = now
    if total is None and i:
        log(**{name: i})


_RESERVED = {"stop", "helper_attached"}


def on(name):
    """Register a handler for a command sent from Pulse (an action with `send: name`)."""
    if str(name) in _RESERVED:
        raise ValueError("%r is reserved for Pulse itself" % name)

    def register(fn):
        c = _c()
        c.handlers[str(name)] = fn
        if str(name) not in c.meta["accepts"]:
            c.meta["accepts"].append(str(name))
        c.add({})  # announce what this job accepts
        return fn
    return register


def ask(question, options=None, timeout=None, default=None):
    """Ask the user and wait for the answer (blocks). Returns the answer, or
    `default` if Pulse can't be reached or the question times out.

    In a distributed job only rank 0 asks; other ranks get `default`
    immediately. Share rank 0's answer yourself, e.g. with
    torch.distributed.broadcast_object_list."""
    # Mistakes in the call itself raise (they'd otherwise silently turn into
    # `default`); only Pulse being unreachable is swallowed.
    opts = [str(o).strip() for o in (options or [])]
    question = str(question).strip()
    if not question or len(question) > 500:
        raise ValueError("question must be 1-500 characters")
    if len(opts) > 6 or any(not o or len(o) > 40 for o in opts) or len(set(opts)) != len(opts):
        raise ValueError("up to 6 distinct options of 1-40 characters")
    if default is not None and opts and str(default).strip() not in opts:
        raise ValueError("default %r isn't one of the options" % (default,))
    timeout_s = _parse_duration(timeout)
    c = _c()
    if not c.enabled or c.url is None:
        return default
    c.add({})
    c._flush()  # make sure the run exists before asking
    body = {"question": question, "options": opts,
            "default": "" if default is None else str(default).strip(), "timeout_s": timeout_s}
    try:
        a = c._request("POST", "/api/v1/runs/%s/asks" % urllib.parse.quote(c.run), body)
    except urllib.error.HTTPError as e:
        print("pulse: the question was refused (HTTP %d); using the default" % e.code, file=sys.stderr)
        return default
    except Exception as e:
        c._warn("couldn't ask (%s)" % e)
        return default
    qid, unreachable_since = a["id"], None
    while True:
        try:
            a = c._request("GET", "/api/v1/asks/%s?wait=30s" % urllib.parse.quote(qid), timeout=45)
            unreachable_since = None
        except urllib.error.HTTPError:
            return default  # the question is gone (or access lost): don't block forever
        except Exception:
            unreachable_since = unreachable_since or time.time()
            if time.time() - unreachable_since > 300:
                c._warn("Pulse unreachable for 5 minutes while waiting for an answer")
                return default
            time.sleep(3)
            continue
        if a.get("answered_at"):
            ans = a.get("answer")
            if a.get("answered_by") in ("timeout", "run ended") and ans == "timeout":
                return default
            return ans


def _values(values):
    out = {}
    for k, v in values.items():
        n = _number(v)
        out[str(k)] = n if n is not None else str(v)
    return out


def done(**values):
    """Mark the run finished (only needed when not started with `pulse run`;
    a script that simply ends is marked done too)."""
    c = _c()
    if not c.under_runner:
        c.close(final=dict(_values(values), status="done"))


def fail(message="", **values):
    """Mark the run failed (only needed when not started with `pulse run`;
    an uncaught exception does this automatically)."""
    c = _c()
    if not c.under_runner:
        c.close(final=dict(_values(values), status="failed", message=str(message)))


def _excepthook(kind, value, tb, _previous=sys.excepthook):
    c = _client
    if c is not None and not c.under_runner and c.started and not c.finished:
        c.close(final={"status": "failed", "message": "%s: %s" % (kind.__name__, value)})
    _previous(kind, value, tb)


sys.excepthook = _excepthook
