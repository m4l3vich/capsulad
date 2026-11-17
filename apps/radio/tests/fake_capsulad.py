"""A CPython stand-in for the injected ``capsulad`` MicroPython host module.

It mirrors the documented script-host surface (docs/app_development/sdk.md and
interpreter/micropython/sdk.py): the same names, the same calling conventions
and the same failure shape (``recv`` returns ``None`` at end of stream and
raises ``OSError`` with ``args[0] == 10`` on a receive timeout). It is a test
double for the *host module*, not a fake of the framework or of the app: each
test scripts the framework answers it wants and inspects what the app sent.
"""

WAIT_FOREVER = 0xFFFFFFFF
STATUS_TIMEOUT = 10
STATUS_END_OF_STREAM = 9


class ScriptedCall:
    """One call handle driven by a scripted test."""

    def __init__(self, json_messages=None, byte_messages=None, info=None):
        self.json_in = list(json_messages or [])
        self.byte_in = list(byte_messages or [])
        self.sent = []
        self.sent_bytes = []
        self.info = info
        self.role = None
        self.fail_send = False
        self.fail_frame = False
        self.ended = None
        self.cancelled = False
        self.half_closed = False

    def send(self, value):
        if self.half_closed:
            raise OSError(4, "call is closed")
        if self.fail_send:
            raise OSError(7, "transport failure")
        if self.fail_frame and isinstance(value, dict) and "frame" in value:
            raise OSError(7, "transport failure")
        self.sent.append(value)
        return self

    def send_bytes(self, data):
        if not isinstance(data, (bytes, bytearray)):
            raise TypeError("expected bytes")
        if self.half_closed:
            # The native call's request stream is closed by half_close, so a
            # later control/chunk is rejected rather than delivered.
            raise OSError(4, "call is closed")
        self.sent_bytes.append(bytes(data))
        return self

    def half_close(self):
        self.half_closed = True
        return self

    def recv(self, timeout=0):
        if self.json_in:
            value = self.json_in.pop(0)
            if value is None:
                raise OSError(STATUS_END_OF_STREAM, "end of stream")
            return value
        raise OSError(STATUS_TIMEOUT, "timed out")

    def recv_bytes(self, timeout=0):
        if self.byte_in:
            value = self.byte_in.pop(0)
            if value is None:
                raise OSError(STATUS_END_OF_STREAM, "end of stream")
            return value
        raise OSError(STATUS_TIMEOUT, "timed out")

    def end(self, code=0, detail=""):
        self.ended = (code, detail)
        return self

    def cancel(self):
        self.cancelled = True
        return self

    def status(self):
        return {"code": 0, "detail": ""}

    def call_info(self):
        return dict(self.info or {})


class TunerPeer(ScriptedCall):
    """A minimal, honest stand-in for a conforming tuner: it echoes the
    session id it was asked to tune and answers with a scripted verdict."""

    def __init__(self, accepted=True, detail="", content_type="audio/mpeg"):
        ScriptedCall.__init__(self)
        self.accepted = accepted
        self.reason = detail
        self.content_type = content_type
        self.tune_requests = []

    def recv_bytes(self, timeout=0):
        from pb import parse, tune_result

        request = self.sent_bytes[-1] if self.sent_bytes else b""
        fields = parse(request)
        session = fields.get(1, b"")
        if isinstance(session, bytes):
            session = session.decode()
        self.tune_requests.append(fields)
        return tune_result(session, self.accepted, self.reason, self.content_type)


