"""Credential-safe result files; no terminal output or raw exception logging."""

import json
import re
import time
from datetime import datetime, timezone
from pathlib import Path


def slim_event(event):
    """When retaining sideband events, omit only the two known audio payloads."""
    if not isinstance(event, dict):
        return event
    field = {'session.input_audio.append': 'audio',
             'session.output_audio.delta': 'delta'}.get(event.get('type'))
    value = event.get(field) if field else None
    if isinstance(value, str) and not re.fullmatch(r'<\d+ chars>', value):
        return {**event, field: f'<{len(value)} chars>'}
    return event


def redact(text: str, secrets: list[str]) -> str:
    """When writing results, replace every supplied non-empty secret literally."""
    values = sorted({secret for secret in secrets if secret}, key=len, reverse=True)
    if not values:
        return text
    return re.sub('|'.join(re.escape(secret) for secret in values), '[redacted]', text)


def make_run_dir(results_dir: str, experiment: str) -> Path:
    if experiment not in {f'E{i}' for i in range(8)}:
        raise ValueError('Unknown experiment')
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    path = Path(results_dir) / f'{stamp}-{experiment}'
    path.mkdir(parents=True, exist_ok=False, mode=0o700)
    return path


class Recorder:
    def __init__(self, run_dir: Path, secrets: list[str]):
        self.run_dir = Path(run_dir)
        self.secrets = tuple(secrets)
        self.started = time.monotonic()
        self.last_ms = 0
        self.events_path = self.run_dir / 'events.jsonl'
        self.events_path.touch(exist_ok=False, mode=0o600)

    def _clean(self, obj):
        if isinstance(obj, dict):
            obj = slim_event(obj)
            return {redact(str(k), self.secrets): self._clean(v) for k, v in obj.items()}
        if isinstance(obj, (list, tuple)):
            return [self._clean(v) for v in obj]
        if isinstance(obj, str):
            return redact(obj, self.secrets)
        if obj is None or isinstance(obj, (bool, int, float)):
            return obj
        return '[redacted]'

    def _json(self, obj):
        # Redact strings before serialization so quotes, backslashes, and Unicode
        # secrets cannot evade matching through JSON escaping. Redact again after.
        serialized = json.dumps(self._clean(obj), ensure_ascii=True, allow_nan=False)
        escaped = [json.dumps(secret, ensure_ascii=True)[1:-1]
                   for secret in self.secrets if secret]
        return redact(redact(serialized, self.secrets), escaped)

    def event(self, direction: str, obj: dict) -> None:
        self.last_ms = max(self.last_ms, int((time.monotonic() - self.started) * 1000))
        row = {'t_ms': self.last_ms, 'dir': direction, 'event': obj}
        with self.events_path.open('a', encoding='utf-8') as output:
            output.write(self._json(row) + '\n')

    def write_summary(self, text: str):
        safe = redact(text, self.secrets)
        # Result Markdown must satisfy the repository's English-only scan.
        safe = safe.encode('ascii', errors='backslashreplace').decode('ascii')
        (self.run_dir / 'summary.md').write_text(redact(safe, self.secrets), encoding='utf-8')

    def write_session(self, obj: dict):
        (self.run_dir / 'session.json').write_text(self._json(obj) + '\n', encoding='utf-8')
