"""Synthetic identities and fake transports only; never call real services."""

import asyncio
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from lab.account import load_persona
from lab.fingerprint import Persona, POST_ORDER, WS_ORDER, for_call, headers
from lab.upstream import build_create_request, http_transport
from lab.sideband import Sideband, curl_connector
import run


class ProfileTests(unittest.TestCase):
    def test_headers_and_ids(self):
        persona = Persona({'user-agent': 'fixture-agent'}, account_id='fixture-account')
        first, second = for_call(persona), for_call(persona)
        self.assertNotEqual(first['thread-id'], second['thread-id'])
        self.assertEqual(first['thread-id'], first['session-id'])
        self.assertEqual(first['session-id'], first['x-session-id'])
        request = build_create_request('fixture-token', '', first, 'offer', {})
        self.assertEqual(tuple(request.headers), POST_ORDER)
        self.assertEqual(tuple(headers(first, 'fixture-token')), WS_ORDER[5:])
        for name in request.headers:
            self.assertFalse(name.startswith(('sec-', 'oai-')))
            self.assertNotIn(name, ('origin', 'referer', 'accept-language'))
        fixed = for_call(Persona({'thread-id': 'fixed'}))
        self.assertEqual(fixed['thread-id'], 'fixed')

    def test_allowlist(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'persona.json'
            for name in ('sec-ch-ua', 'authorization', 'chatgpt-account-id', 'origin', 'unknown'):
                path.write_text(json.dumps({name: 'fixture'}))
                with self.assertRaises(ValueError):
                    load_persona(path, 'codex')
            path.write_text('{}')
            self.assertEqual(load_persona(path, 'codex').fingerprint, 'codex')
            path.write_text(json.dumps({'user-agent': 'fixture', 'oai-session-id': 'fixture', 'oai-language': 'en'}))
            self.assertEqual(load_persona(path, 'legacy-browser').fingerprint, 'legacy-browser')

    def test_dry_run_no_identity_inputs(self):
        output = io.StringIO()
        with patch('lab.account.load_account', side_effect=AssertionError('must not load')), contextlib.redirect_stdout(output):
            result = run.main(['--experiment', 'E0', '--account-id', '123456789',
                               '--db-copy', 'PRIVATE_DB', '--key-file', 'PRIVATE_KEY',
                               '--persona-file', 'PRIVATE_PERSONA', '--dry-run'])
        self.assertEqual(result, 0)
        for secret in ('123456789', 'PRIVATE_DB', 'PRIVATE_KEY', 'PRIVATE_PERSONA', 'Apple_Terminal'):
            self.assertNotIn(secret, output.getvalue())
        self.assertEqual(run.parser().get_default('fingerprint'), 'codex')


class FakeSession:
    instances = []

    def __init__(self, **kwargs):
        self.options = kwargs
        self.calls = []
        self.closed = False
        self.instances.append(self)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        await self.close()

    async def close(self):
        self.closed = True

    async def post(self, url, **kwargs):
        self.calls.append((url, kwargs))
        return type('Response', (), {'status_code': 201, 'headers': {}, 'content': b'answer'})()

    async def ws_connect(self, url, **kwargs):
        self.calls.append((url, kwargs))
        return FakeSocket()


class FakeSocket:
    async def close(self):
        pass

    async def send_str(self, value):
        pass

    async def recv(self):
        from curl_cffi.requests import WebSocketClosed
        raise WebSocketClosed('closed')


class TransportTests(unittest.IsolatedAsyncioTestCase):
    async def test_proxy_headers_and_cleanup(self):
        persona = for_call(Persona({}, account_id='fixture-account'))
        request = build_create_request('fixture-token', '', persona, '', {})
        with patch('curl_cffi.requests.AsyncSession', FakeSession):
            await http_transport(request, 'socks5h://fixture.invalid:1234')
            post = FakeSession.instances[-1]
            self.assertFalse(post.options['trust_env'])
            self.assertEqual(post.calls[0][1]['proxy'], 'socks5h://fixture.invalid:1234')
            self.assertTrue(post.closed)
            sideband = Sideband('fixture-call', 'fixture-token', persona,
                                proxy='http://fixture.invalid:4321')
            await sideband.connect()
            ws = FakeSession.instances[-1]
            self.assertEqual(tuple(ws.calls[0][1]['headers']), WS_ORDER[5:])
            self.assertEqual(ws.calls[0][1]['proxy'], 'http://fixture.invalid:4321')
            self.assertEqual([event async for event in sideband.events()], [])
            await sideband.close()
            self.assertTrue(ws.closed)

    async def test_cancel_connect_cleans_session(self):
        async def cancelled(*args, **kwargs):
            raise asyncio.CancelledError()
        with patch('curl_cffi.requests.AsyncSession', FakeSession), patch.object(FakeSession, 'ws_connect', cancelled):
            with self.assertRaises(asyncio.CancelledError):
                await curl_connector('wss://fixture.invalid/', {}, '')
            self.assertTrue(FakeSession.instances[-1].closed)
