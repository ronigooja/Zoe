"""Small contract tests; no Mem0, model, or vector-service dependency."""
import json
import sqlite3
import sys
import threading
import tempfile
from pathlib import Path
import time
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from services.memory.person_memory import MemoryAPIError, PersonMemory
from services.memory.memory_api import MemoryAPIHandler


class PersonMemoryContractTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "history.sqlite"
        self.db = SimpleNamespace(connection=sqlite3.connect(self.path), _lock=threading.Lock())
        self.memory = SimpleNamespace(
            db=self.db, vector_store=SimpleNamespace(insert=Mock()), _delete_memory=Mock(),
            search=Mock(return_value={"results": []}),
            add=Mock(return_value={"results": [{"event": "ADD"}]}),
            llm=SimpleNamespace(generate_response=Mock(return_value='{"operations":[]}')),
        )
        self.backend = PersonMemory(self.memory)
        self.addCleanup(lambda: self.db.connection.close())
        adapter = SimpleNamespace(set_request_timeout=lambda _: None, reset_request_timeout=lambda _: None)
        self.patch = patch.dict(sys.modules, {"services.memory.responses_llm": adapter})
        self.patch.start()
        self.addCleanup(self.patch.stop)

    def reopen(self):
        self.db.connection.close()
        self.db.connection = sqlite3.connect(self.path)
        self.backend = PersonMemory(self.memory)

    def ingest(self, chunk="chunk", text="transcript", user="owner"):
        return self.backend.ingest(user, "batch", chunk, text, time.monotonic() + 5)

    def test_completed_receipt_survives_backend_restart(self):
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.reopen()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.memory.add.assert_called_once()
        with self.assertRaises(MemoryAPIError) as caught:
            self.ingest(text="changed")
        self.assertEqual(caught.exception.kind, "idempotency_conflict")
        self.ingest(user="other")
        self.assertEqual(self.memory.add.call_count, 2)

    def test_uncertain_application_is_never_replayed(self):
        self.memory.add.side_effect = RuntimeError("lost result")
        with self.assertRaises(RuntimeError):
            self.ingest()
        self.memory.add.side_effect = None
        self.reopen()
        with self.assertRaises(MemoryAPIError) as caught:
            self.ingest()
        self.assertEqual(caught.exception.kind, "ingest_uncertain")
        self.memory.add.assert_called_once()

    def test_write_uses_only_full_request(self):
        request = "Remember that my preferred airport for the upcoming trip is LHR."
        self.backend.write("owner", "op", request, time.monotonic() + 5)
        self.assertEqual(self.memory.search.call_args.args, (request,))
        messages = self.memory.llm.generate_response.call_args.args[0]
        self.assertEqual(json.loads(messages[1]["content"]), {"request": request, "existing_memories": []})

    def test_write_diagnostics_are_debug_only(self):
        for level in ("INFO", "DEBUG"):
            with self.assertLogs("zoe.memory.api", level=level) as logs:
                self.backend.write("owner", level, "private-request", time.monotonic() + 5)
            output = "\n".join(logs.output)
            self.assertEqual("private-request" in output, level == "DEBUG")
            self.assertEqual("model_output=" in output, level == "DEBUG")

    def test_planning_failure_is_not_uncertain_and_is_safe_to_retry(self):
        self.memory.llm.generate_response.return_value = "invalid"
        with self.assertRaises(MemoryAPIError) as caught:
            self.backend.write("owner", "op", "request", time.monotonic() + 5)
        self.assertEqual(caught.exception.kind, "output_invalid")
        self.memory.add.assert_not_called()
        self.reopen()
        self.memory.llm.generate_response.return_value = '{"operations":[]}'
        self.assertEqual(self.backend.write("owner", "op", "request", time.monotonic() + 5), {"operations": []})

    def test_write_completed_and_uncertain_disk_receipts(self):
        self.memory.llm.generate_response.return_value = '{"operations":[{"op":"add","text":"fact"}]}'
        self.memory.add.return_value = {"results": [{"id": "memory"}]}
        outcome = self.backend.write("owner", "done-op", "request", time.monotonic() + 5)
        self.reopen()
        self.assertEqual(self.backend.write("owner", "done-op", "request", time.monotonic() + 5), outcome)
        self.memory.add.assert_called_once()
        self.memory.add.side_effect = RuntimeError("lost result")
        with self.assertRaises(MemoryAPIError) as caught:
            self.backend.write("owner", "uncertain-op", "request", time.monotonic() + 5)
        self.assertEqual(caught.exception.kind, "write_uncertain")
        self.reopen()
        with self.assertRaises(MemoryAPIError) as caught:
            self.backend.write("owner", "uncertain-op", "request", time.monotonic() + 5)
        self.assertEqual(caught.exception.kind, "write_uncertain")
        self.assertEqual(self.memory.add.call_count, 2)

    def test_crash_after_operation_checkpoint_resumes_only_unfinished(self):
        from services.memory.person_memory import person_user_id
        import hashlib
        plan = [{"op": "add", "text": "already applied", "id": "old"}, {"op": "add", "text": "pending"}]
        self.db.connection.execute(
            "INSERT INTO zoe_person_write_receipts VALUES(?,?,?,?,?,?,NULL)",
            (person_user_id("owner"), "op", hashlib.sha256(b"request").hexdigest(), json.dumps(plan), 1, "ready")
        )
        self.db.connection.commit()
        self.reopen()
        self.memory.add.return_value = {"results": [{"id": "new"}]}
        result = self.backend.write("owner", "op", "request", time.monotonic() + 5)
        self.assertEqual(result["operations"][0]["id"], "old")
        self.memory.add.assert_called_once_with("pending", user_id=person_user_id("owner"), infer=False)
        self.memory.llm.generate_response.assert_not_called()
        self.reopen()
        self.assertEqual(self.backend.write("owner", "op", "request", time.monotonic() + 5), result)

    def test_http_contract_rejects_old_shapes(self):
        handler = object.__new__(MemoryAPIHandler)
        handler.command = "POST"
        handler.deadline = time.monotonic() + 5
        handler.server = SimpleNamespace(backend=Mock())
        for route, old in (
            ("write", {"user": "u", "sentence": "s", "restatement": "r"}),
            ("search", {"user": "u", "query": "q"}),
            ("ingest", {"user": "u", "text": "transcript"}),
        ):
            handler.path = "/v1/memory/" + route
            handler._body = lambda: old
            with self.assertRaises(MemoryAPIError) as caught:
                handler._dispatch()
            self.assertEqual(caught.exception.kind, "request_invalid")
        for route in ("write", "search"):
            handler.path = "/v1/memory/" + route
            handler._body = lambda: {"user": "u", "request": "complete request", **({"op_id": "op"} if route == "write" else {})}
            handler._dispatch()
        handler.server.backend.write.assert_called_once_with("u", "op", "complete request", handler.deadline)
        handler.server.backend.search.assert_called_once_with("u", "complete request", 8, handler.deadline)


if __name__ == "__main__":
    unittest.main()
