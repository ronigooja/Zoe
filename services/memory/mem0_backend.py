from __future__ import annotations

import os
from pathlib import Path
from urllib.parse import urlparse
from typing import Any

# Disable optional telemetry before importing Mem0: source text is private.
os.environ["MEM0_TELEMETRY"] = "false"

from mem0 import Memory

from .config import WorkerConfig
from .embedding import LocalEmbedder
from .mem0_embedder import register_pinned_embedder
from .responses_llm import register_responses_provider


class ControlledMem0:
    """Construct the pinned Mem0 backend used by the person memory API."""

    def __init__(self, config: WorkerConfig, embedder: LocalEmbedder | None = None):
        self.config = config
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
        self.memory = Memory.from_config({
            "version": "v1.1",
            "vector_store": {"provider": "qdrant", "config": vector_config},
            "embedder": {
                "provider": "fastembed",
                "config": {"model": config.embedder_model, "embedding_dims": config.embedding_dimensions},
            },
            "llm": llm_config,
            "history_db_path": str(history_path),
            "custom_instructions": (
                "Extract only stable facts directly supported by the current source. "
                "Never treat retrieved memories as evidence for a new fact. Do not infer preferences or identity."
            ),
        })
        os.chmod(history_path, 0o600)
        self.memory.db.connection.execute("PRAGMA journal_mode=WAL")
        self.memory.db.connection.execute("PRAGMA synchronous=FULL")
