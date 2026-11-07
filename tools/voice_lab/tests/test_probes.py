import asyncio
import contextlib
import io
import json
import tempfile
import unittest
import wave
from pathlib import Path
from unittest.mock import AsyncMock, patch

import run
import test_experiments as fixtures
from lab import probes, upstream


class ProbeTests(unittest.IsolatedAsyncioTestCase):
    setUp = fixtures.ExperimentTests.setUp

    def args(self, name):
        cases = self.ctx.paths['cases']
        (cases / 'items.txt').write_text('user|fixture plate\nuser|fixture home\n', encoding='utf-8')
        (cases / 'update.txt').write_text('fixture override', encoding='utf-8')
        args = run.parser().parse_args([
            '--experiment', name, '--account-id', '1', '--db-copy', 'unused',
            '--key-file', 'unused', '--persona-file', 'unused', '--audio', str(cases),
            '--instructions-file', str(cases / 'instructions.txt'),
            '--items-file', str(cases / 'items.txt'), '--update-items-file', str(cases / 'items.txt'),
            '--update-instructions-file', str(cases / 'update.txt'),
            '--state-file', str(cases / 'update.txt'), '--second-audio', str(cases / 'second.wav'),
            '--delay-s', '0', '--timeout-s', '0.1'])
        self.ctx.upstream.build_session = upstream.build_session
        return args

    async def test_p2_initial_items_and_one_call(self):
        result = await probes.run_probe(self.ctx, self.args('P2'))
        self.assertTrue(result['delegated'])
        self.assertEqual(result['assistant_text'], ['input 0'])
        self.assertEqual(len(self.peers), 1)
        self.assertEqual(len(self.ctx.recorder.sessions[0]['initial_items']), 2)
        self.assertTrue(self.peers[0].closed and self.bands[0].closed)

    async def test_updates_once_shapes_and_errors_are_observations(self):
        for name in ('P3', 'P4'):
            for trigger in ('event', 'timer'):
                args = self.args(name)
                args.update_trigger = trigger
                args.update_after_s = 0
                played = []
                script = self.script

                async def play(peer, path):
                    played.append(path)

                async def events(band):
                    yield script[1]
                    await asyncio.sleep(0.005)
                    yield {'type': 'session.updated'}
                    yield {'type': 'error', 'error': {'message': 'fixture rejection', 'code': 'fixture-code'}}
                    yield script[0]
                    yield script[3]
                    await asyncio.Future()

                with self.subTest(name=name, trigger=trigger), \
                        patch.object(self.ctx.peer_factory, 'play_wav', play, create=True), \
                        patch.object(self.ctx.sideband_factory, 'events', events):
                    result = await probes.run_probe(self.ctx, args)
                self.assertEqual(self.bands[-1].sent, [probes.update_frame(name, args)])
                expected = ({'instructions': 'fixture override'} if name == 'P3' else
                            {'initial_items': probes.read_items(args.update_items_file)})
                self.assertEqual(self.bands[-1].sent[0], {'type': 'session.update', 'session': expected})
                self.assertEqual(played, [args.second_audio])
                self.assertEqual(result['later_assistant_text'], ['input 0'])
                self.assertEqual(result['post_update_event_types'], ['session.updated', 'error', 'turn.done', 'turn.done'])
                self.assertEqual(result['post_update_errors'][0]['error']['code'], 'fixture-code')
                self.assertTrue(self.bands[-1].closed and self.peers[-1].closed)

    async def test_p1_weather_cancel_state_progress(self):
        args = self.args('P1')
        for name in ('01.wav', '02.wav', '03.wav'):
            with wave.open(str(args.audio / name), 'wb') as output:
                output.setparams((1, 2, 8000, 0, 'NONE', 'not compressed'))
                output.writeframes(bytes(160))
        played = []
        script = self.script

        async def events(band):
            yield script[1]
            yield script[0]
            yield script[0]
            await asyncio.sleep(0.005)
            yield script[0]
            yield script[3]

        async def play(peer, path):
            played.append(path)

        with patch.object(self.ctx.peer_factory, 'play_wav', play, create=True), \
                patch.object(self.ctx.sideband_factory, 'events', events):
            result = await probes.run_probe(self.ctx, args)
        self.assertTrue(result['state_sent'])
        self.assertEqual(result['progress_answer'], ['input 0'])
        self.assertEqual(played, [args.audio / '03.wav'])
        self.assertEqual(self.bands[0].sent[0]['delegation_item_id'], 'test-delegation')
        self.assertEqual(len(self.peers), 1)

    async def test_cleanup_on_send_error_and_timeout(self):
        args = self.args('P3')
        args.update_trigger = 'timer'
        args.update_after_s = 0
        self.hang = True
        with patch.object(self.ctx.sideband_factory, 'send', AsyncMock(side_effect=RuntimeError('private'))):
            result = await probes.run_probe(self.ctx, args)
        self.assertEqual(result['action_error'], 'RuntimeError')
        self.assertNotIn('private', self.ctx.recorder.summary)
        self.assertTrue(self.peers[-1].closed and self.bands[-1].closed)

    async def test_join_wavs_gaps_and_format(self):
        args = self.args('P1')
        paths = [args.audio / 'first.wav', args.audio / 'last.wav']
        for path in paths:
            with wave.open(str(path), 'wb') as output:
                output.setparams((1, 2, 8000, 0, 'NONE', 'not compressed'))
                output.writeframes(bytes(160))
        target = probes.join_wavs(paths, args.audio / 'combined.wav', 0.1)
        with wave.open(str(target), 'rb') as source:
            self.assertEqual(source.getnframes(), 960)

    async def test_dry_run_contains_metadata_only(self):
        args = self.args('P3')
        plan = probes.plan(args)
        text = json.dumps(plan)
        self.assertNotIn('fixture override', text)
        self.assertNotIn(str(args.audio.parent), text)
        self.assertNotIn('fixture plate', text)
        self.assertEqual(plan['files']['update_instructions_file'], {'name': 'update.txt', 'length': 16})

    def test_tally_prints_only_counts(self):
        args = ['--experiment', 'P2', '--account-id', '1', '--db-copy', 'private-db',
                '--key-file', 'private-key', '--persona-file', 'private-persona', '--audio', 'fixture.wav']
        with patch.object(run, 'execute', AsyncMock(return_value={'pass': True, 'delegated': True})), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(run.main(args), 0)
        self.assertEqual(output.getvalue(), 'runs=3 completed=3 delegated=3 errors=0\n')

    async def test_fixture_v3_only_adds_rule(self):
        cases = Path(run.ROOT) / 'cases'
        v2 = (cases / 'instructions_v2.txt').read_text()
        v3 = (cases / 'instructions_v3.txt').read_text()
        added = [line for line in v3.splitlines(keepends=True) if line not in v2.splitlines(keepends=True)]
        self.assertEqual(len(added), 1)
        self.assertEqual(v3.replace(added[0], ''), v2)