class Host:
    def __init__(self):
        self.reset()

    def reset(self):
        self.env = self.env if hasattr(self, "env") else {}
        self.env.clear()
        self.env.update({
            "socket": "/tmp/test.sock",
            "runId": "run_test",
            "appId": "org.capsulad.radio",
            "sdk": "0.5",
            "library": "/tmp/libcapsulad.dylib",
            "appDir": "/app",
        })
        self.handlers = {}
        self.json_answers = {}
        self.open_answers = {}
        self.routed_answers = {}
        self.calls = []
        self.opens = []
        self.routed_calls = []
        self.notifications = []
        self.logs = []
        self.live = []
        self.timers = []
        self.timer_seq = 0
        self.now_ms = 0
        self.started = False
        self.ran = False
        self.stopped = False
        self.result = {"status": 0, "detail": ""}

    # --- scripting ---------------------------------------------------------

    def answer(self, service, method, value):
        self.json_answers[(service, method)] = value

    def answer_error(self, service, method, error):
        self.json_answers[(service, method)] = error

    def answer_open(self, service, method, *calls):
        self.open_answers[(service, method)] = list(calls)

    def answer_routed(self, app_id, service, method, *calls):
        self.routed_answers[(app_id, service, method)] = list(calls)

    # --- clock and repeating callbacks -------------------------------------

    def monotonic_ms(self):
        return self.now_ms

    MAX_TIMERS = 16

    def every(self, interval_ms, fn):
        if not callable(fn):
            raise TypeError("timer handler must be callable")
        interval = int(interval_ms)
        if interval < 1:
            raise ValueError("interval_ms must be at least 1")
        if len(self.timers) >= self.MAX_TIMERS:
            raise ValueError("at most %d repeating callbacks" % self.MAX_TIMERS)
        self.timer_seq += 1
        token = self.timer_seq
        self.timers.append([token, interval, fn, self.now_ms + interval])
        return token

    def cancel_timer(self, token):
        for index in range(len(self.timers)):
            if self.timers[index][0] == token:
                del self.timers[index]
                return True
        return False

    def advance(self, ms):
        """Test helper: move the fake clock and fire every timer tick due."""
        target = self.now_ms + int(ms)
        while True:
            due = [timer for timer in self.timers if timer[3] <= target]
            if not due:
                break
            timer = due[0]
            for candidate in due:
                if candidate[3] < timer[3]:
                    timer = candidate
            self.now_ms = timer[3]
            timer[3] = self.now_ms + timer[1]
            timer[2]()
        self.now_ms = target

    def open_calls(self, service, method):
        return [item for item in self.opens if item[0] == service and item[1] == method]

    # --- script-host surface ----------------------------------------------

    def log(self, message):
        self.logs.append(str(message))

    def start(self, timeout=10000):
        self.started = True

    def run(self):
        self.ran = True
        return dict(self.result)

    def stop(self):
        self.stopped = True

    def on(self, kind, fn):
        if not callable(fn):
            raise TypeError("handler must be callable")
        self.handlers[kind] = fn

    def call(self, service, method, request=None, timeout=30000):
        self.calls.append((service, method, request, timeout))
        answer = self.json_answers.get((service, method))
        if answer is None:
            raise RuntimeError("no scripted answer for %s/%s" % (service, method))
        if isinstance(answer, BaseException) or (isinstance(answer, type)
                                                 and issubclass(answer, BaseException)):
            raise answer
        if callable(answer):
            return answer(request)
        return answer

    def open(self, service, method, kind=1, timeout=30000):
        self.opens.append((service, method, kind, timeout))
        scripted = self.open_answers.get((service, method))
        if not scripted:
            raise RuntimeError("no scripted open for %s/%s" % (service, method))
        item = scripted.pop(0)
        if isinstance(item, BaseException):
            raise item
        if callable(item) and not isinstance(item, ScriptedCall):
            item = item(kind, timeout)
        item.role = (service, method)
        self.live.append(item)
        return item

    def open_routed(self, app_id, service, method, timeout=30000):
        key = (app_id, service, method)
        self.routed_calls.append((app_id, service, method, timeout))
        scripted = self.routed_answers.get(key)
        if not scripted:
            raise RuntimeError("no scripted routed call for %s" % (key,))
        item = scripted.pop(0)
        if isinstance(item, BaseException):
            raise item
        item.role = (app_id, service, method)
        self.live.append(item)
        return item

    def notify_invocation(self, invocation_id, accepted, detail=""):
        self.notifications.append(("invocation", invocation_id, bool(accepted), detail))

    def notify_action(self, action_invocation_id, succeeded, detail=""):
        self.notifications.append(("action", action_invocation_id, bool(succeeded), detail))

    def register_descriptors(self, data):
        self.logs.append("register_descriptors(%d bytes)" % len(data))

    def info(self):
        return {"app_id": self.env["appId"]}

    def handle_info(self, handle):
        return {"kind": "call"}

    def state(self):
        return {"ready": self.started, "connected": self.started,
                "disconnected": False, "status": 0, "detail": ""}

    def ready(self):
        return self.started

    def connected(self):
        return self.started

    def disconnected(self):
        return False

    def emit(self, kind, event):
        handler = self.handlers.get(kind)
        if handler is None:
            raise AssertionError("no handler registered for " + kind)
        return handler(event)

    def inbound(self, service, method, byte_messages=None, json_messages=None,
                caller_run_id="run_peer"):
        call = ScriptedCall(json_messages=json_messages, byte_messages=byte_messages,
                            info={"call_id": "call_1", "caller_run_id": caller_run_id,
                                  "grpc_service": service, "rpc_name": method,
                                  "deadline_monotonic_ns": 0})
        handler = self.handlers.get("call")
        if handler is None:
            raise AssertionError("no call handler registered")
        handler(call)
        return call


