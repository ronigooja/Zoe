"""Ephemeral, leased model observations; never persisted or logged."""
from __future__ import annotations

import contextvars
import datetime
import json
import os
import re
import threading
import time
import uuid

_component = contextvars.ContextVar("memory_observation_component", default=None)
_lock = threading.Lock()
_lease = ""
_epoch = 0
_expiry = 0.0
_events = []
_overflow = False
_api_token = ""
_timer = None
LEASE_SECONDS = 10
MAX_EVENTS = 64
MAX_PAYLOAD_BYTES = 8192


def set_component(component):
    return _component.set(component)


def reset_component(token):
    _component.reset(token)


def set_api_token(token):
    global _api_token
    _api_token = token


def _expire(expected):
    global _lease, _expiry, _timer, _overflow
    with _lock:
        if _lease == expected and time.monotonic() >= _expiry:
            _events.clear()
            _overflow = False
            _lease, _expiry, _timer = "", 0.0, None


def drain(lease):
    """Renew this generation and atomically drain its bounded queue."""
    global _lease, _expiry, _timer, _epoch, _overflow
    if not isinstance(lease, str) or not re.fullmatch(r"[a-f0-9]{32}", lease):
        raise ValueError("invalid lease")
    with _lock:
        now = time.monotonic()
        if _lease != lease or now >= _expiry:
            _events.clear()
            _overflow = False
            _epoch += 1
        _lease, _expiry = lease, now + LEASE_SECONDS
        if _timer is not None:
            _timer.cancel()
        _timer = threading.Timer(LEASE_SECONDS, _expire, args=(lease,))
        _timer.daemon = True
        _timer.start()
        result = list(_events)
        overflow = _overflow
        _events.clear()
        _overflow = False
        return result, overflow


def _redact(text):
    key = os.environ.get("ZOE_MEM0_LLM_API_KEY", "")
    if key:
        text = text.replace(key, "[REDACTED]")
    if _api_token:
        text = text.replace(_api_token, "[REDACTED]")
    text = re.sub(r"(?i)\b(bearer\s+)[a-z0-9._~+/=-]+", r"\1[REDACTED]", text)
    text = re.sub(r"(?i)\b(sk|pk|api|key|token)[-_][a-z0-9_-]{12,}\b", "[REDACTED]", text)
    text = re.sub(r'''(?i)(["']?(?:api[_-]?key|authorization|access[_-]?token|password|secret)["']?\s*[:=]\s*["']?)[^\s,;"'}]+''', r"\1[REDACTED]", text)
    return text


def begin(messages, model):
    try:
        component = _component.get()
        with _lock:
            lease = (_lease, _epoch) if component in ("memory_write", "memory_ingestion") and time.monotonic() < _expiry else None
        if not lease:
            return None
        call = (lease, component, uuid.uuid4().hex)
        if not isinstance(messages, list) or len(messages) > 32:
            _publish(call, "request", {"omitted": "input_limit"}, True)
            return call
        safe_messages = [{"role": m.get("role"), "content": m.get("content")}
                         for m in messages if isinstance(m, dict) and m.get("role") in ("system", "user", "assistant")
                         and isinstance(m.get("content"), str)]
        if sum(len(m["content"]) for m in safe_messages) > MAX_PAYLOAD_BYTES:
            _publish(call, "request", {"omitted": "input_limit"}, True)
            return call
        _publish(call, "request", {"model": model if isinstance(model, str) else "", "messages": safe_messages})
        return call
    except Exception:
        return None


def finish(call, text):
    if call is not None and isinstance(text, str):
        if len(text) > MAX_PAYLOAD_BYTES:
            _publish(call, "response", {"omitted": "output_limit"}, True)
        else:
            _publish(call, "response", {"text": text})


def _publish(call, kind, payload, truncated=False):
    global _overflow
    try:
        def clean(value):
            if isinstance(value, str):
                return _redact(value)
            if isinstance(value, list):
                return [clean(item) for item in value]
            if isinstance(value, dict):
                return {key: clean(item) for key, item in value.items()}
            return value
        encoded = json.dumps(clean(payload), ensure_ascii=False, separators=(",", ":"))
        if len(encoded.encode("utf-8")) > MAX_PAYLOAD_BYTES:
            truncated = True
            encoded = '{"omitted":"payload_limit"}'
        event = {"component": call[1], "call_id": call[2], "time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                 "type": kind, "payload": json.loads(encoded), "truncated": truncated}
        with _lock:
            if call[0] == (_lease, _epoch) and time.monotonic() < _expiry:
                if len(_events) < MAX_EVENTS:
                    _events.append(event)
                else:
                    _overflow = True
    except Exception:
        pass  # Observation is strictly best effort.
