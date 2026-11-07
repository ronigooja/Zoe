"""Offline AVAS builder/transport tests; all credentials are synthetic."""

import json
import unittest
from dataclasses import FrozenInstanceError

from lab.upstream import (AVAS_URL, CreateResult, LabError, build_create_request,
                          build_message_item, build_session, create_call, parse_call_id)


class BuilderTests(unittest.TestCase):
    def test_session(self):
        expected = {'model': 'gpt-live-1-codex', 'instructions': '',
                    'audio': {'output': {'voice': 'cove'}},
                    'delegation': {'type': 'client'}, 'initial_items': []}
        self.assertEqual(build_session(), expected)
        for enabled in (True, False):
            session = build_session('prompt', 'voice', [{'x': 1}], enabled, 'model')
            self.assertEqual(session, {**expected, 'model': 'model', 'instructions': 'prompt',
                             'audio': {'output': {'voice': 'voice'}},
                             'delegation': {'type': 'client', 'ack_filler': enabled},
                             'initial_items': [{'x': 1}]})
        first = build_session()
        first['initial_items'].append('x')
        self.assertEqual(build_session()['initial_items'], [])
        with self.assertRaises(ValueError):
            build_session(ack_filler='false')

    def test_message_items(self):
        for role in ('user', 'developer', 'assistant'):
            self.assertEqual(build_message_item(role, 'hello'), {
                'type': 'message', 'role': role, 'content': [{
                    'type': 'output_text' if role == 'assistant' else 'input_text',
                    'text': 'hello'}]})
        with self.assertRaises(ValueError):
            build_message_item('system', 'hello')

    def test_request(self):
        persona = {'User-Agent': 'test-agent', 'oai-session-id': 'test-session',
                   'Authorization': 'wrong', 'OAI-Device-ID': 'wrong'}
        request = build_create_request('fake-token', 'fake-device', persona, 'offer', {'x': 1})
        self.assertEqual(request.url, AVAS_URL)
        self.assertEqual(request.headers, {
            'user-agent': 'test-agent', 'oai-session-id': 'test-session',
            'authorization': 'Bearer fake-token', 'oai-device-id': 'fake-device',
            'openai-alpha': 'quicksilver=v2', 'originator': 'codex_cli_rs',
            'content-type': 'application/json'})
        self.assertEqual(request.body, b'{"sdp":"offer","session":{"x":1}}')
        self.assertEqual(json.loads(request.body), {'sdp': 'offer', 'session': {'x': 1}})
        for secret in ('fake-token', 'fake-device', 'authorization'):
            self.assertNotIn(secret, repr(request))
        with self.assertRaises(FrozenInstanceError):
            request.url = 'other'
        self.assertEqual(persona['Authorization'], 'wrong')
        self.assertEqual(build_create_request('', 'fake-device', {}, '', {}, 'custom').url, 'custom')

    def test_device_id_fallback_and_required_identity(self):
        for key in ('oai-device-id', 'OAI-Device-ID'):
            persona = {key: 'persona-device'}
            self.assertEqual(build_create_request('', '', persona, '', {}).headers['oai-device-id'],
                             'persona-device')
            self.assertEqual(build_create_request('', 'account-device', persona, '', {}).headers['oai-device-id'],
                             'account-device')
        for persona in ({}, {'oai-device-id': ''}):
            with self.subTest(persona=persona), self.assertRaisesRegex(LabError, '^No device id available$'):
                build_create_request('', '', persona, '', {})

    def test_parse_call_id(self):
        for location in ('rtc_test', '/v1/live/rtc_test', ' /v1/live/rtc_test/ ',
                         'https://example.invalid/v1/live/rtc_test?x=y',
                         '/v1/live/rtc_test/?x=y', 'rtc_test?x=y'):
            with self.subTest(location=location):
                self.assertEqual(parse_call_id(location), 'rtc_test')
        for location in ('', '  ', '/', 'https://example.invalid/', '?x=y'):
            with self.subTest(location=location), self.assertRaises(ValueError):
                parse_call_id(location)


class CallTests(unittest.IsolatedAsyncioTestCase):
    async def test_success(self):
        request = build_create_request('fake-token', 'fake-device', {}, 'offer', {})
        async def transport(actual, proxy):
            self.assertIs(actual, request)
            self.assertEqual(proxy, 'fake-proxy')
            return 201, {'LoCaTiOn': '/calls/rtc_test'}, b'answer\r\n'
        self.assertEqual(await create_call(request, transport, 'fake-proxy'),
                         CreateResult('answer\r\n', 'rtc_test', 201, '/calls/rtc_test'))

    async def test_error_is_redacted_and_truncated(self):
        request = build_create_request('fake-token', 'fake-device', {}, 'offer', {})
        async def transport(request, proxy):
            return 401, {}, (b'fake-token fake-device fake-proxy other-secret ' + b'x' * 1500)
        with self.assertRaises(LabError) as caught:
            await create_call(request, transport, 'fake-proxy', secrets=['other-secret'])
        self.assertEqual(caught.exception.status, 401)
        self.assertLessEqual(len(caught.exception.body), 1003)
        self.assertTrue(caught.exception.body.endswith('...'))
        for secret in ('fake-token', 'fake-device', 'fake-proxy', 'other-secret'):
            self.assertNotIn(secret, str(caught.exception))
            self.assertNotIn(secret, caught.exception.body)

    async def test_transport_error_does_not_leak(self):
        request = build_create_request('fake-token', 'fake-device', {}, '', {})
        for error_type in (RuntimeError, LabError):
            async def transport(request, proxy):
                raise error_type('fake-token fake-proxy fake-device')
            with self.assertRaisesRegex(LabError, '^Create call transport failed$'):
                await create_call(request, transport)
