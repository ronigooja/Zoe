"""Opt-in Linux check: ZOE_TEST_PINNED_MEM0=1 python -m unittest services.memory.tests.test_pinned_write_recovery.

Requires existing mem0ai==2.2.1 dependencies. No model, Qdrant, or network calls.
"""
import hashlib
import importlib.metadata
import json
import os
import sqlite3
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from services.memory.person_memory import PersonMemory, person_user_id
from services.memory.prompts import MemoryPrompts

PROMPTS = MemoryPrompts("fixture ingest policy", "fixture write policy", "fixture custom policy")


@unittest.skipUnless(os.environ.get("ZOE_TEST_PINNED_MEM0") == "1", "designated Linux pinned-dependency test")
class PinnedWriteRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.assertEqual(importlib.metadata.version("mem0ai"), "2.2.1")
        # Disable telemetry/notices only; public add/update and all memory logic
        # remain real. Construct without service factories/network initialization.
        os.environ["MEM0_TELEMETRY"] = "false"
        import mem0.memory.main as main
        self.main = main
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.history = str(Path(self.directory.name) / "history.sqlite")
        self.vector_db = sqlite3.connect(Path(self.directory.name) / "vectors.sqlite")
        self.addCleanup(self.vector_db.close)
        self.vector_db.execute("CREATE TABLE vectors(id TEXT PRIMARY KEY,payload TEXT,vector TEXT)")
        self.calls = []
        self.raise_after = False
        for name in ("capture_event", "display_first_run_notice"):
            patcher = patch.object(main, name)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.make_backend()
        self.addCleanup(lambda: self.memory.db.connection.close())

    def make_backend(self):
        from mem0.memory.storage import SQLiteManager
        memory = object.__new__(self.main.Memory)
        memory.db = SQLiteManager(self.history)
        memory.config = SimpleNamespace(llm=SimpleNamespace(config={}), vector_store=SimpleNamespace(provider="qdrant"))
        memory.api_version = "v1.1"
        memory.custom_instructions = "configured extraction policy"
        memory.collection_name = "fixture"
        memory._entity_store = None
        memory.embedding_model = Mock()
        memory.embedding_model.embed.return_value = [0.1, 0.2]
        memory.embedding_model.embed_batch.side_effect = lambda texts, *a: [[0.1, 0.2] for _ in texts]
        memory.llm = Mock()

        def get(vector_id):
            row = self.vector_db.execute("SELECT payload FROM vectors WHERE id=?", (vector_id,)).fetchone()
            return SimpleNamespace(id=vector_id, payload=json.loads(row[0])) if row else None

        def insert(*, vectors, ids, payloads):
            # Dispatch must observe the durable intent, not merely an in-memory plan.
            writes = memory.db.connection.execute("SELECT count(*) FROM zoe_person_write_effects").fetchone()[0]
            ingests = memory.db.connection.execute("SELECT count(*) FROM zoe_person_ingest_manifests").fetchone()[0]
            self.assertGreater(writes + ingests, 0)
            for memory_id, payload, vector in zip(ids, payloads, vectors):
                if "zoe_write_token" in payload:
                    intents = [json.loads(row[0]) for row in memory.db.connection.execute(
                        "SELECT intent FROM zoe_person_write_effects")]
                    matching = [intent for intent in intents if intent["id"] == memory_id]
                    self.assertEqual(len(matching), 1)
                    self.assertEqual(matching[0]["payload"], payload)
                    self.assertEqual(matching[0]["vector"], vector)
                    self.assertEqual(memory.db.connection.execute(
                        "SELECT status FROM zoe_person_write_receipts WHERE op_id='op'").fetchone()[0], "applying")
            self.calls.append(list(ids))
            for memory_id, payload, vector in zip(ids, payloads, vectors):
                self.vector_db.execute("INSERT OR REPLACE INTO vectors VALUES(?,?,?)", (memory_id, json.dumps(payload), json.dumps(vector)))
            self.vector_db.commit()
            if self.raise_after:
                raise RuntimeError("applied then lost receipt")

        def update(*, vector_id, vector, payload):
            self.fail("unintercepted update: intent must precede deterministic upsert")

        memory.vector_store = SimpleNamespace(insert=insert, update=update, get=get,
            search=lambda **kwargs: [], list=lambda **kwargs: ([], None))
        self.memory = memory
        self.backend = PersonMemory(memory, PROMPTS)

    def restart(self):
        self.memory.db.connection.close()
        self.make_backend()

    def write(self):
        return self.backend.write("owner", "op", "request", time.monotonic() + 5)

    def test_public_add_and_update_recover_without_replanning(self):
        for replace in (False, True):
            with self.subTest(replace=replace):
                self.memory.db.connection.execute("DELETE FROM zoe_person_write_receipts")
                self.memory.db.connection.execute("DELETE FROM zoe_person_write_effects")
                self.memory.db.connection.commit()
                # Each subcase starts a NEW operation; the prior subcase's
                # recovery guard must not prohibit this initial planning call.
                self.memory.llm.generate_response = Mock()
                self.memory.llm.generate_response.return_value = '{"operations":[{"op":"add","text":"new"}]}'
                existing = []
                if replace:
                    row = self.vector_db.execute("SELECT id,payload FROM vectors LIMIT 1").fetchone()
                    existing = [{"id": row[0], "memory": json.loads(row[1])["data"]}]
                    self.memory.llm.generate_response.return_value = json.dumps({"operations": [{"op": "replace", "existing": "new", "text": "updated"}]})
                # PersonMemory search is not the subject of this public mutation test.
                self.memory.search = Mock(return_value={"results": existing})
                self.raise_after = True
                with self.assertRaises(RuntimeError):
                    self.write()
                self.memory.llm.generate_response.assert_called_once()
                intent = json.loads(self.memory.db.connection.execute(
                    "SELECT intent FROM zoe_person_write_effects WHERE op_id='op' AND position=0").fetchone()[0])
                actual = json.loads(self.vector_db.execute(
                    "SELECT payload FROM vectors WHERE id=?", (intent["id"],)).fetchone()[0])
                self.assertEqual(actual, intent["payload"])
                self.assertEqual(intent["preimage"]["data"] if replace else intent["preimage"],
                                 "new" if replace else None)
                messages = self.memory.llm.generate_response.call_args.args[0]
                self.assertEqual(messages[0]["content"], PROMPTS.memory_write)
                self.assertEqual(json.loads(messages[1]["content"])["request"], "request")
                calls = len(self.calls)
                self.restart()
                self.raise_after = False
                self.memory.llm.generate_response.side_effect = AssertionError("must not replan")
                result = self.write()
                self.memory.llm.generate_response.assert_not_called()
                self.assertEqual(result["operations"][0]["text"], "updated" if replace else "new")
                self.assertEqual(len(self.calls), calls)
                self.assertEqual(self.vector_db.execute("SELECT count(*) FROM vectors").fetchone()[0], 1)

    def test_public_infer_manifest_and_malformed_extraction(self):
        source = "[2026-11-13T11:00Z] user: I moved here yesterday.\n"
        self.memory.llm.generate_response.return_value = '{"memory":[{"text":"one"},{"text":"two"}]}'
        self.raise_after = True
        with self.assertRaises(RuntimeError):
            self.backend.ingest("owner", "batch", "chunk", source, time.monotonic() + 5)
        messages = self.memory.llm.generate_response.call_args.kwargs["messages"]
        self.assertIn(PROMPTS.memory_ingest, messages[1]["content"])
        self.assertIn(source.rstrip("\n"), messages[1]["content"])
        digest = self.memory.db.connection.execute(
            "SELECT content_hash FROM zoe_person_ingest_receipts WHERE chunk_id='chunk'"
        ).fetchone()[0]
        self.assertEqual(digest, hashlib.sha256(source.encode("utf-8")).hexdigest())
        records = json.loads(self.memory.db.connection.execute("SELECT manifest FROM zoe_person_ingest_manifests").fetchone()[0])
        self.assertEqual(len(records), 2)
        self.assertEqual(len(self.calls), 1)
        self.restart()
        self.raise_after = False
        self.memory.llm.generate_response.side_effect = AssertionError("must not reinfer")
        self.assertEqual(self.backend.ingest("owner", "batch", "chunk", source, time.monotonic() + 5), {"added": 2, "updated": 0})
        self.assertEqual(self.vector_db.execute("SELECT count(*) FROM vectors").fetchone()[0], 2)
        self.memory.llm.generate_response.side_effect = None
        self.memory.llm.generate_response.return_value = "malformed"
        with self.assertRaises(Exception):
            self.backend.ingest("owner", "batch", "bad", "source", time.monotonic() + 5)
        status = self.memory.db.connection.execute("SELECT status FROM zoe_person_ingest_receipts WHERE chunk_id='bad'").fetchone()[0]
        self.assertEqual(status, "planning")
