import json
import math
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))


class FakePulse(BaseHTTPRequestHandler):
    events = []
    asks = {}

    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])) or b"{}")
        if self.path.endswith("/events"):
            FakePulse.events.append(body)
            return self._json({"job": body["job"], "run": "r"})
        if self.path.endswith("/asks"):
            FakePulse.asks[body["question"]] = body
            return self._json({"id": "q_1" if body["question"] != "Gone?" else "q_gone"})
        self._json({})

    def do_GET(self):
        if self.path.startswith("/api/v1/asks/q_1"):
            return self._json({"id": "q_1", "answer": "approve", "answered_at": "now"})
        if self.path.startswith("/api/v1/asks/"):
            return self._json({"error": "no such question"}, 404)
        self._json({"commands": []})


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), FakePulse)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        os.environ["PULSE_URL"] = "http://127.0.0.1:%d" % cls.srv.server_port
        os.environ["PULSE_TOKEN"] = "pls_test"
        for v in ("PULSE_RUN", "RANK", "LOCAL_RANK"):
            os.environ.pop(v, None)

    def setUp(self):
        import pulse._client as c
        c._client = None
        FakePulse.events.clear()

    def test_log_track_and_nan(self):
        import pulse
        import pulse._client as c
        pulse.init(job="t")
        for _ in pulse.track(range(4), name="batch", every=0):
            pass
        pulse.log(step=3, loss=float("nan"), phase="warmup", flag=True)
        c._client.close()
        events = [e for b in FakePulse.events for e in b["events"]]
        self.assertTrue(all(b["meta"]["helper"] for b in FakePulse.events))
        self.assertIn({"status": "running"}, [{k: v for k, v in e.items() if not k.startswith("_")} for e in events])
        self.assertIn([4, 4], [e.get("progress.batch") for e in events])
        logged = [e for e in events if "loss" in e][0]
        self.assertEqual(logged["loss"], "NaN")
        self.assertEqual(logged["phase"], "warmup")
        self.assertEqual(logged["flag"], "True")
        json.dumps(events)  # everything must be valid JSON
        seqs = [e["_seq"] for e in events]
        self.assertEqual(seqs, sorted(seqs))

    def test_ask_returns_answer(self):
        import pulse
        self.assertEqual(pulse.ask("Deploy?", ["approve", "reject"], timeout="5m"), "approve")
        self.assertEqual(FakePulse.asks["Deploy?"]["timeout_s"], 300)
        # A question that disappears (app restarted long ago) returns the default.
        self.assertEqual(pulse.ask("Gone?", ["a", "b"], default="b"), "b")

    def test_concurrent_flushes_lose_nothing(self):
        import pulse._client as c
        cl = c._Client()
        cl.started = True  # no background thread: drive flushes by hand
        for i in range(50):
            cl.add({"i": i})
        ts = [threading.Thread(target=cl._flush) for _ in range(4)]
        for t in ts:
            t.start()
        for i in range(50, 60):
            cl.add({"i": i})
        for t in ts:
            t.join()
        while cl.pending:
            cl._flush()
        sent = sorted(e["i"] for b in FakePulse.events for e in b["events"] if "i" in e)
        self.assertEqual(sent, list(range(60)))

    def test_plain_exit_is_done_and_stop_is_reserved(self):
        import pulse
        import pulse._client as c
        pulse.log(loss=1.0)
        c._client.close()  # what atexit does
        events = [e for b in FakePulse.events for e in b["events"]]
        self.assertEqual(events[-1].get("status"), "done")
        with self.assertRaises(ValueError):
            pulse.on("stop")

    def test_non_zero_rank_is_silent(self):
        import pulse
        import pulse._client as c
        os.environ["RANK"] = "1"
        try:
            c._client = None
            pulse.log(loss=1.0)
            self.assertEqual(c._client.pending, [])
            self.assertEqual(pulse.ask("x?", default="d"), "d")
        finally:
            del os.environ["RANK"]

    def test_unreachable_is_a_noop(self):
        import pulse
        import pulse._client as c
        os.environ["PULSE_URL"] = "http://127.0.0.1:9"
        try:
            c._client = None
            pulse.log(loss=1.0)
            self.assertEqual(pulse.ask("x?", ["a", "b"], default="b"), "b")
        finally:
            os.environ["PULSE_URL"] = "http://127.0.0.1:%d" % self.srv.server_port


class ParsingTest(unittest.TestCase):
    def test_durations(self):
        from pulse._client import _parse_duration as d
        self.assertEqual(d("1h30m"), 5400)
        self.assertEqual(d("1h 30m"), 5400)
        self.assertEqual(d("2 hours"), 7200)
        self.assertEqual(d("1d"), 86400)
        self.assertEqual(d(90), 90)
        with self.assertRaises(ValueError):
            d("soon")

    def test_bad_ask_arguments_raise(self):
        import pulse
        with self.assertRaises(ValueError):
            pulse.ask("Deploy?", ["yes", "no"], default="maybe")
        with self.assertRaises(ValueError):
            pulse.ask("x" * 501)
        with self.assertRaises(ValueError):
            pulse.ask("Deploy?", ["ok", "ok"])



