"""Offline sideband tests using a fake connector and connection."""

import asyncio
import json
import unittest

from lab.sideband import (SIDEBAND_URL, Sideband, context_frames, split_text,
                          user_text_frame)
from lab.upstream import LabError


class FrameTests(unittest.TestCase):
    def test_split_utf8(self):
        for text in ('a' * 1001, '\u4f60\u597d' * 200,
                     'a' * 499 + '\U0001f600' * 251,
                     ' \u4f60\u597d\U0001f600\n' * 190):
            with self.subTest(length=len(text)):
                pieces = split_text(text)
                self.assertEqual(''.join(pieces), text)
                self.assertTrue(all(0 < len(piece.encode('utf-8')) <= 500 for piece in pieces))
                for piece in pieces:
                    self.assertEqual(piece.encode('utf-8').decode('utf-8'), piece)
        self.assertEqual(split_text('a' * 499 + '\u4f60b'), ['a' * 499, '\u4f60b'])
        self.assertEqual(split_text('abcdef', 2), ['ab', 'cd', 'ef'])
        for text in ('', ' \t\n', '\u3000'):
            self.assertEqual(split_text(text), [])
        for limit in (0, -1):
            with self.assertRaises(ValueError):
                split_text('a', limit)
        with self.assertRaises(ValueError):
            split_text('\U0001f600', 3)

    def test_context_frames(self):
        frame = {'type': 'session.context.append', 'channel': 'speakable',
                 'content': [{'type': 'input_text', 'text': 'hello'}]}
        self.assertEqual(context_frames('hello'), [frame])
        self.assertEqual(context_frames('hello', 'delegation-1'), [
            {**frame, 'type': 'delegation.context.append', 'delegation_item_id': 'delegation-1'}])
        self.assertEqual(context_frames('hello', channel='commentary')[0]['channel'], 'commentary')
        with self.assertRaises(ValueError):
            context_frames('hello', channel='other')
        self.assertEqual(context_frames(''), [])
        self.assertEqual(context_frames('  '), [])
        self.assertEqual(len(context_frames('x' * 501)), 2)

    def test_experimental_user_text(self):
        self.assertEqual(user_text_frame('hello'), {
            'type': 'conversation.item.create', 'item': {
                'type': 'message', 'role': 'user',
                'content': [{'type': 'input_text', 'text': 'hello'}]}})
        self.assertIn('UNVERIFIED', user_text_frame.__doc__)
        self.assertIn('send_conversation_item_create', user_text_frame.__doc__)


class FakeConnection:
    def __init__(self):
        self.queue = asyncio.Queue()
        self.sent = []
        self.close_count = 0

    async def send(self, raw):
        self.sent.append(raw)

    async def recv(self):
        raw = await self.queue.get()
        if raw is None:
            raise EOFError
        return raw

    async def close(self):
        self.close_count += 1
        await self.queue.put(None)


class FakeRecorder:
    def __init__(self):
        self.events = []

    def event(self, direction, obj):
        self.events.append((direction, obj))


class SidebandTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.socket = FakeConnection()
        self.recorder = FakeRecorder()
        self.calls = []
        async def connector(url, headers, proxy):
            self.calls.append((url, headers, proxy))
            return self.socket
        self.sideband = Sideband('rtc_test', 'fake-token', {'User-Agent': 'test-agent'},
                                 'fake-proxy', connector, self.recorder)

    async def test_connect_and_send(self):
        await self.sideband.connect()
        self.assertEqual(self.calls, [(SIDEBAND_URL + 'rtc_test', {
            'Authorization': 'Bearer fake-token', 'openai-alpha': 'quicksilver=v2',
            'originator': 'codex_cli_rs', 'user-agent': 'test-agent'}, 'fake-proxy')])
        frame = context_frames('hello')[0]
        await self.sideband.send(frame)
        self.assertEqual(json.loads(self.socket.sent[0]), frame)
        self.assertEqual(self.recorder.events, [('out', frame)])
        await self.sideband.close()
        await self.sideband.close()
        self.assertEqual(self.socket.close_count, 1)

    async def test_events_record_and_end_on_close(self):
        await self.sideband.connect()
        await self.socket.queue.put('{"type":"turn.done"}')
        await self.socket.queue.put('not JSON')
        await self.socket.queue.put(b'{"type":"bytes"}')
        await self.socket.queue.put('[1,2]')
        iterator = self.sideband.events()
        expected = [{'type': 'turn.done'}, {'_raw': 'not JSON'},
                    {'type': 'bytes'}, {'_raw': '[1,2]'}]
        for obj in expected:
            self.assertEqual(await anext(iterator), obj)
        pending = asyncio.create_task(anext(iterator))
        await asyncio.sleep(0)
        await self.sideband.close()
        with self.assertRaises(StopAsyncIteration):
            await asyncio.wait_for(pending, 1)
        self.assertEqual(self.recorder.events, [('in', obj) for obj in expected])

    async def test_remote_close(self):
        await self.sideband.connect()
        await self.socket.queue.put(None)
        self.assertEqual([obj async for obj in self.sideband.events()], [])

    async def test_requires_connection(self):
        with self.assertRaises(LabError):
            await self.sideband.send({})
        with self.assertRaises(LabError):
            await anext(self.sideband.events())

    async def test_no_recorder(self):
        async def connector(url, headers, proxy):
            return self.socket
        sideband = Sideband('rtc_test', 'fake-token', {'user-agent': 'test-agent'},
                            connector=connector)
        await sideband.connect()
        await sideband.send({'type': 'test'})
        await self.socket.queue.put('{}')
        await self.socket.queue.put(None)
        self.assertEqual([obj async for obj in sideband.events()], [{}])
        await sideband.close()
