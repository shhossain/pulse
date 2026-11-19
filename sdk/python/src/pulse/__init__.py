"""Report progress, metrics and questions to the Pulse menubar app.

Every call is safe: if Pulse can't be reached, nothing happens and your
program carries on. Only rank 0 of a distributed job reports.
"""
__version__ = "0.1.0"  # before the import: _client sends it as its User-Agent

from ._client import ask, done, fail, init, log, on, progress, track  # noqa: E402

__all__ = ["init", "log", "progress", "track", "on", "ask", "done", "fail"]
