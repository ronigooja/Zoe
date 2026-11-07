"""Shared error identity and formatting, with no live transports."""

import unittest

from lab import sideband, upstream, webrtc
from lab.errors import LabError


class ErrorTests(unittest.IsolatedAsyncioTestCase):
    async def test_all_paths_raise_the_same_class(self):
        self.assertIs(upstream.LabError, LabError)
        self.assertIs(webrtc.LabError, LabError)
        self.assertIs(sideband.LabError, LabError)
        self.assertTrue(issubclass(LabError, RuntimeError))
        self.assertFalse(issubclass(LabError, ValueError))

        with self.assertRaises(LabError) as media:
            list(webrtc.silence_frames(-1))

        request = upstream.build_create_request('fixture-token', 'fixture-device', {}, '', {})

        async def transport(request, proxy):
            return 400, {}, b'fixture response'

        with self.assertRaises(LabError) as http:
            await upstream.create_call(request, transport)
        with self.assertRaises(LabError) as websocket:
            await sideband.Sideband('fixture-call', 'fixture-token', {}).connect()
        for error in (media.exception, http.exception, websocket.exception):
            self.assertIs(type(error), LabError)
        self.assertEqual(http.exception.status, 400)
        self.assertEqual(http.exception.body, 'fixture response')
        self.assertEqual(str(http.exception), 'Create call returned HTTP 400: fixture response')
        self.assertIsNone(media.exception.status)
        self.assertEqual(media.exception.body, '')
