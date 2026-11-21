"""Small contract tests; no Mem0, model, or vector-service dependency."""
import json
import hashlib
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
from services.memory.prompts import MemoryPrompts

PROMPTS = MemoryPrompts("fixture ingest policy", "fixture write policy", "fixture custom policy")


class PersonMemoryContractTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "history.sqlite"
        self.db = SimpleNamespace(connection=sqlite3.connect(self.path), _lock=threading.Lock())
        self.db.save_messages = Mock()
        self.db.connection.executescript(
            "CREATE TABLE history(id TEXT PRIMARY KEY,memory_id TEXT,old_memory TEXT,new_memory TEXT,"
            "event TEXT,created_at TEXT,is_deleted INTEGER);"
            "CREATE TABLE messages(id TEXT PRIMARY KEY,session_scope TEXT,role TEXT,content TEXT,created_at TEXT);"
        )
        self.vector_path = Path(self.directory.name) / "vectors.sqlite"
        self.vectors = sqlite3.connect(self.vector_path)
        self.addCleanup(lambda: self.vectors.close())
        self.vectors.execute("CREATE TABLE vectors(id TEXT PRIMARY KEY,payload TEXT,vector TEXT)")
        self.dispatch_error = None
        self.vector_calls = []
        self.existing_vectors = []

        def insert(*, vectors, ids, payloads):
            for memory_id, payload, vector in zip(ids, payloads, vectors):
                self.vector_calls.append(memory_id)
                self.vectors.execute("INSERT OR REPLACE INTO vectors VALUES(?,?,?)",
                                     (memory_id, json.dumps(payload), json.dumps(vector)))
                self.vectors.commit()
                if self.dispatch_error:
                    raise self.dispatch_error

        def get(*, vector_id):
            row = self.vectors.execute("SELECT payload FROM vectors WHERE id=?", (vector_id,)).fetchone()
            return SimpleNamespace(payload=json.loads(row[0])) if row else None

        self.raw_insert = insert
        self.raw_get = get
        self.memory = SimpleNamespace(
            db=self.db, vector_store=SimpleNamespace(insert=insert, update=Mock(), get=get, search=lambda *a, **k: self.existing_vectors), _delete_memory=Mock(),
            search=Mock(return_value={"results": []}),
            add=Mock(return_value={"results": [{"event": "ADD"}]}),
            llm=SimpleNamespace(generate_response=Mock(return_value='{"operations":[]}')),
        )
        self.backend = PersonMemory(self.memory, PROMPTS)
        self.memory.add.side_effect = self.inferred_add
        self.memory.update = Mock(side_effect=self.update_memory)
        self.addCleanup(lambda: self.db.connection.close())
        adapter = SimpleNamespace(set_request_timeout=lambda _: None, reset_request_timeout=lambda _: None)
        self.patch = patch.dict(sys.modules, {"services.memory.responses_llm": adapter})
        self.patch.start()
        self.addCleanup(self.patch.stop)

    def reopen(self):
        self.db.connection.close()
        self.db.connection = sqlite3.connect(self.path)
        self.vectors.close()
        self.vectors = sqlite3.connect(self.vector_path)
        # A real restart creates new Mem0 methods, not wrappers around wrappers.
        old = self.memory
        self.memory = SimpleNamespace(
            db=self.db, vector_store=SimpleNamespace(insert=self.raw_insert, get=self.raw_get,
                update=self.raw_update, search=lambda *a, **k: self.existing_vectors),
            _delete_memory=Mock(), search=old.search, add=old.add, llm=old.llm,
            update=Mock(side_effect=self.update_memory))
        self.backend = PersonMemory(self.memory, PROMPTS)

    def raw_update(self, *, vector_id, vector, payload):
        return self.raw_insert(ids=[vector_id], vectors=[vector], payloads=[payload])

    def update_memory(self, memory_id, text):
        current = self.memory.vector_store.get(vector_id=memory_id)
        self.memory.vector_store.update(vector_id=memory_id, vector=[2.0], payload=dict(
            current.payload, data=text, hash=hashlib.md5(text.encode()).hexdigest()))

    def inferred_add(self, text, **kwargs):
        if not kwargs.get("infer"):
            self.memory.vector_store.insert(ids=["random"], vectors=[[1.0]], payloads=[{
                "user_id": kwargs["user_id"], "data": text, "hash": hashlib.md5(text.encode()).hexdigest()}])
            return self.memory.add.return_value
        self.memory.vector_store.search()
        raw = self.memory.llm.generate_response([])
        facts = json.loads(raw)["memory"]
        ids, vectors, payloads = [], [], []
        existing_hashes = {r.payload.get("hash") for r in self.existing_vectors}
        for index, fact in enumerate(facts):
            digest = hashlib.md5(fact["text"].encode()).hexdigest()
            if digest in existing_hashes:
                continue
            ids.append(f"id-{index}")
            vectors.append([index, 1.0])
            payloads.append({"user_id": kwargs["user_id"], "data": fact["text"], "hash": digest})
        if ids:
            try:
                self.memory.vector_store.insert(ids=ids, vectors=vectors, payloads=payloads)
            except Exception:
                for memory_id, vector, payload in zip(ids, vectors, payloads):
                    self.memory.vector_store.insert(ids=[memory_id], vectors=[vector], payloads=[payload])
        return {"results": []}

    def ingest(self, chunk="chunk", text="transcript", user="owner"):
        self.memory.llm.generate_response.return_value = '{"memory":[{"text":"fact"}]}'
        return self.backend.ingest(user, "batch", chunk, text, time.monotonic() + 5)

    def test_completed_receipt_survives_backend_restart(self):
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.reopen()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.memory.add.assert_called_once()
        with self.assertRaises(MemoryAPIError) as caught:
            self.ingest(text="changed")
        self.assertEqual(caught.exception.kind, "idempotency_conflict")
        self.memory.add.side_effect = lambda text, **kwargs: self.inferred_add(text, **kwargs)
        # A different owner must not overwrite an existing owner's exact ID.
        self.vectors.execute("DELETE FROM vectors")
        self.vectors.commit()
        self.ingest(user="other")
        self.assertEqual(self.memory.add.call_count, 2)

    def test_applied_then_error_reconciles_without_inference(self):
        self.dispatch_error = RuntimeError("lost result")
        with self.assertRaises(RuntimeError):
            self.ingest()
        self.dispatch_error = None
        self.reopen()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.memory.add.assert_called_once()
        self.assertEqual(self.vector_calls, ["id-0"])

    def test_write_uses_only_full_request(self):
        request = "Remember that my preferred airport for the upcoming trip is LHR."
        self.backend.write("owner", "op", request, time.monotonic() + 5)
        self.assertEqual(self.memory.search.call_args.args, (request,))
        messages = self.memory.llm.generate_response.call_args.args[0]
        self.assertEqual(json.loads(messages[1]["content"]), {"request": request, "existing_memories": []})

    def prepare_write(self, replace=False):
        from services.memory.person_memory import person_user_id
        if replace:
            self.raw_insert(ids=["target"], vectors=[[0.0]], payloads=[{
                "user_id": person_user_id("owner"), "data": "old", "hash": hashlib.md5(b"old").hexdigest()}])
            self.memory.search.return_value = {"results": [{"id": "target", "memory": "old"}]}
            self.memory.llm.generate_response.return_value = '{"operations":[{"op":"replace","existing":"old","text":"new"}]}'
        else:
            self.memory.llm.generate_response.return_value = '{"operations":[{"op":"add","text":"new"}]}'

    def write_request(self):
        return self.backend.write("owner", "op", "request", time.monotonic() + 5)

    def test_write_add_and_replace_effect_boundaries(self):
        for replace in (False, True):
            for boundary in ("before", "after", "checkpoint"):
                with self.subTest(replace=replace, boundary=boundary):
                    self.db.connection.execute("DELETE FROM zoe_person_write_receipts")
                    self.db.connection.execute("DELETE FROM zoe_person_write_effects")
                    self.db.connection.execute("DELETE FROM history")
                    self.db.connection.commit()
                    self.vectors.execute("DELETE FROM vectors")
                    self.vectors.commit()
                    self.memory.search.return_value = {"results": []}
                    self.memory.llm.generate_response.reset_mock()
                    self.prepare_write(replace)
                    self.vector_calls.clear()
                    if boundary == "before":
                        self.backend._dispatch_insert = Mock(side_effect=RuntimeError("before"))
                    elif boundary == "after":
                        self.dispatch_error = RuntimeError("after")
                    else:
                        self.db.connection.execute("CREATE TRIGGER checkpoint_fault BEFORE UPDATE OF progress ON zoe_person_write_receipts BEGIN SELECT RAISE(ABORT,'crash'); END")
                        self.db.connection.commit()
                    with self.assertRaises((RuntimeError, sqlite3.IntegrityError)):
                        self.write_request()
                    self.reopen()
                    self.dispatch_error = None
                    if boundary == "checkpoint":
                        self.db.connection.execute("DROP TRIGGER checkpoint_fault")
                        self.db.connection.commit()
                    outcome = self.write_request()
                    self.assertEqual(outcome["operations"][0]["text"], "new")
                    self.assertEqual(len(self.vector_calls), 1)
                    self.assertEqual(self.vectors.execute("SELECT count(*) FROM vectors").fetchone()[0], 1)
                    self.memory.llm.generate_response.assert_called_once()
                    self.reopen()
                    self.assertEqual(self.write_request(), outcome)
                    self.assertEqual(self.db.connection.execute("SELECT count(*) FROM history").fetchone()[0], 1)

    def test_write_conflicting_payload_quarantines_without_overwrite(self):
        for replace in (False, True):
            for field in ("user_id", "hash", "data"):
                with self.subTest(replace=replace, field=field):
                    self.db.connection.execute("DELETE FROM zoe_person_write_receipts")
                    self.db.connection.execute("DELETE FROM zoe_person_write_effects")
                    self.db.connection.commit()
                    self.vectors.execute("DELETE FROM vectors")
                    self.vectors.commit()
                    self.memory.search.return_value = {"results": []}
                    self.prepare_write(replace)
                    self.backend._dispatch_insert = Mock(side_effect=RuntimeError("before"))
                    with self.assertRaises(RuntimeError):
                        self.write_request()
                    intent = json.loads(self.db.connection.execute("SELECT intent FROM zoe_person_write_effects").fetchone()[0])
                    payload = dict(intent["preimage"] or intent["payload"], **{field: "conflict"})
                    self.raw_insert(ids=[intent["id"]], vectors=[[9.0]], payloads=[payload])
                    self.reopen()
                    self.vector_calls.clear()
                    with self.assertRaises(MemoryAPIError) as caught:
                        self.write_request()
                    self.assertEqual(caught.exception.kind, "write_quarantined")
                    self.assertEqual(self.vector_calls, [])
                    self.reopen()
                    with self.assertRaises(MemoryAPIError) as caught:
                        self.write_request()
                    self.assertEqual(caught.exception.kind, "write_quarantined")

    def test_write_manifest_commit_failure_restarts_from_saved_model_plan(self):
        self.prepare_write()
        self.db.connection.execute("CREATE TRIGGER intent_fault BEFORE INSERT ON zoe_person_write_effects BEGIN SELECT RAISE(ABORT,'crash'); END")
        self.db.connection.commit()
        with self.assertRaises(sqlite3.IntegrityError):
            self.write_request()
        self.assertEqual(self.vector_calls, [])
        self.reopen()
        self.db.connection.execute("DROP TRIGGER intent_fault")
        self.db.connection.commit()
        self.assertEqual(self.write_request()["operations"][0]["text"], "new")
        self.memory.llm.generate_response.assert_called_once()

    def test_manifestless_applying_is_quarantined_without_duplicate_random_effect(self):
        from services.memory.person_memory import person_user_id
        self.raw_insert(ids=["old-random-effect"], vectors=[[1.0]], payloads=[{
            "user_id": person_user_id("owner"), "data": "new", "hash": hashlib.md5(b"new").hexdigest()}])
        self.db.connection.execute("INSERT INTO zoe_person_write_receipts VALUES(?,?,?,?,?,?,NULL)",
            (person_user_id("owner"), "op", hashlib.sha256(b"request").hexdigest(),
             '[{"op":"add","text":"new"}]', 0, "applying"))
        self.db.connection.commit()
        self.reopen()
        self.vector_calls.clear()
        with self.assertRaises(MemoryAPIError) as caught:
            self.write_request()
        self.assertEqual(caught.exception.kind, "write_quarantined")
        self.memory.add.assert_not_called()
        self.memory.llm.generate_response.assert_not_called()
        self.assertEqual(self.vector_calls, [])
        self.assertEqual(self.vectors.execute("SELECT id FROM vectors").fetchall(), [("old-random-effect",)])
        self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_write_receipts").fetchone()[0], "quarantined")

    def direct_ingest(self):
        return self.backend.ingest("owner", "batch", "chunk", "transcript", time.monotonic() + 5)

    def test_partial_batch_restart_applies_only_missing_ids(self):
        self.memory.llm.generate_response.return_value = '{"memory":[{"text":"one"},{"text":"two"}]}'
        self.dispatch_error = RuntimeError("applied first only")
        with self.assertRaises(RuntimeError):
            self.direct_ingest()
        self.assertEqual(self.vector_calls, ["id-0"])
        self.reopen()
        self.dispatch_error = None
        self.assertEqual(self.direct_ingest(), {"added": 2, "updated": 0})
        self.assertEqual(self.vector_calls, ["id-0", "id-1"])
        self.memory.add.assert_called_once()
        self.reopen()
        self.direct_ingest()
        self.assertEqual(self.db.connection.execute("SELECT count(*) FROM history").fetchone()[0], 2)
        self.assertEqual(self.db.connection.execute("SELECT count(*) FROM messages").fetchone()[0], 1)

    def test_manifest_persisted_before_any_dispatch_and_restart(self):
        with patch.object(self.backend, "_recover_ingest", side_effect=RuntimeError("crash")):
            with self.assertRaises(RuntimeError):
                self.ingest()
        self.assertEqual(self.vector_calls, [])
        self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts").fetchone()[0], "manifest")
        self.reopen()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.memory.add.assert_called_once()

    def test_owner_or_hash_conflict_never_overwritten(self):
        for key in ("user_id", "hash"):
            with self.subTest(key=key):
                self.memory.add.reset_mock()
                with patch.object(self.backend, "_recover_ingest", side_effect=RuntimeError):
                    with self.assertRaises(RuntimeError):
                        self.ingest()
                manifest = json.loads(self.db.connection.execute("SELECT manifest FROM zoe_person_ingest_manifests").fetchone()[0])
                payload = dict(manifest[0]["payload"], **{key: "wrong"})
                self.vectors.execute("INSERT OR REPLACE INTO vectors VALUES(?,?,?)", ("id-0", json.dumps(payload), "[]"))
                self.vectors.commit()
                self.reopen()
                with self.assertRaises(MemoryAPIError) as caught:
                    self.ingest()
                self.assertEqual(caught.exception.kind, "ingest_quarantined")
                self.assertEqual(self.vector_calls, [])
                self.db.connection.execute("DELETE FROM zoe_person_ingest_receipts")
                self.db.connection.execute("DELETE FROM zoe_person_ingest_manifests")
                self.db.connection.commit()

    def test_bad_extraction_cannot_complete_but_empty_can(self):
        for raw in ("broken", '{}', '{"memory":[{}]}'):
            self.memory.llm.generate_response.return_value = raw
            with self.assertRaises(MemoryAPIError):
                self.direct_ingest()
            self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts").fetchone()[0], "planning")
        self.reopen()
        self.memory.llm.generate_response.return_value = '{"memory":[]}'
        self.assertEqual(self.direct_ingest(), {"added": 0, "updated": 0})
        self.assertEqual(self.vector_calls, [])

    def test_legacy_receipt_is_durably_quarantined(self):
        from services.memory.person_memory import person_user_id
        self.db.connection.execute("INSERT INTO zoe_person_ingest_receipts VALUES(?,?,?,?,?,NULL)",
                                   (person_user_id("owner"), "chunk", "batch", hashlib.sha256(b"transcript").hexdigest(), "running"))
        self.db.connection.commit()
        for _ in range(2):
            self.reopen()
            with self.assertRaises(MemoryAPIError) as caught:
                self.ingest()
            self.assertEqual(caught.exception.kind, "ingest_quarantined")
        self.memory.add.assert_not_called()
        self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts").fetchone()[0], "quarantined")

    def test_verification_and_bookkeeping_failure_leave_manifest_recoverable(self):
        self.db.connection.execute(
            "CREATE TRIGGER fail_history BEFORE INSERT ON history BEGIN SELECT RAISE(ABORT,'fault'); END")
        self.db.connection.commit()
        with self.assertRaises(sqlite3.IntegrityError):
            self.ingest()
        self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts").fetchone()[0], "manifest")
        self.assertEqual(self.db.connection.execute("SELECT count(*) FROM messages").fetchone()[0], 0)
        self.reopen()
        self.db.connection.execute("DROP TRIGGER fail_history")
        self.db.connection.commit()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.assertEqual(self.vector_calls, ["id-0"])
        self.memory.add.assert_called_once()

    def test_missing_after_upsert_is_not_completed(self):
        self.backend._dispatch_insert = Mock()
        with self.assertRaises(MemoryAPIError) as caught:
            self.ingest()
        self.assertEqual(caught.exception.kind, "ingest_not_verified")
        self.assertEqual(self.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts").fetchone()[0], "manifest")
        self.reopen()
        self.assertEqual(self.ingest(), {"added": 1, "updated": 0})
        self.memory.add.assert_called_once()

    def test_missing_embedding_cannot_create_partial_manifest(self):
        def incomplete(text, **kwargs):
            self.memory.vector_store.search()
            self.memory.llm.generate_response([])
            self.memory.vector_store.insert(ids=["one"], vectors=[[1.0]], payloads=[{
                "user_id": kwargs["user_id"], "data": "one", "hash": hashlib.md5(b"one").hexdigest()}])
        self.memory.add.side_effect = incomplete
        self.memory.llm.generate_response.return_value = '{"memory":[{"text":"one"},{"text":"two"}]}'
        with self.assertRaises(MemoryAPIError) as caught:
            self.direct_ingest()
        self.assertEqual(caught.exception.kind, "ingest_incomplete_plan")
        self.assertEqual(self.vector_calls, [])
        self.assertEqual(self.db.connection.execute("SELECT count(*) FROM zoe_person_ingest_manifests").fetchone()[0], 0)

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
        self.dispatch_error = RuntimeError("lost result")
        with self.assertRaises(RuntimeError):
            self.backend.write("owner", "uncertain-op", "request", time.monotonic() + 5)
        self.reopen()
        self.dispatch_error = None
        self.backend.write("owner", "uncertain-op", "request", time.monotonic() + 5)
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
