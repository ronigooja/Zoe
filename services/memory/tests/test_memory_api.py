from __future__ import annotations

import http.client
import io
import json
import logging
import sqlite3
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from services.memory.memory_api import MemoryAPIServer, start_memory_api
from services.memory.person_memory import PersonMemory, person_user_id
from services.memory.worker import configure_logging


class FakeMemory:
    def __init__(self):
        connection = sqlite3.connect(":memory:", check_same_thread=False)
        connection.executescript(
            "CREATE TABLE history(memory_id TEXT, text TEXT);"
            "CREATE TABLE messages(session_scope TEXT, content TEXT);"
        )
        self.db = SimpleNamespace(connection=connection, _lock=threading.Lock())
        self.records = {}
        self.llm = Mock()
        self.llm.generate_response.return_value = '{"operations":[]}'
        self.search_calls = []
        self.add_calls = []
        self.update_calls = []
        self.vector_store = SimpleNamespace(insert=self.insert, get=self.get, list=self.list)

    def insert(self, *, vectors, ids, payloads):
        for memory_id, payload in zip(ids, payloads):
            self.records[memory_id] = payload

    def get(self, *, vector_id):
        return SimpleNamespace(payload=self.records[vector_id])

    def list(self, *, filters, top_k):
        return ([SimpleNamespace(id=k, payload=v) for k, v in self.records.items()
                 if v["user_id"] == filters["user_id"]][:top_k], None)

    def search(self, query, *, filters, top_k, rerank):
        self.search_calls.append((query, filters, top_k, rerank))
        return {"results": [{"id": item.id, "memory": item.payload["data"], "score": .8}
                            for item in self.list(filters=filters, top_k=top_k)[0]]}

    def add(self, text, *, user_id, infer):
        assert infer is False
        self.add_calls.append((text, user_id, infer))
        memory_id = f"m{len(self.add_calls)}"
        self.vector_store.insert(vectors=[[]], ids=[memory_id], payloads=[{"user_id": user_id, "data": text}])
        self.db.connection.execute("INSERT INTO history VALUES (?,?)", (memory_id, text))
        self.db.connection.commit()
        return {"results": [{"id": memory_id}]}

    def update(self, memory_id, text):
        self.update_calls.append((memory_id, text))
        self.records[memory_id]["data"] = text

    def _delete_memory(self, memory_id, existing_memory=None):
        del self.records[memory_id]
        self.db.connection.execute("INSERT INTO history VALUES (?,?)", (memory_id, "deleted"))
        self.db.connection.commit()

    def delete_all(self, *, user_id):
        for item in self.list(filters={"user_id": user_id}, top_k=10000)[0]:
            self._delete_memory(item.id)


