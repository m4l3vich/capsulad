"""Capsulad MicroPython SDK.

The host injects the C module ``capsulad`` (thin libcapsulad ABI primitives) and
executes this prelude before the package script. The public surface mirrors the
QuickJS SDK: start/run/on/call/stream/routed plus notification helpers.
"""

import capsulad as _c


# MicroPython's embed port has no filesystem/open support, so this SDK carries
# a small, complete JSON codec for canonical protobuf JSON payloads instead of
# depending on the unix port's json/io modules.
def _json_escape(text):
    out = ['"']
    for ch in text:
        code = ord(ch)
        if ch == '"':
            out.append('\\"')
        elif ch == "\\":
            out.append("\\\\")
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\r":
            out.append("\\r")
        elif ch == "\t":
            out.append("\\t")
        elif code < 0x20:
            out.append("\\u%04x" % code)
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _json_dumps(value):
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, str):
        return _json_escape(value)
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return repr(value)
    if isinstance(value, (list, tuple)):
        return "[" + ",".join(_json_dumps(item) for item in value) + "]"
    if isinstance(value, dict):
        parts = []
        for key in value:
            parts.append(_json_escape(str(key)) + ":" + _json_dumps(value[key]))
        return "{" + ",".join(parts) + "}"
    raise TypeError("cannot encode " + type(value).__name__ + " as JSON")


_JSON_WS = " \t\r\n"


class _JsonParser:
    __slots__ = ("text", "pos", "length")

    def __init__(self, text):
        if isinstance(text, bytes):
            text = text.decode()
        self.text = text
        self.pos = 0
        self.length = len(text)

    def parse(self):
        value = self._value()
        self._ws()
        if self.pos != self.length:
            raise ValueError("trailing JSON data")
        return value

    def _ws(self):
        while self.pos < self.length and self.text[self.pos] in _JSON_WS:
            self.pos += 1

    def _value(self):
        self._ws()
        if self.pos >= self.length:
            raise ValueError("unexpected end of JSON")
        ch = self.text[self.pos]
        if ch == "{":
            return self._object()
        if ch == "[":
            return self._array()
        if ch == '"':
            return self._string()
        if ch == "t" and self.text.startswith("true", self.pos):
            self.pos += 4
            return True
        if ch == "f" and self.text.startswith("false", self.pos):
            self.pos += 5
            return False
        if ch == "n" and self.text.startswith("null", self.pos):
            self.pos += 4
            return None
        return self._number()

    def _object(self):
        self.pos += 1
        result = {}
        self._ws()
        if self.pos < self.length and self.text[self.pos] == "}":
            self.pos += 1
            return result
        while True:
            self._ws()
            if self.pos >= self.length or self.text[self.pos] != '"':
                raise ValueError("expected JSON object key")
            key = self._string()
            self._ws()
            if self.pos >= self.length or self.text[self.pos] != ":":
                raise ValueError("expected ':' in JSON object")
            self.pos += 1
            result[key] = self._value()
            self._ws()
            if self.pos >= self.length:
                raise ValueError("unterminated JSON object")
            ch = self.text[self.pos]
            self.pos += 1
            if ch == "}":
                return result
            if ch != ",":
                raise ValueError("expected ',' or '}' in JSON object")

    def _array(self):
        self.pos += 1
        result = []
        self._ws()
        if self.pos < self.length and self.text[self.pos] == "]":
            self.pos += 1
            return result
        while True:
            result.append(self._value())
            self._ws()
            if self.pos >= self.length:
                raise ValueError("unterminated JSON array")
            ch = self.text[self.pos]
            self.pos += 1
            if ch == "]":
                return result
            if ch != ",":
                raise ValueError("expected ',' or ']' in JSON array")

    def _string(self):
        self.pos += 1
        out = []
        while self.pos < self.length:
            ch = self.text[self.pos]
            self.pos += 1
            if ch == '"':
                return "".join(out)
            if ch != "\\":
                out.append(ch)
                continue
            if self.pos >= self.length:
                break
            esc = self.text[self.pos]
            self.pos += 1
            if esc == '"':
                out.append('"')
            elif esc == "\\":
                out.append("\\")
            elif esc == "/":
                out.append("/")
            elif esc == "b":
                out.append("\b")
            elif esc == "f":
                out.append("\f")
            elif esc == "n":
                out.append("\n")
            elif esc == "r":
                out.append("\r")
            elif esc == "t":
                out.append("\t")
            elif esc == "u":
                if self.pos + 4 > self.length:
                    raise ValueError("short JSON unicode escape")
                code = int(self.text[self.pos:self.pos + 4], 16)
                self.pos += 4
                out.append(chr(code))
            else:
                raise ValueError("invalid JSON escape")
        raise ValueError("unterminated JSON string")

    def _number(self):
        start = self.pos
        while self.pos < self.length and self.text[self.pos] in "-+0123456789.eE":
            self.pos += 1
        if start == self.pos:
            raise ValueError("invalid JSON value")
        token = self.text[start:self.pos]
        try:
            if "." in token or "e" in token or "E" in token:
                return float(token)
            return int(token)
        except ValueError:
            raise ValueError("invalid JSON number")