class AccessHeadersTest(unittest.TestCase):
    def setUp(self):
        self.saved = {k: os.environ.get(k) for k in ("PULSE_CF_ACCESS_CLIENT_ID", "PULSE_CF_ACCESS_CLIENT_SECRET")}

    def tearDown(self):
        for k, v in self.saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def test_service_token_only_over_https_and_only_when_complete(self):
        import pulse._client as c
        os.environ["PULSE_CF_ACCESS_CLIENT_ID"] = "abc.access"
        os.environ["PULSE_CF_ACCESS_CLIENT_SECRET"] = "s3cret"
        self.assertEqual(c._access_headers("https://pulse.example.com"),
                         {"CF-Access-Client-Id": "abc.access", "CF-Access-Client-Secret": "s3cret"})
        self.assertEqual(c._access_headers("http://pulse.example.com"), {})
        os.environ["PULSE_CF_ACCESS_CLIENT_SECRET"] = ""
        self.assertEqual(c._access_headers("https://pulse.example.com"), {})



class _Scripted(BaseHTTPRequestHandler):
    """Answers every request with the class's (status, headers, body); records what it got."""
    reply = (200, {"Content-Type": "application/json"}, b"{}")
    seen = []

    def log_message(self, *a):
        pass

    def _answer(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        type(self).seen.append(dict(self.headers))
        code, headers, body = type(self).reply
        self.send_response(code)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = _answer


def _serve(handler):
    srv = HTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class ProxyErrorTest(unittest.TestCase):
    """Responses from something in front of Pulse must neither lose data nor leak tokens."""

    def setUp(self):
        self.saved = dict(os.environ)
        for k in ("PULSE_RUN", "RANK", "LOCAL_RANK", "PULSE_CF_ACCESS_CLIENT_ID", "PULSE_CF_ACCESS_CLIENT_SECRET"):
            os.environ.pop(k, None)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.saved)

    def _client_for(self, handler):
        import pulse._client as c
        srv = _serve(handler)
        self.addCleanup(srv.shutdown)
        os.environ["PULSE_URL"] = "http://127.0.0.1:%d" % srv.server_port
        os.environ["PULSE_TOKEN"] = "pls_test"
        c._client = None
        cl = c._c()
        cl.warned = True  # keep test output quiet
        cl.pending.append({"progress": "1/2"})
        return cl

    def test_proxy_refusals_keep_the_data(self):
        for reply in [(403, {"Content-Type": "text/html", "Cf-Ray": "8f00"}, b"<html>Forbidden</html>"),
                      (429, {"Content-Type": "text/html"}, b"slow down"),
                      (404, {"Content-Type": "text/plain"}, b"404 page not found")]:
            H = type("H", (_Scripted,), {"reply": reply, "seen": []})
            cl = self._client_for(H)
            self.assertFalse(cl._flush())
            self.assertEqual(len(cl.pending), 1, "dropped on %r" % (reply[:2],))

    def test_rate_limit_keeps_the_data(self):
        H = type("H", (_Scripted,), {"reply": (429, {"Content-Type": "application/json", "Retry-After": "1"},
                                               b'{"error":"over its request rate"}'), "seen": []})
        cl = self._client_for(H)
        self.assertFalse(cl._flush())
        self.assertEqual(len(cl.pending), 1)

    def test_pulse_refusal_drops_the_data(self):
        H = type("H", (_Scripted,), {"reply": (403, {"Content-Type": "application/json"}, b'{"error":"no"}'), "seen": []})
        cl = self._client_for(H)
        self.assertFalse(cl._flush())
        self.assertEqual(len(cl.pending), 0)

    def test_redirects_are_not_followed(self):
        # A login page in front of Pulse redirects elsewhere; the tokens must not follow.
        Elsewhere = type("Elsewhere", (_Scripted,), {"seen": []})
        other = _serve(Elsewhere)
        self.addCleanup(other.shutdown)
        loc = "http://127.0.0.1:%d/login" % other.server_port
        H = type("H", (_Scripted,), {"reply": (302, {"Location": loc}, b""), "seen": []})
        cl = self._client_for(H)
        self.assertFalse(cl._flush())
        self.assertEqual(Elsewhere.seen, [], "followed the redirect, sending the token along")
        self.assertEqual(len(cl.pending), 1)

    def test_sends_its_own_user_agent(self):
        # Cloudflare refuses Python's default "Python-urllib" User-Agent as a bot.
        H = type("H", (_Scripted,), {"seen": []})
        cl = self._client_for(H)
        self.assertTrue(cl._flush())
        ua = H.seen[0].get("User-Agent", "")
        self.assertTrue(ua.startswith("pulse-python/"), ua)

    def test_certificate_errors_say_how_to_fix_them(self):
        import urllib.error
        import pulse._client as c
        e = urllib.error.URLError("[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed")
        self.assertIn("Install Certificates", c._why(e))

    def test_values_are_trimmed(self):
        import pulse._client as c
        os.environ["PULSE_URL"] = "https://pulse.example.com\n"
        os.environ["PULSE_TOKEN"] = "pls_test\n"
        os.environ["PULSE_CF_ACCESS_CLIENT_ID"] = "abc.access\n"
        os.environ["PULSE_CF_ACCESS_CLIENT_SECRET"] = "s3cret\r\n"
        self.assertEqual(c._discover(), ("https://pulse.example.com", "pls_test"))
        self.assertEqual(c._access_headers("https://pulse.example.com")["CF-Access-Client-Secret"], "s3cret")

    def test_unusable_access_settings_are_reported(self):
        import pulse._client as c
        os.environ["PULSE_CF_ACCESS_CLIENT_ID"] = "abc.access"
        self.assertIn("both", c._access_problem("https://pulse.example.com"))
        os.environ["PULSE_CF_ACCESS_CLIENT_SECRET"] = "s3cret"
        self.assertIsNone(c._access_problem("http://pulse.example.com"))  # just not sent
        self.assertEqual(c._access_headers("http://pulse.example.com"), {})
        self.assertIsNone(c._access_problem("https://pulse.example.com"))


if __name__ == "__main__":
    unittest.main()
