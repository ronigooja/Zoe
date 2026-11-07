"""Test-only AVAS request builders and an injectable, lazy HTTP transport."""

import json
from dataclasses import dataclass, field
from urllib.parse import urlsplit

from .errors import LabError


AVAS_URL = ('https://chatgpt.com/backend-api/codex/realtime/calls'
            '?intent=quicksilver&architecture=avas')


def build_session(instructions: str = '', voice: str = 'cove',
                  initial_items: list | None = None, ack_filler: bool | None = None,
                  model: str = 'gpt-live-1-codex',
                  acknowledgement_delay_ms: int | None = None) -> dict:
    delegation = {'type': 'client'}
    if ack_filler is not None:
        if not isinstance(ack_filler, bool):
            raise ValueError('ack_filler must be boolean')
        delegation['ack_filler'] = ack_filler
    if acknowledgement_delay_ms is not None:
        if type(acknowledgement_delay_ms) is not int or acknowledgement_delay_ms < 0:
            raise ValueError('acknowledgement_delay_ms must be a non-negative integer')
        delegation['acknowledgement_delay_ms'] = acknowledgement_delay_ms
    return {'model': model, 'instructions': instructions,
            'audio': {'output': {'voice': voice}}, 'delegation': delegation,
            'initial_items': list(initial_items) if initial_items is not None else []}


def build_message_item(role: str, text: str) -> dict:
    if role not in {'user', 'developer', 'assistant'}:
        raise ValueError('Invalid initial item role')
    return {'type': 'message', 'role': role, 'content': [
        {'type': 'output_text' if role == 'assistant' else 'input_text', 'text': text}]}


@dataclass(frozen=True)
class CreateRequest:
    url: str
    headers: dict = field(repr=False)
    body: bytes = field(repr=False)


@dataclass(frozen=True)
class CreateResult:
    answer_sdp: str
    call_id: str
    status: int
    location: str


def resolve_device_id(device_id: str, persona: dict) -> str:
    headers = {key.lower(): value for key, value in persona.items()}
    selected = device_id or headers.get('oai-device-id')
    if not selected:
        raise LabError('No device id available')
    return selected


def build_create_request(token: str, device_id: str, persona: dict, sdp: str,
                         session: dict, url: str = AVAS_URL) -> CreateRequest:
    headers = {key.lower(): value for key, value in persona.items()}
    headers.update({'authorization': 'Bearer ' + token,
                    'oai-device-id': resolve_device_id(device_id, persona),
                    'openai-alpha': 'quicksilver=v2', 'originator': 'codex_cli_rs',
                    'content-type': 'application/json'})
    body = json.dumps({'sdp': sdp, 'session': session}, ensure_ascii=False,
                      separators=(',', ':')).encode('utf-8')
    return CreateRequest(url, headers, body)


def parse_call_id(location: str) -> str:
    path = urlsplit(location.strip()).path.rstrip('/')
    call_id = path.rsplit('/', 1)[-1].strip()
    if not call_id:
        raise ValueError('Create response lacks a call identifier')
    return call_id


async def http_transport(request, proxy=''):
    """Open HTTP with explicit proxy routing, never ambient proxy settings."""
    try:
        import httpx
    except ImportError:
        raise LabError('Live HTTP requires the pinned httpx[socks] dependency') from None
    async with httpx.AsyncClient(proxy=proxy or None, trust_env=False,
                                 timeout=45, follow_redirects=False) as client:
        response = await client.post(request.url, headers=request.headers,
                                     content=request.body)
        return response.status_code, dict(response.headers), response.content


async def create_call(request, transport=None, proxy: str = '', *, secrets=()) -> CreateResult:
    """Create one call; redact caller secrets plus known request credentials."""
    try:
        status, headers, body = await (transport or http_transport)(request, proxy)
    except LabError as exc:
        if str(exc) == 'Live HTTP requires the pinned httpx[socks] dependency':
            raise
        raise LabError('Create call transport failed') from None
    except Exception:
        # Transport exception text can contain proxy URLs or credentials.
        raise LabError('Create call transport failed') from None
    if not 200 <= status < 300:
        sensitive = list(secrets) + [proxy]
        for key, value in request.headers.items():
            if key.lower() in {'authorization', 'oai-device-id'}:
                sensitive.append(value)
                if key.lower() == 'authorization' and value.startswith('Bearer '):
                    sensitive.append(value[7:])
        detail = body.decode('utf-8', errors='replace')
        for secret in sorted(set(filter(None, sensitive)), key=len, reverse=True):
            detail = detail.replace(secret, '[REDACTED]')
        # Redact before truncating so a boundary cannot leave a partial secret.
        detail = detail[:1000] + ('...' if len(detail) > 1000 else '')
        raise LabError('Create call returned HTTP ' + str(status), status, detail)
    location = next((value for key, value in headers.items()
                     if key.lower() == 'location'), '')
    return CreateResult(body.decode('utf-8'), parse_call_id(location), status, location)