def _json_loads(text):
    return _JsonParser(text).parse()


_EVENT_NAMES = {
    1: "invocation",
    2: "settings_changed",
    3: "action",
    4: "activation",
    5: "touch_released",
}

_handlers = {}
_call_handler = None
_disconnect_handler = None
_started = False
_finished = False
_live = []
_timers = []
_timer_seq = 0
MAX_TIMERS = 16


class Call:
    """One outbound or inbound AppBus call. Handles are explicit and released
    by end(), cancel(), or the run loop's terminal cleanup."""

    __slots__ = ("_handle", "_closed", "info")

    def __init__(self, handle, info=None):
        self._handle = handle
        self._closed = False
        self.info = info
        _live.append(self)

    def send(self, value):
        self._check()
        _c._send_json(self._handle, _json_dumps(value))
        return self

    def send_bytes(self, data):
        self._check()
        _c._send_proto(self._handle, data)
        return self

    def half_close(self):
        self._check()
        _c._half_close(self._handle)
        return self

    def recv(self, timeout=30000):
        self._check()
        text = _c._recv_json(self._handle, timeout)
        if text is None:
            return None
        return _json_loads(text)

    def recv_bytes(self, timeout=30000):
        self._check()
        return _c._recv_proto(self._handle, timeout)

    def end(self, code=0, detail=""):
        if not self._closed:
            self._closed = True
            try:
                _c._end(self._handle, code, detail)
            finally:
                self._release_quiet()
        return self

    def cancel(self):
        if not self._closed:
            self._closed = True
            try:
                _c._cancel(self._handle)
            finally:
                self._release_quiet()
        return self

    def status(self):
        code, detail = _c._call_status(self._handle)
        return {"code": code, "detail": detail}

    def call_info(self):
        return _json_loads(_c._call_info(self._handle))

    def _check(self):
        if self._closed:
            raise ValueError("call is closed")

    def _release_quiet(self):
        try:
            _live.remove(self)
        except ValueError:
            return
        try:
            _c._release(self._handle)
        except Exception:
            pass


def _release_all():
    for item in list(_live):
        try:
            item.cancel()
        except Exception:
            item._release_quiet()


def on(kind, fn):
    global _call_handler, _disconnect_handler
    if not callable(fn):
        raise TypeError("handler must be callable")
    if kind == "call":
        _call_handler = fn
        return
    if kind == "disconnect":
        _disconnect_handler = fn
        return
    if kind not in _EVENT_NAMES.values():
        raise ValueError("unknown event " + str(kind))
    _handlers[kind] = fn


def monotonic_ms():
    """Milliseconds from a monotonic clock; never goes backwards."""
    return _c._monotonic_ms()


def every(interval_ms, fn):
    """Call fn() from the run loop at most once per interval_ms.

    The callback runs between dispatched events (and from the poll timeout when
    the bus is idle), so it may not block for long. Exceptions are logged and do
    not end the run. Returns a token for cancel_timer().
    """
    global _timer_seq
    if not callable(fn):
        raise TypeError("timer handler must be callable")
    interval = int(interval_ms)
    if interval < 1:
        raise ValueError("interval_ms must be at least 1")
    if len(_timers) >= MAX_TIMERS:
        raise ValueError("at most %d repeating callbacks" % MAX_TIMERS)
    _timer_seq += 1
    token = _timer_seq
    _timers.append([token, interval, fn, monotonic_ms() + interval])
    return token


def cancel_timer(token):
    """Stop a callback registered with every(). Returns whether it existed."""
    for index in range(len(_timers)):
        if _timers[index][0] == token:
            del _timers[index]
            return True
    return False


def _poll_timeout():
    """Bound the next poll by the nearest due timer, at most one second."""
    wait = 1000
    if not _timers:
        return wait
    try:
        now = monotonic_ms()
    except Exception:
        # A broken clock must not end the run; poll at the default bound.
        return wait
    for timer in _timers:
        due = timer[3] - now
        if due < 0:
            due = 0
        if due < wait:
            wait = due
    return wait


def _registered(timer):
    for current in _timers:
        if current is timer:
            return True
    return False


def _run_timers():
    if not _timers:
        return
    try:
        now = monotonic_ms()
    except Exception:
        return
    for timer in list(_timers):
        if now < timer[3]:
            continue
        # Another callback in this same pass may have cancelled it.
        if not _registered(timer):
            continue
        timer[3] = now + timer[1]
        try:
            timer[2]()
        except Exception as err:
            try:
                _c._log("timer handler failed: " + repr(err))
            except Exception:
                pass


