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


if __name__ == "__main__":
    unittest.main()
