from __future__ import annotations

import os
from dataclasses import dataclass


MODEL_ID = "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2"
MODEL_REVISION = "faf4aa4225822f3bc6376869cb1164e8e3feedd0"
MODEL_ONNX_REPOSITORY = "Qdrant/paraphrase-multilingual-MiniLM-L12-v2-onnx-Q"
EMBEDDING_DIMENSIONS = 384
MODEL_CODEC = "paraphrase_multilingual_minilm_l12_384_faf4aa42"
_REASONING_EFFORTS = {"none", "low", "medium", "high", "max"}


def _required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise ValueError(f"{name} is required")
    return value


@dataclass(frozen=True)
class WorkerConfig:
    control_url: str
    runner_token: str
    qdrant_url: str
    qdrant_api_key: str | None
    qdrant_collection: str
    mem0_collection: str
    embedder_model: str
    embedder_revision: str
    llm_provider: str
    llm_model: str
    llm_base_url: str | None
    llm_api_key: str | None
    reasoning_effort: str
    poll_seconds: float
    batch_size: int
    extractor_version: str
    deletion_only: bool = False

    @classmethod
    def from_env(cls) -> "WorkerConfig":
        deletion_only = os.environ.get("ZOE_MEMORY_DELETION_ONLY", "").strip().lower() in ("1", "true", "yes")
        if not deletion_only:
            os.environ.setdefault(
                "FASTEMBED_CACHE_PATH",
                os.environ.get("ZOE_MEMORY_MODEL_CACHE", "/var/lib/zoe/memory/models"),
            )
        provider = os.environ.get("ZOE_MEM0_LLM_PROVIDER", "openai").strip().lower()
        if not deletion_only and provider != "openai":
            raise ValueError("ZOE_MEM0_LLM_PROVIDER must be openai; other providers are not yet accounted")
        reasoning_effort = os.environ.get("ZOE_MEM0_LLM_REASONING_EFFORT", "").strip()
        if reasoning_effort and reasoning_effort not in _REASONING_EFFORTS:
            raise ValueError("ZOE_MEM0_LLM_REASONING_EFFORT must be one of none, low, medium, high, max, or empty")
        return cls(
            control_url=_required("ZOE_CONTROL_URL").rstrip("/"),
            runner_token=_required("ZOE_RUNNER_TOKEN"),
            qdrant_url=os.environ.get("ZOE_QDRANT_URL", "http://127.0.0.1:6333").rstrip("/"),
            qdrant_api_key=os.environ.get("ZOE_QDRANT_API_KEY") or None,
            qdrant_collection=os.environ.get("ZOE_QDRANT_COLLECTION", f"zoe_memory_v1_{MODEL_CODEC}"),
            mem0_collection=os.environ.get("ZOE_MEM0_COLLECTION", "zoe_mem0_v1"),
            embedder_model=MODEL_ID,
            embedder_revision=MODEL_REVISION,
            llm_provider=provider,
            llm_model="" if deletion_only else _required("ZOE_MEM0_LLM_MODEL"),
            llm_base_url=os.environ.get("ZOE_MEM0_LLM_BASE_URL") or None,
            llm_api_key=os.environ.get("ZOE_MEM0_LLM_API_KEY") or None,
            reasoning_effort=reasoning_effort,
            poll_seconds=max(0.1, min(float(os.environ.get("ZOE_MEMORY_POLL_SECONDS", "2")), 60.0)),
            batch_size=max(1, min(int(os.environ.get("ZOE_MEMORY_BATCH_SIZE", "10")), 100)),
            extractor_version=os.environ.get("ZOE_MEMORY_EXTRACTOR_VERSION", "mem0-controlled-v1"),
            deletion_only=deletion_only,
        )

    @property
    def embedding_dimensions(self) -> int:
        return EMBEDDING_DIMENSIONS