HOST = Host()


def install():
    """Publish this module as ``capsulad`` for the app under test."""
    import sys

    sys.modules["capsulad"] = sys.modules[__name__]
    return HOST


# The public names the app imports (mirroring interpreter/micropython/sdk.py).
env = HOST.env


def log(message):
    HOST.log(message)


def start(timeout=10000):
    HOST.start(timeout)


def run():
    return HOST.run()


def stop():
    HOST.stop()


def on(kind, fn):
    HOST.on(kind, fn)


def call(service, method, request=None, timeout=30000):
    return HOST.call(service, method, request, timeout)


def call_bytes(service, method, request, timeout=30000):
    return HOST.call(service, method, request, timeout)


def open(service, method, kind=1, timeout=30000):
    return HOST.open(service, method, kind, timeout)


def open_routed(app_id, service, method, timeout=30000):
    return HOST.open_routed(app_id, service, method, timeout)


def stream(service, method, request=None, timeout=30000):
    call_obj = open(service, method, 1, timeout)
    if request is not None:
        call_obj.send(request)
    call_obj.half_close()
    return call_obj


def routed(app_id, service, method, request=None, timeout=30000):
    call_obj = open_routed(app_id, service, method, timeout)
    if request is not None:
        call_obj.send(request)
    call_obj.half_close()
    reply = call_obj.recv(timeout)
    call_obj.end(0, "")
    return reply


def routed_stream(app_id, service, method, request=None, timeout=30000):
    call_obj = open_routed(app_id, service, method, timeout)
    if request is not None:
        call_obj.send(request)
    call_obj.half_close()
    return call_obj


def notify_invocation(invocation_id, accepted, detail=""):
    HOST.notify_invocation(invocation_id, accepted, detail)


def notify_action(action_invocation_id, succeeded, detail=""):
    HOST.notify_action(action_invocation_id, succeeded, detail)


def info():
    return HOST.info()


def handle_info(handle):
    return HOST.handle_info(handle)


def register_descriptors(data):
    HOST.register_descriptors(data)


def ready():
    return HOST.ready()


def connected():
    return HOST.connected()


def disconnected():
    return HOST.disconnected()


def state():
    return HOST.state()


def monotonic_ms():
    return HOST.monotonic_ms()


def every(interval_ms, fn):
    return HOST.every(interval_ms, fn)


def cancel_timer(token):
    return HOST.cancel_timer(token)


Call = ScriptedCall
