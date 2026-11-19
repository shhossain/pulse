"""Report progress, metrics and questions to the Pulse menubar app.

Every call is safe: if Pulse can't be reached, nothing happens and your
program carries on. Only rank 0 of a distributed job reports.
"""
from ._client import ask, done, fail, init, log, on, progress, track

__all__ = ["init", "log", "progress", "track", "on", "ask", "done", "fail"]
__version__ = "0.1.0"
