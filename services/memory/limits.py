"""Read the shared Zoe limits once before starting the memory API."""
from __future__ import annotations

import json
import os
import stat
from pathlib import Path
from types import MappingProxyType
from typing import Mapping

MAX_FILE_BYTES = 64 << 10
SCHEMA = json.loads((Path(__file__).resolve().parents[2] / 'internal/limits/schema.json').read_text())
LEGACY_ENV = {
    'ZOE_COMPACT_MAX_CHARACTERS': 'compact_max_characters',
    'ZOE_MAX_RUNS': 'max_concurrent_runs',
    'ZOE_UI_CONTROLLER_MAX_RETRIES': 'ui_output_attempts',
    'ZOE_AGENT_CONTEXT_TOKEN_BUDGET': 'runner_context_max_tokens',
    'ZOE_AGENT_MAX_ROTATIONS': 'runner_max_rotations',
}


def reject_legacy_env() -> None:
    for name, key in LEGACY_ENV.items():
        if name in os.environ:
            raise ValueError(f'{name} was removed; move to {key} in ZOE_LIMITS_FILE')


def defaults() -> Mapping[str, int]:
    return MappingProxyType({key: bounds[0] for key, bounds in SCHEMA.items()})


def load_limits(path: str | Path | None = None) -> Mapping[str, int]:
    path = Path(path if path is not None else os.environ.get('ZOE_LIMITS_FILE') or '/etc/zoe/limits.json')
    try:
        mode = path.stat().st_mode
        if not stat.S_ISREG(mode) or not mode & 0o444:
            raise OSError
        with path.open('rb') as source:
            data = source.read(MAX_FILE_BYTES + 1)
    except FileNotFoundError:
        raise ValueError('limits_missing') from None
    except OSError:
        raise ValueError('limits_unreadable') from None
    return decode(data)


def decode(data: bytes) -> Mapping[str, int]:
    if len(data) > MAX_FILE_BYTES:
        raise ValueError('limits_too_large')
    try:
        text = data.decode('utf-8')
    except UnicodeError:
        raise ValueError('limits_invalid_utf8') from None
    if not text.strip():
        raise ValueError('limits_empty')

    def object_fields(pairs):
        values = {}
        for key, value in pairs:
            if key in values:
                raise ValueError('limits_duplicate_key: ' + key if key in SCHEMA else 'limits_unknown_key')
            values[key] = value
        return values

    def invalid_constant(_):
        raise ValueError('limits_invalid_json')

    def parse_integer(text):
        # Avoid Python's digit-limit differences for arbitrarily large integers.
        return int(text) if len(text) <= 20 else 1 << 64

    try:
        values = json.loads(text, object_pairs_hook=object_fields, parse_constant=invalid_constant,
                            parse_int=parse_integer)
    except (json.JSONDecodeError, RecursionError):
        raise ValueError('limits_invalid_json') from None
    if not isinstance(values, dict):
        raise ValueError('limits_invalid_json')
    for key, value in values.items():
        if key not in SCHEMA:
            raise ValueError('limits_unknown_key')
        if type(value) is not int:
            raise ValueError('limits_integer_required: ' + key)
        _, lower, upper = SCHEMA[key]
        if not lower <= value <= upper:
            raise ValueError('limits_out_of_range: ' + key)
    for key in sorted(SCHEMA):
        if key not in values:
            raise ValueError('limits_missing_key: ' + key)

    def require(condition, left, right):
        if not condition:
            raise ValueError(f'limits_constraint: {left}, {right}')

    for left, factor, offset, right, ceiling in (
        ('opening_context_runes', 4, 0, 'gateway_opening_item_bytes', 32768),
        ('voice_sdp_max_bytes', 6, 16384, 'http_call_start_max_bytes', None),
        ('task_text_max_bytes', 12, 4096, 'http_request_max_bytes', None),
        ('voice_text_max_bytes', 6, 4096, 'http_request_max_bytes', None),
        ('runner_assistant_output_max_bytes', 6, 4096, 'http_request_max_bytes', None),
    ):
        require(values[left] * factor + offset <= (ceiling if ceiling else values[right]), left, right)
    require(values['http_request_max_bytes'] >= 512 << 10,
            'http_request_max_bytes', 'gateway_transcript_batch_bytes')
    for left, right in (
        ('memory_ingest_chunk_bytes', 'memory_ingest_body_bytes'),
        ('memory_search_default', 'memory_search_max'),
        ('runner_result_evidence_max_bytes', 'http_request_max_bytes'),
        ('task_entries_page_default', 'task_entries_page_max'),
        ('steward_candidate_default', 'steward_candidate_max'),
        ('diagnostic_max_batch', 'diagnostic_events_per_device'),
        ('diagnostic_event_max_bytes', 'diagnostic_body_max_bytes'),
        ('entry_receipt_max_bytes', 'http_request_max_bytes'),
        ('instance_fence_max_bytes', 'http_request_max_bytes'),
        ('voice_lifecycle_max_bytes', 'http_request_max_bytes'),
        ('diagnostic_body_max_bytes', 'http_request_max_bytes'),
        ('capability_manifest_max_bytes', 'http_request_max_bytes'),
        ('ui_feedback_max_bytes', 'ui_output_max_bytes'),
        ('ui_output_max_bytes', 'model_api_response_max_bytes'),
    ):
        require(values[left] <= values[right], left, right)
    return MappingProxyType(values)