def start(timeout=10000):
    global _started
    if _started:
        return
    _c._start(timeout)
    _started = True


def run():
    global _finished
    if not _started:
        raise RuntimeError("capsulad.start() must be called first")
    if _finished:
        return {"status": _c._disconnect_status(), "detail": _c._disconnect_detail()}
    try:
        while True:
            job = _c._poll(_poll_timeout())
            if job is None:
                if _c._stopped() or _c._disconnected():
                    break
                _run_timers()
                continue
            kind = job[0]
            if kind == "event":
                name = _EVENT_NAMES.get(job[1], "unknown_%d" % job[1])
                handler = _handlers.get(name)
                if handler is not None:
                    handler(_json_loads(job[2]) if job[2] is not None else None)
            elif kind == "call":
                info = _json_loads(job[4]) if job[4] is not None else None
                call = Call(job[3], info)
                if _call_handler is None:
                    call.end(12, "no call handler registered")
                    continue
                try:
                    _call_handler(call)
                except Exception as err:
                    if not call._closed:
                        call.end(13, str(err))
                    _c._log("call handler failed: " + repr(err))
            elif kind == "disconnect":
                if _disconnect_handler is not None:
                    _disconnect_handler({"status": job[1], "detail": job[2]})
                break
            _run_timers()
    finally:
        _finished = True
        _timers[:] = []
        _release_all()
    return {"status": _c._disconnect_status(), "detail": _c._disconnect_detail()}


def stop():
    _c._stop()


def call(service, method, request=None, timeout=30000):
    text = _c._unary(service, method, _json_dumps({} if request is None else request), timeout)
    return _json_loads(text)


def call_bytes(service, method, request, timeout=30000):
    return _c._unary_proto(service, method, request, timeout)


def open(service, method, kind=1, timeout=30000):
    return Call(_c._call_start(service, method, kind, timeout), None)


def open_routed(app_id, service, method, timeout=30000):
    return Call(_c._call_start_routed(app_id, service, method, timeout), None)


def stream(service, method, request=None, timeout=30000):
    call_obj = open(service, method, 1, timeout)
    if request is not None:
        call_obj.send(request)
    call_obj.half_close()
    return call_obj


def routed(app_id, service, method, request=None, timeout=30000):
    call_obj = open_routed(app_id, service, method, timeout)
    try:
        call_obj.send({} if request is None else request)
        call_obj.half_close()
        reply = call_obj.recv(timeout)
        call_obj.end(0, "")
        return reply
    except Exception:
        call_obj.cancel()
        raise


def routed_stream(app_id, service, method, request=None, timeout=30000):
    call_obj = open_routed(app_id, service, method, timeout)
    if request is not None:
        call_obj.send(request)
    call_obj.half_close()
    return call_obj


def notify_invocation(invocation_id, accepted, detail=""):
    _c._notify_invocation(invocation_id, 1 if accepted else 0, detail)


def notify_action(action_invocation_id, succeeded, detail=""):
    _c._notify_action(action_invocation_id, 1 if succeeded else 0, detail)


def info():
    return _json_loads(_c._client_info())


def handle_info(handle):
    return _json_loads(_c._handle_info(handle))


def register_descriptors(data):
    _c._register_descriptors(data)


def ready():
    return _c._ready()


def connected():
    return _c._connected()


def disconnected():
    return _c._disconnected()


def state():
    return {
        "ready": _c._ready(),
        "connected": _c._connected(),
        "disconnected": _c._disconnected(),
        "status": _c._disconnect_status(),
        "detail": _c._disconnect_detail(),
    }


def log(message):
    _c._log(str(message))


env = _c._env()
WAIT_FOREVER = 0xFFFFFFFF

# The prelude executes in __main__, so publish the public surface on the
# injected capsulad module that app scripts import.
_c.Call = Call
_c.on = on
_c.start = start
_c.run = run
_c.stop = stop
_c.call = call
_c.call_bytes = call_bytes
_c.open = open
_c.open_routed = open_routed
_c.stream = stream
_c.routed = routed
_c.routed_stream = routed_stream
_c.notify_invocation = notify_invocation
_c.notify_action = notify_action
_c.info = info
_c.handle_info = handle_info
_c.register_descriptors = register_descriptors
_c.ready = ready
_c.connected = connected
_c.disconnected = disconnected
_c.state = state
_c.log = log
_c.env = env
_c.WAIT_FOREVER = WAIT_FOREVER
_c.monotonic_ms = monotonic_ms
_c.every = every
_c.cancel_timer = cancel_timer
