"""Person-scoped Mem0 operations for the HTTP API."""
from __future__ import annotations

from typing import Mapping
from .limits import defaults

import hashlib
import json
import logging
import threading
import time
import uuid
from contextlib import contextmanager
from typing import Any

from .prompts import MemoryPrompts

LOG = logging.getLogger("zoe.memory.api")


class _ManifestReady(BaseException):
    """Stop pinned Mem0 before dispatch (its Exception fallback must not catch this)."""


def _canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


class MemoryAPIError(Exception):
    def __init__(self, status: int, kind: str):
        self.status = status
        self.kind = kind


def person_user_id(user: str) -> str:
    """The caller's opaque person ID encodes instance lineage and owner, never a claim."""
    return "zoe-person-" + hashlib.sha256(user.encode("utf-8")).hexdigest()


def check_deadline(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise MemoryAPIError(504, "deadline_exceeded")
    return remaining


def _text(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip()) and value == value.strip()


def validate_operations(raw: Any, limits: Mapping[str, int] | None = None) -> list[dict[str, str]]:
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError
            result[key] = value
        return result

    try:
        value = json.loads(raw, object_pairs_hook=unique_object) if isinstance(raw, str) else raw
        if not isinstance(value, dict) or set(value) != {"operations"}:
            raise ValueError
        operations = value["operations"]
        if not isinstance(operations, list) or len(operations) > (limits if limits is not None else defaults())["memory_write_max_operations"]:
            raise ValueError
        for item in operations:
            if not isinstance(item, dict) or item.get("op") not in {"add", "replace", "none"}:
                raise ValueError
            keys = {"op", "text", "existing"} if item["op"] == "replace" else {"op", "text"}
            if set(item) != keys or not _text(item["text"]):
                raise ValueError
            if item["op"] == "replace" and not _text(item["existing"]):
                raise ValueError
        return operations
    except (ValueError, TypeError, RecursionError):
        raise MemoryAPIError(502, "output_invalid") from None


class PersonMemory:
    """One worker serializes its calls; receipts fence progress with SQLite CAS.

    When reconciling effects, exact payload comparisons detect observed conflicts,
    not concurrent writers between read and upsert. Deploy one writer per store;
    Qdrant has no compare-and-swap and this is not distributed atomicity.
    """
    def __init__(self, memory: Any, prompts: MemoryPrompts, limits: Mapping[str, int] | None = None):
        self.limits = limits if limits is not None else defaults()
        from .auth_tracking import attach_auth_tracker
        self.memory = memory
        self.prompts = prompts
        self._lock = threading.Lock()
        self._auth_rejected = attach_auth_tracker(memory.llm)
        self._ingest_context = None
        self._ingest_facts = None
        self._ingest_hashes = set()
        self._ingest_existing = []
        self._write_context = None
        # History has no user column in Mem0 2.2.1. Keep IDs, not content, so
        # even deleted vectors' history remains safely attributable on retry.
        db = memory.db
        with db._lock:
            db.connection.execute(
                "CREATE TABLE IF NOT EXISTS zoe_person_write_effects "
                "(user_id TEXT NOT NULL, op_id TEXT NOT NULL, position INTEGER NOT NULL, intent TEXT NOT NULL, "
                "PRIMARY KEY(user_id,op_id,position))"
            )
            db.connection.execute(
                "CREATE TABLE IF NOT EXISTS zoe_person_ingest_manifests "
                "(user_id TEXT NOT NULL, chunk_id TEXT NOT NULL, manifest TEXT NOT NULL, "
                "PRIMARY KEY(user_id,chunk_id))"
            )
            db.connection.execute(
                "CREATE TABLE IF NOT EXISTS zoe_person_memory_ownership "
                "(user_id TEXT NOT NULL, memory_id TEXT NOT NULL, PRIMARY KEY(user_id,memory_id))"
            )
            db.connection.execute(
                "CREATE TABLE IF NOT EXISTS zoe_person_ingest_receipts "
                "(user_id TEXT NOT NULL, chunk_id TEXT NOT NULL, batch_id TEXT NOT NULL, "
                "content_hash TEXT NOT NULL, status TEXT NOT NULL, result TEXT, "
                "PRIMARY KEY(user_id,chunk_id))"
            )
            db.connection.execute(
                "CREATE TABLE IF NOT EXISTS zoe_person_write_receipts "
                "(user_id TEXT NOT NULL, op_id TEXT NOT NULL, content_hash TEXT NOT NULL, "
                "plan TEXT, progress INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, result TEXT, "
                "PRIMARY KEY(user_id,op_id))"
            )
            db.connection.commit()
        original_insert = memory.vector_store.insert

        def insert(*, vectors, ids, payloads, **kwargs):
            if self._write_context is not None:
                if len(ids) != 1 or len(vectors) != 1 or len(payloads) != 1:
                    raise MemoryAPIError(502, "write_plan_invalid")
                self._capture_write(vectors[0], payloads[0])
            if self._ingest_context is not None:
                user_id, batch_id, chunk_id, content_hash = self._ingest_context
                if not (len(ids) == len(vectors) == len(payloads)) or not ids:
                    raise MemoryAPIError(502, "manifest_invalid")
                if len({str(value) for value in ids}) != len(ids):
                    raise MemoryAPIError(502, "manifest_invalid")
                records = []
                for memory_id, vector, payload in zip(ids, vectors, payloads):
                    if payload.get("user_id") != user_id:
                        raise MemoryAPIError(409, "manifest_conflict")
                    payload = dict(payload, zoe_batch_id=batch_id, zoe_chunk_id=chunk_id,
                                   zoe_source_hash=content_hash)
                    records.append({"id": str(memory_id), "vector": list(vector), "payload": payload})
                expected = self._expected_ingest_hashes()
                if {r["payload"].get("hash") for r in records} != expected:
                    raise MemoryAPIError(502, "ingest_incomplete_plan")
                with db._lock:
                    db.connection.execute(
                        "INSERT INTO zoe_person_ingest_manifests VALUES(?,?,?)",
                        (user_id, chunk_id, _canonical(records)))
                    db.connection.execute(
                        "UPDATE zoe_person_ingest_receipts SET status='manifest' WHERE user_id=? AND chunk_id=?",
                        (user_id, chunk_id))
                    db.connection.commit()
                raise _ManifestReady()
            for memory_id, payload in zip(ids, payloads):
                self._remember(str(memory_id), payload)
            return original_insert(vectors=vectors, ids=ids, payloads=payloads, **kwargs)

        memory.vector_store.insert = insert
        self._dispatch_insert = original_insert
        original_update = memory.vector_store.update

        def update(vector_id, vector=None, payload=None, **kwargs):
            if self._write_context is not None:
                if str(vector_id) != self._write_context["id"] or vector is None or payload is None:
                    raise MemoryAPIError(502, "write_plan_invalid")
                self._capture_write(vector, payload)
            return original_update(vector_id=vector_id, vector=vector, payload=payload, **kwargs)

        memory.vector_store.update = update
        original_search = memory.vector_store.search

        def vector_search(*args, **kwargs):
            results = original_search(*args, **kwargs)
            if self._ingest_context is not None:
                self._ingest_hashes = {item.payload.get("hash") for item in results}
                self._ingest_existing = [
                    {"id": str(index), "text": item.payload.get("data", "")}
                    for index, item in enumerate(results)
                ]
            return results

        memory.vector_store.search = vector_search
        original_delete = memory._delete_memory

        def delete(memory_id, existing_memory=None, *args, **kwargs):
            item = existing_memory or memory.vector_store.get(vector_id=memory_id)
            if item is not None:
                self._remember(str(memory_id), item.payload)
            return original_delete(memory_id, existing_memory, *args, **kwargs)

        memory._delete_memory = delete

    def _expected_ingest_hashes(self):
        if self._ingest_facts is None:
            raise MemoryAPIError(502, "ingest_unplanned")
        return {hashlib.md5(fact["text"].encode()).hexdigest() for fact in self._ingest_facts} - self._ingest_hashes

    def _capture_write(self, vector, payload):
        context = self._write_context
        if payload.get("user_id") != context["user_id"] or payload.get("data") != context["text"]:
            raise MemoryAPIError(409, "write_plan_conflict")
        if payload.get("hash") != hashlib.md5(context["text"].encode()).hexdigest():
            raise MemoryAPIError(409, "write_plan_conflict")
        payload = dict(payload, zoe_write_token=context["token"])
        intent = {"id": context["id"], "preimage": context["preimage"],
                  "payload": payload, "vector": list(vector)}
        db = self.memory.db
        with db._lock:
            db.connection.execute("INSERT INTO zoe_person_write_effects VALUES(?,?,?,?)",
                                  (context["user_id"], context["op_id"], context["position"], _canonical(intent)))
            db.connection.execute("UPDATE zoe_person_write_receipts SET status='applying' WHERE user_id=? AND op_id=? AND progress=? AND status='ready'",
                                  (context["user_id"], context["op_id"], context["position"]))
            db.connection.commit()
        raise _ManifestReady()

    def _remember(self, memory_id: str, payload: dict[str, Any]) -> None:
        user_id = payload.get("user_id", "")
        if not isinstance(user_id, str) or not user_id.startswith("zoe-person-"):
            return
        db = self.memory.db
        with db._lock:
            db.connection.execute(
                "INSERT OR IGNORE INTO zoe_person_memory_ownership VALUES (?,?)", (user_id, memory_id)
            )
            db.connection.commit()

    @contextmanager
    def _serialized(self, deadline: float):
        from .auth_tracking import check_auth
        if not self._lock.acquire(timeout=check_deadline(deadline)):
            raise MemoryAPIError(504, "deadline_exceeded")
        self._auth_rejected.clear()
        try:
            check_deadline(deadline)
            try:
                yield
            finally:
                check_auth(self._auth_rejected)
        finally:
            self._lock.release()

    def _search(self, user: str, query: str, top_k: int, deadline: float) -> list[dict[str, Any]]:
        check_deadline(deadline)
        result = self.memory.search(query, filters={"user_id": person_user_id(user)}, top_k=top_k, rerank=False)
        items = [{"id": item["id"], "text": item["memory"], "score": item.get("score", 0.0)}
                 for item in result.get("results", [])]
        LOG.debug("memory_api search query=%r results=%r", query, items)
        return items

    def search(self, user: str, query: str, top_k: int, deadline: float) -> dict[str, Any]:
        with self._serialized(deadline):
            return {"items": self._search(user, query, top_k, deadline)}

    def write(self, user: str, op_id: str, request: str, deadline: float) -> dict[str, Any]:
        from .responses_llm import (
            reset_request_timeout, set_request_timeout,
        )

        with self._serialized(deadline):
            db = self.memory.db
            user_id = person_user_id(user)
            content_hash = hashlib.sha256(request.encode("utf-8")).hexdigest()
            with db._lock:
                row = db.connection.execute(
                    "SELECT content_hash,plan,progress,status,result FROM zoe_person_write_receipts "
                    "WHERE user_id=? AND op_id=?", (user_id, op_id)
                ).fetchone()
                if row:
                    if row[0] != content_hash:
                        raise MemoryAPIError(409, "idempotency_conflict")
                    if row[3] == "done":
                        return json.loads(row[4])
                    if row[3] == "quarantined":
                        raise MemoryAPIError(409, "write_quarantined")
                    if row[3] == "applying":
                        effect = db.connection.execute(
                            "SELECT 1 FROM zoe_person_write_effects WHERE user_id=? AND op_id=? AND position=?",
                            (user_id, op_id, row[2])).fetchone()
                        if effect is None:
                            db.connection.execute("UPDATE zoe_person_write_receipts SET status='quarantined' WHERE user_id=? AND op_id=?",
                                                  (user_id, op_id))
                            db.connection.commit()
                            raise MemoryAPIError(409, "write_quarantined")
                else:
                    db.connection.execute(
                        "INSERT INTO zoe_person_write_receipts(user_id,op_id,content_hash,status) "
                        "VALUES(?,?,?,'planning')", (user_id, op_id, content_hash)
                    )
                    db.connection.commit()
            if row and row[1] is not None:
                return self._apply_write(user_id, op_id, json.loads(row[1]), row[2], deadline)
            existing = self._search(user, request, self.limits["memory_write_recall_rows"], deadline)
            messages = [
                {"role": "system", "content": self.prompts.memory_write},
                {"role": "user", "content": json.dumps({
                    "request": request,
                    "existing_memories": [item["text"] for item in existing],
                }, ensure_ascii=False)},
            ]
            LOG.debug("memory_api write model_input=%r", messages)
            timeout_token = set_request_timeout(check_deadline(deadline))
            started = time.monotonic()
            status, error_kind = "error", "backend_unavailable"
            from .model_observation import set_component, reset_component
            observation_token = set_component("memory_write")
            try:
                raw = self.memory.llm.generate_response(messages, response_format={"type": "json_object"})
                status, error_kind = "ok", "none"
            except TimeoutError:
                error_kind = "deadline_exceeded"
                raise
            finally:
                reset_component(observation_token)
                reset_request_timeout(timeout_token)
                LOG.info("memory_api llm_call component=memory_write status=%s error_kind=%s elapsed_ms=%d",
                         status, error_kind, (time.monotonic() - started) * 1000)
            LOG.debug("memory_api write model_output=%r", raw)
            operations = validate_operations(raw, self.limits)  # Validate the ENTIRE output before any write.
            resolved = []
            for item in operations:
                operation = {"op": item["op"], "text": item["text"]}
                if item["op"] == "replace":
                    matches = [old["id"] for old in existing if old["text"] == item["existing"]]
                    if len(matches) == 1:
                        operation["id"] = matches[0]
                        current = self.memory.vector_store.get(vector_id=matches[0])
                        if current is None or current.payload.get("user_id") != user_id or current.payload.get("data") != item["existing"]:
                            self._quarantine_write(user_id, op_id)
                        operation["preimage"] = json.loads(_canonical(current.payload))
                    else:
                        operation["op"] = "add"
                resolved.append(operation)
            with db._lock:
                saved = db.connection.execute(
                    "UPDATE zoe_person_write_receipts SET plan=?,status='ready' WHERE user_id=? AND op_id=? AND status='planning'",
                    (json.dumps(resolved), user_id, op_id)
                )
                db.connection.commit()
                if saved.rowcount != 1:
                    raise MemoryAPIError(409, "write_uncertain")
            return self._apply_write(user_id, op_id, resolved, 0, deadline)

    def _apply_write(self, user_id: str, op_id: str, plan: list, progress: int, deadline: float):
        db = self.memory.db
        for position in range(progress, len(plan)):
            check_deadline(deadline)
            item = plan[position]
            token = str(uuid.uuid5(uuid.NAMESPACE_URL, _canonical([user_id, op_id, position])))
            with db._lock:
                saved = db.connection.execute(
                    "SELECT intent FROM zoe_person_write_effects WHERE user_id=? AND op_id=? AND position=?",
                    (user_id, op_id, position)).fetchone()
                claimed = db.connection.execute(
                    "UPDATE zoe_person_write_receipts SET status=CASE WHEN ? THEN 'applying' ELSE 'ready' END "
                    "WHERE user_id=? AND op_id=? AND status IN ('ready','applying') AND progress=?",
                    (bool(saved), user_id, op_id, position)
                )
                db.connection.commit()
                if claimed.rowcount != 1:
                    raise MemoryAPIError(409, "write_uncertain")
            if item["op"] != "none":
                if not saved:
                    preimage = None
                    memory_id = token if item["op"] == "add" else item["id"]
                    if item["op"] == "replace":
                        current = self.memory.vector_store.get(vector_id=memory_id)
                        if current is None or current.payload.get("user_id") != user_id:
                            self._quarantine_write(user_id, op_id)
                        preimage = item.get("preimage")
                        if preimage is None or _canonical(current.payload) != _canonical(preimage):
                            self._quarantine_write(user_id, op_id)
                    self._write_context = {"user_id": user_id, "op_id": op_id, "position": position,
                                           "id": memory_id, "preimage": preimage, "token": token,
                                           "text": item["text"]}
                    try:
                        try:
                            if item["op"] == "add":
                                self.memory.add(item["text"], user_id=user_id, infer=False)
                            else:
                                self.memory.update(memory_id, item["text"])
                        except _ManifestReady:
                            pass
                    finally:
                        self._write_context = None
                    with db._lock:
                        saved = db.connection.execute(
                            "SELECT intent FROM zoe_person_write_effects WHERE user_id=? AND op_id=? AND position=?",
                            (user_id, op_id, position)).fetchone()
                    if not saved:
                        raise MemoryAPIError(502, "write_plan_missing")
                intent = json.loads(saved[0])
                if (intent["payload"].get("user_id") != user_id or
                        intent["payload"].get("zoe_write_token") != token or
                        intent["payload"].get("data") != item["text"] or
                        intent["payload"].get("hash") != hashlib.md5(item["text"].encode()).hexdigest()):
                    self._quarantine_write(user_id, op_id)
                if (intent["id"] != (token if item["op"] == "add" else item["id"]) or
                        _canonical(intent["preimage"]) != _canonical(item.get("preimage"))):
                    self._quarantine_write(user_id, op_id)
                current = self.memory.vector_store.get(vector_id=intent["id"])
                actual = current.payload if current else None
                if _canonical(actual) != _canonical(intent["payload"]):
                    if _canonical(actual) != _canonical(intent["preimage"]):
                        self._quarantine_write(user_id, op_id)
                    check_deadline(deadline)
                    # This preimage check is NOT Qdrant CAS. Other processes
                    # must not write concurrently; a racing writer can still win.
                    self._remember(intent["id"], intent["payload"])
                    self._dispatch_insert(vectors=[intent["vector"]], ids=[intent["id"]],
                                          payloads=[intent["payload"]])
                current = self.memory.vector_store.get(vector_id=intent["id"])
                if current is None or _canonical(current.payload) != _canonical(intent["payload"]):
                    self._quarantine_write(user_id, op_id)
                item["id"] = intent["id"]
            check_deadline(deadline)
            with db._lock, db.connection:
                if item["op"] != "none":
                    db.connection.execute(
                        "INSERT OR IGNORE INTO history(id,memory_id,old_memory,new_memory,event,created_at,is_deleted) "
                        "VALUES(?,?,?,?,?,?,0)", (token, intent["id"],
                        intent["preimage"].get("data") if intent["preimage"] else None,
                        item["text"], "ADD" if item["op"] == "add" else "UPDATE",
                        intent["payload"].get("created_at")))
                db.connection.execute(
                    "UPDATE zoe_person_write_receipts SET plan=?,progress=?,status='ready' "
                    "WHERE user_id=? AND op_id=?", (json.dumps(plan), position + 1, user_id, op_id)
                )
                db.connection.commit()
        outcome = {"operations": [{k: v for k, v in item.items() if k != "preimage"} for item in plan]}
        with db._lock:
            db.connection.execute(
                "UPDATE zoe_person_write_receipts SET status='done',result=? WHERE user_id=? AND op_id=?",
                (json.dumps(outcome), user_id, op_id)
            )
            db.connection.commit()
        return outcome

    def _quarantine_write(self, user_id, op_id):
        db = self.memory.db
        with db._lock:
            db.connection.execute("UPDATE zoe_person_write_receipts SET status='quarantined' WHERE user_id=? AND op_id=?",
                                  (user_id, op_id))
            db.connection.commit()
        raise MemoryAPIError(409, "write_quarantined")

    def ingest(self, user: str, batch_id: str, chunk_id: str, text: str, deadline: float) -> dict[str, int]:
        from .responses_llm import reset_request_timeout, set_request_timeout

        with self._serialized(deadline):
            user_id = person_user_id(user)
            content_hash = hashlib.sha256(text.encode("utf-8")).hexdigest()
            db = self.memory.db
            with db._lock:
                row = db.connection.execute(
                    "SELECT batch_id,content_hash,status,result FROM zoe_person_ingest_receipts "
                    "WHERE user_id=? AND chunk_id=?", (user_id, chunk_id)
                ).fetchone()
                if row:
                    if row[0] != batch_id or row[1] != content_hash:
                        raise MemoryAPIError(409, "idempotency_conflict")
                    if row[2] == "done":
                        return json.loads(row[3])
                    if row[2] not in {"planning", "manifest"}:
                        db.connection.execute(
                            "UPDATE zoe_person_ingest_receipts SET status='quarantined' WHERE user_id=? AND chunk_id=?",
                            (user_id, chunk_id))
                        db.connection.commit()
                        raise MemoryAPIError(409, "ingest_quarantined")
                else:
                    db.connection.execute(
                        "INSERT INTO zoe_person_ingest_receipts VALUES (?,?,?,?,?,NULL)",
                        (user_id, chunk_id, batch_id, content_hash, "planning"))
                db.connection.commit()
                saved = db.connection.execute(
                    "SELECT manifest FROM zoe_person_ingest_manifests WHERE user_id=? AND chunk_id=?",
                    (user_id, chunk_id)).fetchone()
            if saved:
                return self._recover_ingest(user_id, batch_id, chunk_id, content_hash, text,
                                            json.loads(saved[0]), deadline)
            if row and row[2] == "manifest":
                raise MemoryAPIError(409, "ingest_manifest_missing")
            LOG.debug("memory_api ingest model_input=%r instructions=%r", text, self.prompts.memory_ingest)
            timeout_token = set_request_timeout(check_deadline(deadline))
            started = time.monotonic()
            status, error_kind = "error", "backend_unavailable"
            original_generate = self.memory.llm.generate_response
            original_save_messages = db.save_messages
            extracted = None

            def generate(messages, **kwargs):
                nonlocal extracted
                # Keep Mem0's retrieval and persistence, but own the actual model input.
                # In particular, its default system prompt enables assistant extraction.
                raw = original_generate(messages=[
                    {"role": "system", "content": self.prompts.memory_ingest},
                    {"role": "user", "content": json.dumps({
                        "transcript": text,
                        "existing_memories": self._ingest_existing,
                    }, ensure_ascii=False)},
                ], **kwargs)
                # Fail closed before Mem0's permissive parser can turn malformed
                # model output into a successful empty extraction.
                try:
                    value = json.loads(raw)
                    facts = value["memory"]
                    if not isinstance(facts, list) or any(
                        not isinstance(fact, dict) or not _text(fact.get("text")) for fact in facts
                    ):
                        raise ValueError
                except (ValueError, TypeError, KeyError):
                    raise MemoryAPIError(502, "output_invalid") from None
                extracted = facts
                self._ingest_facts = facts
                LOG.debug("memory_api ingest model_output=%r", raw)
                return raw

            self.memory.llm.generate_response = generate
            # Recovery owns message persistence, including the empty/dedup path.
            db.save_messages = lambda *args, **kwargs: None
            self._ingest_context = (user_id, batch_id, chunk_id, content_hash)
            self._ingest_facts = None
            self._ingest_hashes = set()
            self._ingest_existing = []
            from .model_observation import set_component, reset_component
            observation_token = set_component("memory_ingestion")
            try:
                try:
                    self.memory.add(text, user_id=user_id, infer=True)
                except _ManifestReady:
                    pass
                from .auth_tracking import check_auth
                check_auth(self._auth_rejected)
                check_deadline(deadline)
                status, error_kind = "ok", "none"
            except TimeoutError:
                error_kind = "deadline_exceeded"
                raise
            except MemoryAPIError as exc:
                error_kind = exc.kind
                raise
            finally:
                reset_component(observation_token)
                self._ingest_context = None
                self._ingest_existing = []
                self.memory.llm.generate_response = original_generate
                db.save_messages = original_save_messages
                reset_request_timeout(timeout_token)
                LOG.info("memory_api llm_call component=memory_ingest status=%s error_kind=%s elapsed_ms=%d",
                         status, error_kind, (time.monotonic() - started) * 1000)
            with db._lock:
                saved = db.connection.execute(
                    "SELECT manifest FROM zoe_person_ingest_manifests WHERE user_id=? AND chunk_id=?",
                    (user_id, chunk_id)).fetchone()
                if not saved:
                    if extracted is None or self._expected_ingest_hashes():
                        # Nonempty extraction without a manifest may indicate
                        # embedding loss, not just deduplication. Never claim it.
                        raise MemoryAPIError(502, "ingest_unplanned")
                    db.connection.execute("INSERT INTO zoe_person_ingest_manifests VALUES(?,?,?)",
                                          (user_id, chunk_id, "[]"))
                    db.connection.execute(
                        "UPDATE zoe_person_ingest_receipts SET status='manifest' WHERE user_id=? AND chunk_id=?",
                        (user_id, chunk_id))
                    db.connection.commit()
            return self._recover_ingest(user_id, batch_id, chunk_id, content_hash, text,
                                        json.loads(saved[0]) if saved else [], deadline)

    def _recover_ingest(self, user_id, batch_id, chunk_id, content_hash, text, records, deadline):
        """Reconcile primary vectors, then atomically checkpoint local bookkeeping.

        Missing IDs are safe same-ID upserts, not a second inference. Auxiliary
        entity links are deliberately not part of this primary-record receipt.
        """
        for record in records:
            payload = record["payload"]
            if (payload.get("user_id"), payload.get("zoe_batch_id"), payload.get("zoe_chunk_id"),
                payload.get("zoe_source_hash")) != (user_id, batch_id, chunk_id, content_hash):
                self._quarantine_ingest(user_id, chunk_id)
        # Check all existing IDs before writing any missing ID.
        missing = []
        for record in records:
            check_deadline(deadline)
            current = self.memory.vector_store.get(vector_id=record["id"])
            if current is None:
                missing.append(record)
            elif _canonical(current.payload) != _canonical(record["payload"]):
                self._quarantine_ingest(user_id, chunk_id)
        for record in missing:
            check_deadline(deadline)
            self._remember(record["id"], record["payload"])
            self._dispatch_insert(vectors=[record["vector"]], ids=[record["id"]],
                                  payloads=[record["payload"]])
        for record in records:
            check_deadline(deadline)
            current = self.memory.vector_store.get(vector_id=record["id"])
            if current is not None and _canonical(current.payload) != _canonical(record["payload"]):
                self._quarantine_ingest(user_id, chunk_id)
            if current is None:
                raise MemoryAPIError(409, "ingest_not_verified")
        outcome = {"added": len(records), "updated": 0}
        db = self.memory.db
        # The deterministic history ID and receipt share a SQLite transaction.
        # A process crash rolls both back; no second history event is invented.
        with db._lock:
            try:
                db.connection.execute("BEGIN")
                for record in records:
                    payload = record["payload"]
                    history_id = str(uuid.uuid5(uuid.NAMESPACE_URL, _canonical(
                        [user_id, batch_id, chunk_id, record["id"]])))
                    db.connection.execute(
                        "INSERT OR IGNORE INTO history "
                        "(id,memory_id,old_memory,new_memory,event,created_at,is_deleted) VALUES(?,?,NULL,?,'ADD',?,0)",
                        (history_id, record["id"], payload["data"], payload.get("created_at")))
                scope = f"user_id={user_id}"
                message_id = str(uuid.uuid5(uuid.NAMESPACE_URL, _canonical([user_id, batch_id, chunk_id])))
                db.connection.execute(
                    "INSERT OR IGNORE INTO messages(id,session_scope,role,content,created_at) "
                    "VALUES(?,?,'user',?,strftime('%Y-%m-%dT%H:%M:%f+00:00','now'))",
                    (message_id, scope, text))
                db.connection.execute(
                    "DELETE FROM messages WHERE session_scope=? AND id NOT IN "
                    "(SELECT id FROM messages WHERE session_scope=? ORDER BY created_at DESC,id DESC LIMIT ?)",
                    (scope, scope, self.limits["memory_recent_rows"]))
                db.connection.execute(
                    "UPDATE zoe_person_ingest_receipts SET status='done',result=? WHERE user_id=? AND chunk_id=?",
                    (json.dumps(outcome), user_id, chunk_id))
                db.connection.commit()
            except BaseException:
                db.connection.rollback()
                raise
        return outcome

    def _quarantine_ingest(self, user_id, chunk_id):
        db = self.memory.db
        with db._lock:
            db.connection.execute("UPDATE zoe_person_ingest_receipts SET status='quarantined' WHERE user_id=? AND chunk_id=?",
                                  (user_id, chunk_id))
            db.connection.commit()
        raise MemoryAPIError(409, "ingest_quarantined")

    def delete_all(self, user: str, deadline: float) -> dict[str, Any]:
        with self._serialized(deadline):
            user_id = person_user_id(user)
            self.memory.delete_all(user_id=user_id)
            check_deadline(deadline)
            # Verify Mem0 did not stop early on a repeated vector batch.
            if self.memory.vector_store.list(filters={"user_id": user_id}, top_k=1)[0]:
                raise MemoryAPIError(503, "delete_incomplete")
            db = self.memory.db
            with db._lock:
                try:
                    db.connection.execute("BEGIN")
                    db.connection.execute(
                        "DELETE FROM history WHERE memory_id IN "
                        "(SELECT memory_id FROM zoe_person_memory_ownership WHERE user_id=?)", (user_id,)
                    )
                    db.connection.execute("DELETE FROM messages WHERE session_scope=?", (f"user_id={user_id}",))
                    db.connection.execute("DELETE FROM zoe_person_memory_ownership WHERE user_id=?", (user_id,))
                    db.connection.execute("DELETE FROM zoe_person_ingest_receipts WHERE user_id=?", (user_id,))
                    db.connection.execute("DELETE FROM zoe_person_ingest_manifests WHERE user_id=?", (user_id,))
                    db.connection.execute("DELETE FROM zoe_person_write_receipts WHERE user_id=?", (user_id,))
                    db.connection.execute("DELETE FROM zoe_person_write_effects WHERE user_id=?", (user_id,))
                    db.connection.commit()
                except Exception:
                    db.connection.rollback()
                    raise
            return {"deleted": True}

    def health(self, deadline: float) -> dict[str, Any]:
        with self._serialized(deadline):
            self.memory.vector_store.list(filters={"user_id": person_user_id("healthz")}, top_k=1)
            with self.memory.db._lock:
                self.memory.db.connection.execute("SELECT 1").fetchone()
            return {"status": "ok"}
