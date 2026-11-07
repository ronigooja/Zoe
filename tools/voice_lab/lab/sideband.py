"""Test-only sideband frames and an injectable, lazy WebSocket connection."""

import json

from .errors import LabError
from .record import slim_event
from .upstream import build_message_item


SIDEBAND_URL = 'wss://api.openai.com/v1/live/'


def split_text(text: str, limit: int = 500) -> list[str]:
    """Match sendAVASContext: skip blank input, otherwise preserve every byte."""
    if limit <= 0:
        raise ValueError('Byte limit must be positive')
    if not text.strip():
        return []
    raw = text.encode('utf-8')
    pieces = []
    start = 0
    while start < len(raw):
        end = min(start + limit, len(raw))
        while end > start and end < len(raw) and raw[end] & 0xC0 == 0x80:
            end -= 1
        if end == start:
            raise ValueError('Byte limit cannot fit a UTF-8 character')
        pieces.append(raw[start:end].decode('utf-8'))
        start = end
    return pieces


def context_frames(text: str, delegation_id: str | None = None) -> list[dict]:
    frames = []
    for piece in split_text(text):
        frame = {'type': 'session.context.append', 'channel': 'speakable',
                 'content': [{'type': 'input_text', 'text': piece}]}
        if delegation_id is not None:
            frame.update(type='delegation.context.append', delegation_item_id=delegation_id)
        frames.append(frame)
    return frames


def user_text_frame(text: str) -> dict:
    """Experimental: the README lists typed input as UNVERIFIED on AVAS.

    Chosen conversation.item.create shape follows send_conversation_item_create
    in codex-rs/codex-api/src/endpoint/realtime_websocket/methods.rs, using the
    message content shape from methods_frameless_bidi.rs. This is not evidence
    that the AVAS sideband accepts this event.
    """
    return {'type': 'conversation.item.create', 'item': build_message_item('user', text)}


async def websocket_connector(url, headers, proxy):
    """Require modern websockets with explicit proxy support; never auto-route."""
    try:
        from websockets.asyncio.client import connect
    except ImportError:
        raise LabError('Live sideband requires the pinned websockets dependency') from None
    return await connect(url, additional_headers=headers, user_agent_header=None,
                         proxy=proxy or None, open_timeout=30, close_timeout=10)


class Sideband:
    def __init__(self, call_id, token, persona, proxy='', connector=None, recorder=None):
        self._call_id = call_id
        self._token = token
        self._persona = persona
        self._proxy = proxy
        self._connector = connector or websocket_connector
        self._recorder = recorder
        self._connection = None
        self._closed = False

    async def connect(self):
        if self._connection is not None:
            raise LabError('Sideband is already connected')
        if self._closed:
            raise LabError('Sideband is closed')
        persona = {key.lower(): value for key, value in self._persona.items()}
        if not persona.get('user-agent'):
            raise LabError('Sideband persona requires user-agent')
        headers = {'Authorization': 'Bearer ' + self._token,
                   'openai-alpha': 'quicksilver=v2', 'originator': 'codex_cli_rs',
                   'user-agent': persona['user-agent']}
        try:
            self._connection = await self._connector(
                SIDEBAND_URL + self._call_id, headers, self._proxy)
        except LabError as exc:
            if str(exc) == 'Live sideband requires the pinned websockets dependency':
                raise
            raise LabError('Sideband connection failed') from None
        except Exception:
            raise LabError('Sideband connection failed') from None

    async def send(self, frame: dict):
        if self._connection is None or self._closed:
            raise LabError('Sideband is not connected')
        try:
            await self._connection.send(json.dumps(frame, ensure_ascii=False))
        except Exception:
            raise LabError('Sideband send failed') from None
        if self._recorder is not None:
            self._recorder.event('out', frame)

    async def events(self):
        if self._connection is None:
            raise LabError('Sideband is not connected')
        while not self._closed:
            try:
                raw = await self._connection.recv()
            except (EOFError, StopAsyncIteration):
                return
            except Exception as exc:
                # websockets is deliberately not imported for fake connections.
                closed = any(cls.__name__ == 'ConnectionClosed' and
                             cls.__module__.startswith('websockets')
                             for cls in type(exc).__mro__)
                if self._closed or closed:
                    return
                raise LabError('Sideband receive failed') from None
            if raw is None or self._closed:
                return
            if isinstance(raw, bytes):
                raw = raw.decode('utf-8', errors='replace')
            try:
                obj = json.loads(raw)
            except (ValueError, TypeError):
                obj = {'_raw': raw}
            if not isinstance(obj, dict):
                obj = {'_raw': raw}
            obj = slim_event(obj)
            if self._recorder is not None:
                self._recorder.event('in', obj)
            yield obj

    async def close(self):
        if self._closed:
            return
        self._closed = True
        if self._connection is not None:
            try:
                await self._connection.close()
            except Exception:
                raise LabError('Sideband close failed') from None
