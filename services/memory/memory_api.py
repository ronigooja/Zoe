"""Authenticated standard-library HTTP listener for the internal memory API."""
from __future__ import annotations

import hmac
import json
import logging
import os
import queue
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from .person_memory import MemoryAPIError, PersonMemory, check_deadline
from .responses_llm import unaccounted_sync_call

LOG = logging.getLogger("zoe.memory.api")
MAX_BODY_BYTES = 64 * 1024
MAX_INGEST_BODY_BYTES = 256 * 1024


def _required_text(body: dict[str, Any], key: str) -> str:
    value = body.get(key)
    if not isinstance(value, str) or not value.strip():
        raise MemoryAPIError(400, "request_invalid")
    return value


class MemoryAPIServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], backend: PersonMemory, token: str, timeout: float = 60):
        self.backend = backend
        self.token = token.encode("utf-8")
        self.request_timeout = timeout
        self.slots = threading.BoundedSemaphore(16)
        super().__init__(address, MemoryAPIHandler)

    def handle_error(self, request, client_address):
        # BaseServer otherwise prints tracebacks, which may include content.
        LOG.warning("memory_api connection failed")


class MemoryAPIHandler(BaseHTTPRequestHandler):
    server: MemoryAPIServer

    def setup(self):
        super().setup()
        self.connection.settimeout(self.server.request_timeout)
        self.deadline = time.monotonic() + self.server.request_timeout

    def log_message(self, format, *args):
        pass  # Never log raw URLs, headers, request lines, or error bodies.

    def do_GET(self):
        self._handle()

    def do_POST(self):
        self._handle()

    def send_error(self, code, message=None, explain=None):
        self._respond(code, {"error_kind": "request_invalid"})

    def _respond(self, status: int, result: dict[str, Any]):
        payload = json.dumps(result, ensure_ascii=False, allow_nan=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        self.wfile.write(payload)

    def _body(self) -> dict[str, Any]:
        if self.headers.get("Transfer-Encoding") or len(self.headers.get_all("Content-Length", [])) != 1:
            raise MemoryAPIError(400, "request_invalid")
        try:
            size = int(self.headers["Content-Length"])
        except (ValueError, TypeError):
            raise MemoryAPIError(400, "request_invalid") from None
        limit = MAX_INGEST_BODY_BYTES if self.path == "/v1/memory/ingest" else MAX_BODY_BYTES
        if size > limit:
            raise MemoryAPIError(413, "body_too_large")
        if size < 0:
            raise MemoryAPIError(400, "request_invalid")
        if self.headers.get_content_type() != "application/json":
            raise MemoryAPIError(400, "request_invalid")
        self.connection.settimeout(check_deadline(self.deadline))
        data = self.rfile.read(size)
        try:
            body = json.loads(data)
        except (ValueError, UnicodeError, RecursionError):
            raise MemoryAPIError(400, "request_invalid") from None
        if len(data) != size or not isinstance(body, dict):
            raise MemoryAPIError(400, "request_invalid")
        return body

    def _dispatch(self):
        if self.command == "GET" and self.path == "/healthz":
            return self.server.backend.health(self.deadline)
        if self.command != "POST" or self.path not in {
            "/v1/memory/search", "/v1/memory/write", "/v1/memory/ingest", "/v1/memory/delete-all",
        }:
            raise MemoryAPIError(404, "not_found")
        body = self._body()
        user = _required_text(body, "user")
        if self.path.endswith("/ingest"):
            if set(body) != {"user", "text"}:
                raise MemoryAPIError(400, "request_invalid")
            text = _required_text(body, "text")
            with unaccounted_sync_call():
                return self.server.backend.ingest(user, text, self.deadline)
        if self.path.endswith("/search"):
            if set(body) - {"user", "query", "top_k"}:
                raise MemoryAPIError(400, "request_invalid")
            query = _required_text(body, "query")
            top_k = body.get("top_k", 8)
            if type(top_k) is not int or not 1 <= top_k <= 20:
                raise MemoryAPIError(400, "request_invalid")
            return self.server.backend.search(user, query, top_k, self.deadline)
        if self.path.endswith("/write"):
            if set(body) != {"user", "sentence", "restatement"}:
                raise MemoryAPIError(400, "request_invalid")
            sentence = _required_text(body, "sentence")
            restatement = _required_text(body, "restatement")
            with unaccounted_sync_call():
                return self.server.backend.write(user, sentence, restatement, self.deadline)
        if set(body) != {"user"}:
            raise MemoryAPIError(400, "request_invalid")
        return self.server.backend.delete_all(user, self.deadline)

    def _handle(self):
        started = time.monotonic()
        status = 500
        # Only fixed labels reach INFO, including for attacker-controlled URLs.
        kind = {"/healthz": "health", "/v1/memory/search": "search", "/v1/memory/write": "write",
                "/v1/memory/ingest": "ingest", "/v1/memory/delete-all": "delete_all"}.get(self.path, "unknown")
        try:
            authorization = self.headers.get("Authorization", "").encode("utf-8")
            if len(self.headers.get_all("Authorization", [])) != 1 or not hmac.compare_digest(
                authorization, b"Bearer " + self.server.token
            ):
                raise MemoryAPIError(401, "unauthorized")
            if not self.server.slots.acquire(blocking=False):
                raise MemoryAPIError(503, "busy")
            results = queue.Queue(maxsize=1)

            def run():
                try:
                    results.put((200, self._dispatch()))
                except MemoryAPIError as exc:
                    results.put((exc.status, {"error_kind": exc.kind}))
                except TimeoutError:
                    results.put((504, {"error_kind": "deadline_exceeded"}))
                except Exception:
                    results.put((502, {"error_kind": "backend_unavailable"}))
                finally:
                    self.server.slots.release()

            threading.Thread(target=run, daemon=True).start()
            try:
                status, result = results.get(timeout=check_deadline(self.deadline))
            except queue.Empty:
                raise MemoryAPIError(504, "deadline_exceeded") from None
        except MemoryAPIError as exc:
            status, result = exc.status, {"error_kind": exc.kind}
        try:
            self._respond(status, result)
        except (OSError, ValueError):
            pass
        finally:
            LOG.info("memory_api request kind=%s status=%s elapsed_ms=%d", kind, status,
                     (time.monotonic() - started) * 1000)


def start_memory_api(controlled: Any) -> MemoryAPIServer | None:
    token_file = os.environ.get("ZOE_MEMORY_API_TOKEN_FILE", "")
    if not token_file:
        LOG.info("memory_api disabled: ZOE_MEMORY_API_TOKEN_FILE is unset")
        return None
    token = Path(token_file).read_text(encoding="utf-8").strip()
    if not token or "\n" in token or "\r" in token:
        raise ValueError("memory API token file must contain one nonempty token")
    if controlled is None:
        raise ValueError("memory API is unavailable in deletion-only mode")
    address = os.environ.get("ZOE_MEMORY_API_ADDR", "0.0.0.0:8788")
    host, port = address.rsplit(":", 1)
    timeout = float(os.environ.get("ZOE_MEMORY_API_TIMEOUT_SECONDS", "60"))
    if not 0 < timeout <= 120:
        raise ValueError("memory API timeout must be positive and at most 120 seconds")
    server = MemoryAPIServer((host, int(port)), PersonMemory(controlled.memory), token, timeout)
    threading.Thread(target=server.serve_forever, name="memory-api", daemon=True).start()
    return server