class MemoryAPITests(unittest.TestCase):
    def setUp(self):
        self.memory = FakeMemory()
        self.backend = PersonMemory(self.memory)
        self.server = MemoryAPIServer(("127.0.0.1", 0), self.backend, "secret-marker", 2)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.memory.db.connection.close()

    def request(self, path, body=None, token="secret-marker", *, method="POST", headers=None):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        request_headers = {"Content-Type": "application/json"}
        if token is not None:
            request_headers["Authorization"] = "Bearer " + token
        request_headers.update(headers or {})
        data = json.dumps(body) if body is not None else ""
        connection.request(method, path, data, request_headers)
        response = connection.getresponse()
        result = response.status, json.loads(response.read())
        connection.close()
        return result

    def write(self, operations):
        self.memory.llm.generate_response.return_value = json.dumps({"operations": operations})
        return self.request("/v1/memory/write", {"user": "workspace/owner", "sentence": "private-sentence",
                                                "restatement": "private-query"})

    def test_auth_required_even_health(self):
        for token in (None, "wrong-token"):
            self.assertEqual(self.request("/healthz", token=token, method="GET"),
                             (401, {"error_kind": "unauthorized"}))
        self.assertEqual(self.request("/healthz", method="GET")[0], 200)

    def test_body_limit_and_request_validation(self):
        self.assertEqual(self.request("/v1/memory/search", {"user": "u", "query": "a" * 65536})[0], 413)
        for top_k in (0, 21, True, "8"):
            self.assertEqual(self.request("/v1/memory/search", {"user": "u", "query": "q", "top_k": top_k})[0], 400)

    def test_search_no_llm_and_person_isolation(self):
        self.memory.add("private-fact", user_id=person_user_id("workspace/owner"), infer=False)
        status, result = self.request("/v1/memory/search", {"user": "workspace/owner", "query": "private-query"})
        self.assertEqual(status, 200)
        self.assertEqual(result["items"], [{"id": "m1", "text": "private-fact", "score": .8}])
        self.assertEqual(self.memory.search_calls[-1][2:], (8, False))
        self.assertEqual(self.request("/v1/memory/search", {"user": "workspace/other", "query": "q"})[1], {"items": []})
        self.memory.llm.generate_response.assert_not_called()
        self.assertNotEqual(person_user_id("workspace/owner"), person_user_id("other/owner"))

    def test_write_add_replace_none_exact_text(self):
        self.assertEqual(self.write([{"op": "add", "text": "private-fact"}])[0], 200)
        self.assertEqual(self.write([{"op": "replace", "existing": "private-fact", "text": "new-fact"}])[1],
                         {"operations": [{"op": "replace", "id": "m1", "text": "new-fact"}]})
        self.assertEqual(self.memory.update_calls, [("m1", "new-fact")])
        messages = self.memory.llm.generate_response.call_args.args[0]
        self.assertNotIn('"m1"', json.dumps(messages))
        self.assertEqual(self.write([{"op": "replace", "existing": "NEW-fact", "text": "another"}])[1]["operations"][0]["op"], "add")
        before = len(self.memory.add_calls)
        self.assertEqual(self.write([{"op": "none", "text": "not durable"}])[0], 200)
        self.assertEqual(self.write([]), (200, {"operations": []}))
        self.assertEqual(len(self.memory.add_calls), before)
        self.assertEqual(self.memory.llm.generate_response.call_count, 5)

    def test_invalid_output_never_partially_writes(self):
        bad_outputs = [
            "not-json", '{"operations":[],"extra":true}',
            '{"operations":[],"operations":[]}',
            json.dumps({"operations": [{"op": "add", "text": "valid"}, {"op": "add", "text": " bad "}]}),
            json.dumps({"operations": [{"op": "replace", "index": 0, "text": "bad"}]}),
            json.dumps({"operations": [{"op": "add", "text": "valid"}] * 9}),
        ]
        for raw in bad_outputs:
            self.memory.llm.generate_response.return_value = raw
            self.assertEqual(self.request("/v1/memory/write", {"user": "u", "sentence": "s", "restatement": "r"}),
                             (502, {"error_kind": "output_invalid"}))
        self.assertEqual(self.memory.add_calls, [])
        self.assertEqual(self.memory.update_calls, [])

    def test_production_adapter_requires_accounting_context(self):
        from services.memory.responses_llm import ResponsesLLM
        self.memory.llm = object.__new__(ResponsesLLM)
        with patch.object(ResponsesLLM, "generate_response") as generate:
            self.assertEqual(self.request("/v1/memory/write", {"user": "u", "sentence": "s", "restatement": "r"}),
                             (503, {"error_kind": "accounting_unavailable"}))
            generate.assert_not_called()

    def test_production_adapter_reuses_job_accounting_context(self):
        from services.memory.responses_llm import ResponsesLLM, _model_call_context, _request_timeout_seconds
        self.memory.llm = object.__new__(ResponsesLLM)
        self.backend.control_api = Mock()

        def generate(*args, **kwargs):
            context = _model_call_context.get()
            self.assertEqual((context.job_id, context.attempt), ("existing-job", 2))
            self.assertIs(context.api, self.backend.control_api)
            self.assertGreater(_request_timeout_seconds.get(), 0)
            return '{"operations":[]}'

        with patch.object(ResponsesLLM, "generate_response", side_effect=generate) as model:
            self.assertEqual(self.request("/v1/memory/write", {"user": "u", "sentence": "s", "restatement": "r"},
                                          headers={"X-Zoe-Memory-Job-ID": "existing-job", "X-Zoe-Memory-Job-Attempt": "2"}),
                             (200, {"operations": []}))
            self.assertEqual(model.call_count, 1)

    def test_delete_all_purges_only_person_history_messages(self):
        for user in ("a", "b"):
            self.memory.add(user, user_id=person_user_id(user), infer=False)
            self.memory.db.connection.execute("INSERT INTO messages VALUES (?,?)", (f"user_id={person_user_id(user)}", user))
        self.memory.db.connection.commit()
        self.assertEqual(self.request("/v1/memory/delete-all", {"user": "a"}), (200, {"deleted": True}))
        self.assertEqual(list(self.memory.records), ["m2"])
        self.assertEqual(self.memory.db.connection.execute("SELECT memory_id FROM history").fetchall(), [("m2",)])
        self.assertEqual(self.memory.db.connection.execute("SELECT content FROM messages").fetchall(), [("b",)])
        self.assertEqual(self.request("/v1/memory/delete-all", {"user": "a"})[0], 200)

    def test_content_debug_only_and_never_token(self):
        root = logging.getLogger()
        previous_level = root.level
        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        root.addHandler(handler)
        try:
            for level in ("info", "debug"):
                stream.seek(0)
                stream.truncate()
                with patch.dict("os.environ", {"ZOE_LOG_LEVEL": level}):
                    configure_logging()
                self.write([{"op": "add", "text": "private-fact"}])
                logging.getLogger("mem0.memory.main").info("private-mem0")
                self.request("/healthz", token="wrong-token", method="GET")
                output = stream.getvalue()
                for content in ("private-sentence", "private-query", "private-fact", "private-mem0"):
                    self.assertEqual(content in output, level == "debug", output)
                self.assertNotIn("secret-marker", output)
                self.assertNotIn("wrong-token", output)
                self.assertIn("memory_api request kind=write status=200 elapsed_ms=", output)
        finally:
            root.removeHandler(handler)
            root.setLevel(previous_level)

    def test_listener_disabled_without_token_file(self):
        with patch.dict("os.environ", {"ZOE_MEMORY_API_TOKEN_FILE": ""}):
            with self.assertLogs("zoe.memory.api", level="INFO") as logs:
                self.assertIsNone(start_memory_api(None))
            self.assertEqual(len(logs.output), 1)

    def test_deadline_returns_json(self):
        def slow(*args):
            time.sleep(.15)
            return {"items": []}
        self.server.request_timeout = .05
        with patch.object(self.backend, "search", side_effect=slow):
            self.assertEqual(self.request("/v1/memory/search", {"user": "u", "query": "q"}),
                             (504, {"error_kind": "deadline_exceeded"}))
        time.sleep(.2)


if __name__ == "__main__":
    unittest.main()
