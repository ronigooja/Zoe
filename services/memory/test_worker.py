from __future__ import annotations

import http.client
import json
import os
import signal
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from .memory_api import MemoryAPIServer
from .worker import main


class MainTests(unittest.TestCase):
    def test_missing_token_fails_before_backend_construction(self):
        with patch.dict(os.environ, {}, clear=True), patch(
            "services.memory.worker.ControlledMem0"
        ) as backend, self.assertLogs("zoe.memory.worker", level="ERROR") as logs:
            with self.assertRaises(SystemExit) as result:
                main()
        self.assertEqual(result.exception.code, 1)
        backend.assert_not_called()
        self.assertEqual(len(logs.output), 1)
        self.assertIn("ZOE_MEMORY_API_TOKEN_FILE", logs.output[0])

    def test_main_serves_authenticated_health_and_sigterm_closes_server(self):
        backend = Mock()
        person = Mock()
        person.health.return_value = {"status": "ok"}
        handlers = {}
        ready = threading.Event()
        observed = []
        errors = []
        original = object()

        def register(signum, handler):
            handlers[signum] = handler
            ready.set()
            return original

        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            token_file = Path(directory) / "token"
            token_file.write_text("synthetic-token\n")
            server = MemoryAPIServer(("127.0.0.1", 0), person, "synthetic-token")

            def client():
                try:
                    if not ready.wait(5):
                        raise TimeoutError("signal handler was not registered")
                    connection = http.client.HTTPConnection(*server.server_address, timeout=5)
                    try:
                        connection.request("GET", "/healthz", headers={"Authorization": "Bearer synthetic-token"})
                        response = connection.getresponse()
                        observed.append((response.status, json.loads(response.read())))
                    finally:
                        connection.close()
                except Exception as exc:
                    errors.append(exc)
                finally:
                    if callable(handlers.get(signal.SIGTERM)):
                        handlers[signal.SIGTERM](signal.SIGTERM, None)

            thread = threading.Thread(target=client, daemon=True)
            with patch.dict(os.environ, {"ZOE_MEMORY_API_TOKEN_FILE": str(token_file),
                                        "ZOE_MEM0_LLM_MODEL": "test-model"}, clear=True), patch(
                "services.memory.worker.ControlledMem0", return_value=backend
            ), patch("services.memory.worker.create_memory_api", return_value=server), patch(
                "services.memory.worker.signal.signal", side_effect=register
            ) as register_signal:
                thread.start()
                main()
                thread.join(5)
            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
            self.assertEqual(observed, [(200, {"status": "ok"})])
            self.assertEqual(server.socket.fileno(), -1)
            register_signal.assert_called_with(signal.SIGTERM, original)
            backend.memory.db.close.assert_called_once()

    def test_listener_factory_reads_token_and_address(self):
        from .memory_api import create_memory_api

        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            token_file = Path(directory) / "token"
            token_file.write_text("synthetic-token\n")
            with patch.dict(os.environ, {"ZOE_MEMORY_API_TOKEN_FILE": str(token_file),
                                        "ZOE_MEMORY_API_ADDR": "127.0.0.1:0"}), patch(
                "services.memory.memory_api.PersonMemory"
            ) as person:
                controlled = SimpleNamespace(memory=object())
                server = create_memory_api(controlled)
                try:
                    person.assert_called_once_with(controlled.memory)
                    self.assertEqual(server.token, b"synthetic-token")
                    self.assertEqual(server.server_address[0], "127.0.0.1")
                finally:
                    server.server_close()
