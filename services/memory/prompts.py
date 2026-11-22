"""Restart-only instruction snapshot; errors never expose prompt contents."""
from __future__ import annotations

from typing import Mapping
from .limits import defaults

import os
from dataclasses import dataclass
from pathlib import Path

NAMES = ("memory_ingest", "memory_write", "memory_custom")


@dataclass(frozen=True)
class MemoryPrompts:
    memory_ingest: str
    memory_write: str
    memory_custom: str


def load_prompts(directory: str | Path | None = None, limits: Mapping[str, int] | None = None) -> MemoryPrompts:
    max_bytes = (limits if limits is not None else defaults())["prompt_file_max_bytes"]
    directory = Path(directory if directory is not None else
                     os.environ.get("ZOE_PROMPT_DIR") or "/etc/zoe/prompts")
    values = {}
    for name in NAMES:
        try:
            with (directory / (name + ".txt")).open("rb") as source:
                data = source.read(max_bytes + 1)
        except FileNotFoundError:
            raise ValueError("prompt_missing: " + name) from None
        except OSError:
            raise ValueError("prompt_unreadable: " + name) from None
        if len(data) > max_bytes:
            raise ValueError("prompt_too_large: " + name)
        try:
            text = data.decode("utf-8").removesuffix("\n")
        except UnicodeError:
            raise ValueError("prompt_invalid_utf8: " + name) from None
        if not text:
            raise ValueError("prompt_empty: " + name)
        values[name] = text
    return MemoryPrompts(**values)
