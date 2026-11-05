from __future__ import annotations

import os
import sqlite3
from pathlib import Path
from datetime import datetime, timezone
from urllib.parse import urlparse
from typing import Any

# Zoe is self-hosted and source text may contain private user data. Mem0's
# optional anonymous telemetry is disabled before importing its module.
os.environ["MEM0_TELEMETRY"] = "false"

from mem0 import Memory

from .config import WorkerConfig
from .embedding import LocalEmbedder
from .mem0_embedder import register_pinned_embedder
from .responses_llm import (
    register_responses_provider,
    reset_model_call_context,
    reset_request_timeout,
    set_model_call_context,
    set_request_timeout,
)
from .stores import mem0_user_id

MAX_LLM_REQUEST_SECONDS = 60.0
LLM_FINALIZATION_RESERVE_SECONDS = 45.0


class LeaseBudgetError(RuntimeError):
    pass


def llm_timeout_for_job(job: dict[str, Any], now: datetime | None = None) -> float:
    lease_value = str(job.get("lease_until", "")).strip()
    if not lease_value:
        return MAX_LLM_REQUEST_SECONDS
    try:
        deadline = datetime.fromisoformat(lease_value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise LeaseBudgetError("invalid memory job lease deadline") from exc
    if deadline.tzinfo is None:
        raise LeaseBudgetError("memory job lease deadline has no timezone")
    current = now or datetime.now(timezone.utc)
    request_seconds = min(MAX_LLM_REQUEST_SECONDS, (deadline - current).total_seconds() - LLM_FINALIZATION_RESERVE_SECONDS)
    if request_seconds <= 0:
        raise LeaseBudgetError("memory job lease is too short for a bounded LLM request")
    return request_seconds


class ControlledMem0:
    def __init__(self, config: WorkerConfig, embedder: LocalEmbedder | None = None, control_api: Any = None):
        self.config = config
        self._control_api = control_api
        if embedder is None:
            embedder = LocalEmbedder(config.embedder_model, config.embedder_revision, config.embedding_dimensions)
        register_pinned_embedder(embedder)
        qdrant_address = urlparse(config.qdrant_url)
        if not qdrant_address.hostname:
            raise ValueError("ZOE_QDRANT_URL must include a host")
        vector_config: dict[str, Any] = {
            "collection_name": config.mem0_collection,
            "embedding_model_dims": config.embedding_dimensions,
            "host": qdrant_address.hostname,
            "port": qdrant_address.port or (443 if qdrant_address.scheme == "https" else 6333),
            "on_disk": True,
        }
        if config.qdrant_api_key:
            vector_config["api_key"] = config.qdrant_api_key
        embedder_config = {
            "provider": "fastembed",
            "config": {
                "model": config.embedder_model,
                "embedding_dims": config.embedding_dimensions,
            },
        }
        register_responses_provider()
        llm_config: dict[str, Any] = {
            "provider": "openai",
            "config": {
                "model": config.llm_model,
                "openai_base_url": config.llm_base_url,
                "api_key": config.llm_api_key,
                "temperature": 0.1,
            },
        }
        if config.reasoning_effort:
            llm_config["config"]["reasoning_effort"] = config.reasoning_effort
        history_path = Path(os.environ.get("ZOE_MEM0_HISTORY_DB", "/var/lib/zoe/memory/mem0-history.sqlite"))
        history_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.memory = Memory.from_config(
            {
                "version": "v1.1",
                "vector_store": {"provider": "qdrant", "config": vector_config},
                "embedder": embedder_config,
                "llm": llm_config,
                "history_db_path": str(history_path),
                "custom_instructions": (
                    "Extract only stable facts directly supported by the current source. "
                    "Never treat retrieved memories as evidence for a new fact. Do not infer preferences or identity."
                ),
            }
        )
        os.chmod(history_path, 0o600)
        self.memory.db.connection.execute("PRAGMA journal_mode=WAL")
        self.memory.db.connection.execute("PRAGMA synchronous=FULL")
        self.memory.db.connection.execute(
            """CREATE TABLE IF NOT EXISTS zoe_memory_ownership (
                user_id TEXT NOT NULL,
                workspace_id TEXT NOT NULL,
                claim_id TEXT NOT NULL,
                memory_id TEXT NOT NULL,
                PRIMARY KEY(user_id, memory_id)
            )"""
        )
        self._track_memory_ownership()

    def _remember_ownership(self, memory_id: str, metadata: dict[str, Any] | None) -> None:
        if not metadata:
            return
        user_id = str(metadata.get("user_id", ""))
        workspace = str(metadata.get("workspace_id", ""))
        claim_id = str(metadata.get("claim_id", ""))
        if not user_id or not workspace or not claim_id:
            return
        if user_id != mem0_user_id(workspace, claim_id):
            raise ValueError("Mem0 memory identity does not match its Zoe workspace and claim")
        db = self.memory.db
        with db._lock:
            db.connection.execute(
                "INSERT OR IGNORE INTO zoe_memory_ownership(user_id,workspace_id,claim_id,memory_id) VALUES(?,?,?,?)",
                (user_id, workspace, claim_id, str(memory_id)),
            )
            db.connection.commit()

    def _track_memory_ownership(self) -> None:
        vector_store = self.memory.vector_store
        original_insert = vector_store.insert

        def tracked_insert(*, vectors, ids, payloads, **kwargs):
            for memory_id, payload in zip(ids, payloads):
                self._remember_ownership(str(memory_id), payload)
            return original_insert(vectors=vectors, ids=ids, payloads=payloads, **kwargs)

        vector_store.insert = tracked_insert
        original_update = vector_store.update

        def tracked_update(*, vector_id, vector, payload, **kwargs):
            self._remember_ownership(str(vector_id), payload)
            return original_update(vector_id=vector_id, vector=vector, payload=payload, **kwargs)

        vector_store.update = tracked_update
        original_delete = self.memory._delete_memory

        def tracked_delete(memory_id, existing_memory=None, *args, **kwargs):
            item = existing_memory or vector_store.get(vector_id=memory_id)
            if item is not None:
                self._remember_ownership(str(memory_id), item.payload)
            return original_delete(memory_id, existing_memory, *args, **kwargs)

        self.memory._delete_memory = tracked_delete

    def extract(self, job: dict[str, Any]) -> list[dict[str, str]]:
        if self._control_api is None:
            raise RuntimeError("memory model-call accounting API is required")
        claim = job.get("claim") or {}
        source = job.get("source") or {}
        workspace = str(job["workspace_id"])
        user_id = mem0_user_id(workspace, str(job["entity_id"]))
        source_text = str(source.get("extractor_input", "")).strip()
        if not source_text:
            return []
        request_timeout = llm_timeout_for_job(job)
        timeout_token = set_request_timeout(request_timeout)
        call_token = set_model_call_context(self._control_api, job)
        # Explicit recall keeps Mem0's retrieval path exercised. The add call
        # also uses Mem0's own retrieval and update workflow for the same user.
        try:
            self.memory.search(source_text, filters={"user_id": user_id}, top_k=5)
            result = self.memory.add(
                source_text,
                user_id=user_id,
                metadata={
                    "workspace_id": workspace,
                    "claim_id": str(job["entity_id"]),
                    "claim_revision": int(job["entity_version"]),
                    "source_id": str(source.get("id", "")),
                    "source_version": int(source.get("version", 0)),
                },
                infer=True,
                prompt=(
                    "Extract only concise facts that the CURRENT SOURCE text supports. "
                    "Use recalled memories only to recognize duplicates or updates; never use them as evidence. "
                    "Return facts as short declarative statements.\nCURRENT SOURCE:\n" + source_text
                ),
            )
        finally:
            reset_model_call_context(call_token)
            reset_request_timeout(timeout_token)
        candidates: list[dict[str, str]] = []
        for item in result.get("results", []):
            if not isinstance(item, dict) or item.get("event") not in {"ADD", "UPDATE"}:
                continue
            content = str(item.get("memory", "")).strip()
            if content:
                candidates.append(
                    {
                        "subject": str(claim.get("subject", "source"))[:200] or "source",
                        "key": str(claim.get("key", "derived_fact"))[:200] or "derived_fact",
                        "content": content,
                    }
                )
        return candidates

    def delete_claim(self, workspace: str, claim_id: str) -> None:
        user_id = mem0_user_id(workspace, claim_id)
        filters = {"user_id": user_id}

        # Mem0's delete_all removes vector records but intentionally records
        # DELETE events in its history database. Capture only this claim's IDs
        # first so we can erase its history after Mem0 has deleted its vectors.
        # The same claim-scoped user ID is the session scope for source
        # messages, so those rows can be removed without touching other claims.
        db = self.memory.db
        with db._lock:
            memory_ids = {
                str(row[0])
                for row in db.connection.execute(
                    "SELECT memory_id FROM zoe_memory_ownership WHERE user_id=? AND workspace_id=? AND claim_id=?",
                    (user_id, workspace, claim_id),
                ).fetchall()
            }
        seen_batches: set[tuple[str, ...]] = set()
        while True:
            memories = self.memory.vector_store.list(filters=filters, top_k=100)[0]
            if not memories:
                break
            batch_ids = tuple(sorted(str(memory.id) for memory in memories))
            if batch_ids in seen_batches:
                raise RuntimeError("Mem0 returned a repeated claim-scoped delete batch")
            seen_batches.add(batch_ids)
            memory_ids.update(batch_ids)
            for memory in memories:
                self._remember_ownership(str(memory.id), memory.payload)
                self.memory.delete(str(memory.id))

        # Mem0 2.2.x has no scoped history/messages purge API. Keep this
        # compatibility shim narrow: only IDs found under the deterministic
        # claim user ID and the exact matching session_scope are removed.
        with db._lock:
            try:
                db.connection.execute("BEGIN")
                if memory_ids:
                    db.connection.executemany(
                        "DELETE FROM history WHERE memory_id = ?",
                        ((memory_id,) for memory_id in memory_ids),
                    )
                db.connection.execute(
                    "DELETE FROM messages WHERE session_scope = ?",
                    (f"user_id={user_id}",),
                )
                db.connection.execute(
                    "DELETE FROM zoe_memory_ownership WHERE user_id=? AND workspace_id=? AND claim_id=?",
                    (user_id, workspace, claim_id),
                )
                db.connection.execute("COMMIT")
            except sqlite3.Error:
                db.connection.execute("ROLLBACK")
                raise
