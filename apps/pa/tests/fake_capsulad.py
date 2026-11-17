"""A CPython stand-in for the injected ``capsulad`` MicroPython host module.

It mirrors the documented script-host surface the PA app uses
(docs/app_development/sdk.md, interpreter/micropython/sdk.py): the same names,
the same calling conventions and the same failure shape — ``recv_bytes``
returns ``None`` at the end of a stream and raises ``OSError(status, text)``
otherwise. It doubles the *host module*, not the framework or the app: each
test scripts the framework answers it wants and inspects what the app sent.
"""

import importlib.util
import os
import sys
import types

HERE = os.path.dirname(os.path.abspath(__file__))
APP_MAIN = os.path.join(os.path.dirname(HERE), "main.py")

STATUS_END_OF_STREAM = 9
STATUS_TIMEOUT = 10
STATUS_CANCELLED = 11
STATUS_BUSY = 13


class ScriptedCall:
    """One call handle whose answers a test scripts.

    ``byte_in`` items are returned in order: bytes for a message, ``None`` for
    the end of the stream, an exception instance to raise. An exhausted
    ``byte_in`` behaves like a real call with nothing pending: a receive
    timeout.
    """

    def __init__(self, info=None, byte_in=None, send_failures=None, terminal=None):
        self.info = info
        self.byte_in = list(byte_in or [])
        self.send_failures = list(send_failures or [])
        self.terminal = terminal
        self.sent_bytes = []
        self.ended = None
        self.cancelled = False
        self.half_closed = False

    def send_bytes(self, data):
        if self.send_failures:
            raise self.send_failures.pop(0)
        if not isinstance(data, (bytes, bytearray)):
            raise TypeError("expected bytes")
        self.sent_bytes.append(bytes(data))
        return self

    def recv_bytes(self, timeout=0):
        if self.byte_in:
            value = self.byte_in.pop(0)
            if isinstance(value, Exception):
                raise value
            return value
        raise OSError(STATUS_TIMEOUT, "recv timed out; call is still open")

    def half_close(self):
        self.half_closed = True
        return self

    def end(self, code=0, detail=""):
        self.ended = (code, detail)
        return self

    def cancel(self):
        self.cancelled = True
        return self

    def status(self):
        code, detail = self.terminal or (0, "")
        return {"code": code, "detail": detail}

    def call_info(self):
        return dict(self.info or {})


class Host:
    """The script host: records calls, answers ``capsulad.call`` and hands out
    scripted outbound calls from ``outbound``."""

    def __init__(self):
        self.env = {"appId": "org.capsulad.pa", "runId": "run_test", "sdk": "0.5"}
        self.logs = []
        self.handlers = {}
        self.opened = []  # (service, method, kind, timeout)
        self.calls = []  # (service, method, request, timeout)
        self.answers = {}  # unary method -> value or exception
        self.outbound = []  # ScriptedCall queue returned by open()
        self.started = False
        self.stopped = False
        self.runs = 0

    # --- the host module surface the app uses -----------------------------

    def log(self, message):
        self.logs.append(str(message))

    def on(self, kind, handler):
        self.handlers[kind] = handler

    def start(self, timeout=10000):
        self.started = True

    def run(self):
        self.runs += 1

    def stop(self):
        self.stopped = True

    def call(self, service, method, request=None, timeout=30000):
        self.calls.append((service, method, request, timeout))
        answer = self.answers.get(method, {})
        if isinstance(answer, Exception):
            raise answer
        return answer

    def open(self, service, method, kind=1, timeout=30000):
        self.opened.append((service, method, kind, timeout))
        if self.outbound:
            return self.outbound.pop(0)
        return ScriptedCall()

    def emit(self, kind, payload):
        handler = self.handlers.get(kind)
        if handler is not None:
            handler(payload)

    def texts(self):
        return "\n".join(self.logs)

    # --- plumbing ---------------------------------------------------------

    def install(self):
        module = types.ModuleType("capsulad")
        for name in ("log", "on", "start", "run", "stop", "call", "open", "env"):
            setattr(module, name, getattr(self, name))
        sys.modules["capsulad"] = module
        return module


def load_app(host=None, name="pa_main"):
    """Import the app script with `host` installed as the capsulad module."""
    host = host if host is not None else Host()
    sys.dont_write_bytecode = True
    host.install()
    spec = importlib.util.spec_from_file_location(name, APP_MAIN)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return host, module
