"""Restart-only instruction snapshot; errors never expose prompt contents."""
from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

NAMES = ("memory_ingest", "memory_write", "memory_custom")
MAX_BYTES = 16000


@dataclass(frozen=True)
class MemoryPrompts:
    memory_ingest: str
    memory_write: str
    memory_custom: str


def load_prompts(directory: str | Path | None = None) -> MemoryPrompts:
    directory = Path(directory if directory is not None else
                     os.environ.get("ZOE_PROMPT_DIR") or "/etc/zoe/prompts")
    values = {}
    for name in NAMES:
        try:
            with (directory / (name + ".txt")).open("rb") as source:
                data = source.read(MAX_BYTES + 1)
        except FileNotFoundError:
            raise ValueError("prompt_missing: " + name) from None
        except OSError:
            raise ValueError("prompt_unreadable: " + name) from None
        if len(data) > MAX_BYTES:
            raise ValueError("prompt_too_large: " + name)
        try:
            text = data.decode("utf-8").removesuffix("\n")
        except UnicodeError:
            raise ValueError("prompt_invalid_utf8: " + name) from None
        if not text:
            raise ValueError("prompt_empty: " + name)
        values[name] = text
    return MemoryPrompts(**values)
