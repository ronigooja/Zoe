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
from lab.sideband import context_frames


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
                self.assertTrue({'session.updated', 'error', 'turn.done'} <= set(result['post_update_event_types']))
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

    async def test_append_channels_bind_once_errors_and_later_text(self):
        script = self.script
        for name in ('P5', 'P8'):
            for channel in ('instructions', 'thinking', 'commentary'):
                for bind in ('none', 'open'):
                    args = self.args(name)
                    args.channel, args.bind = channel, bind
                    args.text_file = args.update_instructions_file
                    args.spoken_window_s = 0.01
                    played = []

                    async def play(peer, path):
                        played.append(path)

                    async def events(band):
                        yield script[1]
                        await asyncio.sleep(0.003)
                        yield {'type': 'output_transcript.added', 'text': 'fixture filler'}
                        yield {'type': 'error', 'error': {'code': 'fixture-code', 'message': 'rejected'}}
                        yield script[1]
                        await asyncio.sleep(0.02)
                        yield script[0]
                        yield script[3]

                    with self.subTest(name=name, channel=channel, bind=bind), \
                            patch.object(self.ctx.peer_factory, 'play_wav', play, create=True), \
                            patch.object(self.ctx.sideband_factory, 'events', events):
                        result = await probes.run_probe(self.ctx, args)
                    actual_channel = 'instructions' if name == 'P8' else channel
                    identifier = 'test-delegation' if name != 'P8' and bind == 'open' else None
                    expected = {'type': f'session.{actual_channel}.append', 'delegation_id': identifier,
                                'content': [{'type': 'input_text', 'text': 'fixture override'}]}
                    self.assertEqual(self.bands[-1].sent, [expected])
                    self.assertEqual(result['sent_frame'], expected)
                    self.assertTrue(result['spoken'])
                    self.assertTrue(result['spoken_window_complete'])
                    self.assertEqual(result['later_assistant_text'], ['input 0'])
                    self.assertEqual(result['post_append_errors'][0]['error']['code'], 'fixture-code')
                    self.assertEqual(played, [args.second_audio])
                    self.assertTrue(result['pass'])
                    self.assertTrue(self.peers[-1].closed and self.bands[-1].closed)

    async def test_append_silent_window_and_audio_only_window(self):
        for audio in (False, True):
            args = self.args('P5')
            args.text_file = args.update_instructions_file
            args.spoken_window_s = 0.01
            script = self.script

            async def events(band):
                yield script[1]
                await asyncio.sleep(0.003)
                if audio:
                    self.peers[-1].received_audio_ms += 10
                await asyncio.sleep(0.02)
                yield script[3]

            with patch.object(self.ctx.peer_factory, 'play_wav', AsyncMock(), create=True), \
                    patch.object(self.ctx.sideband_factory, 'events', events):
                result = await probes.run_probe(self.ctx, args)
            self.assertEqual(result['spoken'], audio)
            self.assertTrue(result['spoken_window_complete'])

    async def test_p7_delay_body_and_empty_thinking(self):
        script = self.script
        original = probes.AppendController.observe_window

        async def short_window(controller, seconds, second=False):
            self.assertTrue(4.9 < seconds <= 5.0)
            await original(controller, 0.01, second)

        for delay in (None, 0, 2500):
            for empty in (False, True):
                args = self.args('P7')
                args.ack_delay_ms, args.empty_thinking = delay, empty

                async def events(band):
                    yield script[1]
                    await asyncio.sleep(0.003)
                    if empty:
                        yield script[3]
                    yield {'type': 'error', 'error': {'code': 'fixture'}}
                    await asyncio.sleep(0.02)

                with patch.object(probes.AppendController, 'observe_window', short_window), \
                        patch.object(self.ctx.sideband_factory, 'events', events):
                    result = await probes.run_probe(self.ctx, args)
                delegation = self.ctx.recorder.sessions[-1]['delegation']
                if delay is None:
                    self.assertNotIn('acknowledgement_delay_ms', delegation)
                else:
                    self.assertEqual(delegation['acknowledgement_delay_ms'], delay)
                self.assertEqual(self.bands[-1].sent, [
                    {'type': 'session.thinking.append', 'delegation_id': 'test-delegation',
                     'content': [{'type': 'input_text', 'text': ''}]}] if empty else [])
                self.assertEqual(result['filler'], empty)
                self.assertEqual(result['filler_text'], ['input 0'] if empty else [])
                self.assertEqual(len(result['errors']), 1)
                self.assertTrue(result['filler_window_complete'])

    async def test_p6_seeds_items_only_when_requested(self):
        args = self.args('P6')
        for name in ('01.wav',):
            with wave.open(str(args.audio / name), 'wb') as output:
                output.setparams((1, 2, 8000, 0, 'NONE', 'not compressed'))
                output.writeframes(bytes(160))
        args.audio = args.audio / '01.wav'
        args.p6_items_file = args.items_file
        self.script = [{'type': 'turn.done', 'turn': {'id': 'u1', 'role': 'user', 'transcript': 'one'}}]
        await probes.run_probe(self.ctx, args)
        self.assertEqual(len(self.ctx.recorder.sessions[-1]['initial_items']), 2)

    async def test_p6_per_utterance_matching_and_no_items(self):
        args = self.args('P6')
        for name in ('01.wav', '02.wav', '03.wav'):
            with wave.open(str(args.audio / name), 'wb') as output:
                output.setparams((1, 2, 8000, 0, 'NONE', 'not compressed'))
                output.writeframes(bytes(160))
        self.script = [
            {'type': 'delegation.created', 'item': {'id': 'first'}},
            {'type': 'turn.done', 'turn': {'id': 'u1', 'role': 'user', 'transcript': 'one'}},
            {'type': 'turn.done', 'turn': {'role': 'assistant', 'transcript': 'a1'}},
            {'type': 'turn.done', 'turn': {'id': 'u2', 'role': 'user', 'transcript': 'two'}},
            {'type': 'delegation.created', 'item': {'id': 'late', 'user_bidi_turn_id': 'u1'}},
            {'type': 'turn.done', 'turn': {'role': 'assistant', 'transcript': 'a2'}},
            {'type': 'turn.done', 'turn': {'id': 'u3', 'role': 'user', 'transcript': 'three'}},
            {'type': 'delegation.created', 'item': {'id': 'third'}},
        ]
        result = await probes.run_probe(self.ctx, args)
        rows = result['utterances']
        self.assertEqual([r['delegation_created'] for r in rows], [True, False, True])
        self.assertEqual([r['user_transcript'] for r in rows], ['one', 'two', 'three'])
        self.assertEqual([r['assistant_text'] for r in rows], [['a1'], ['a2'], []])
        self.assertEqual(rows[0]['delegation_match'][1]['method'], 'turn_id')
        self.assertEqual(self.ctx.recorder.sessions[-1]['initial_items'], [])
        self.assertTrue(self.peers[-1].closed and self.bands[-1].closed)
        args.audio = args.audio / '01.wav'
        result = await probes.run_probe(self.ctx, args)
        self.assertEqual(len(result['utterances']), 1)

    async def test_new_probes_cleanup_on_send_failure(self):
        for name in ('P5', 'P7', 'P8', 'P9', 'P10'):
            args = self.args(name)
            args.text_file = args.update_instructions_file
            args.empty_thinking = True
            args.create_delay_s = 0
            args.with_context = 'state' if name == 'P9' else None
            with patch.object(self.ctx.sideband_factory, 'send', AsyncMock(side_effect=RuntimeError('private'))):
                result = await probes.run_probe(self.ctx, args)
            self.assertEqual(result['error'], 'RuntimeError')
            self.assertNotIn('private', self.ctx.recorder.summary)
            self.assertTrue(self.peers[-1].closed and self.bands[-1].closed)

    async def test_new_dry_runs_metadata_only_and_run_directories(self):
        from lab.record import make_run_dir

        for name in ('P5', 'P6', 'P7', 'P8', 'P9', 'P10'):
            args = self.args(name)
            args.text_file = args.update_instructions_file
            text = json.dumps(probes.plan(args))
            for private in ('fixture override', 'Delegate every request.', 'fake-token', str(args.audio.parent)):
                self.assertNotIn(private, text)
            self.assertTrue(make_run_dir(args.audio / 'results', name).is_dir())

    async def test_p9_sequence_and_distinct_spoken_windows(self):
        self.ctx.context_frames = context_frames
        script = self.script
        for context in (None, 'state'):
            for post_spoken in (False, True):
                args = self.args('P9')
                args.with_context = context
                args.text_file = args.update_instructions_file
                args.create_delay_s = 0.02
                args.spoken_window_s = 0.02
                played = []
                rejection = {'type': 'error', 'error': {
                    'code': 'fixture-code', 'param': 'type', 'message': 'x' * 300}}

                async def play(peer, path):
                    played.append(path)

                async def events(band):
                    yield script[1]
                    await asyncio.sleep(0.005)
                    yield rejection
                    yield script[3]
                    yield script[1]
                    await asyncio.sleep(0.025)
                    if post_spoken:
                        yield {'type': 'output_transcript.added', 'text': 'after create'}
                        yield script[3]
                    await asyncio.sleep(0.025)
                    yield script[3]

                with self.subTest(context=context, post_spoken=post_spoken), \
                        patch.object(self.ctx.peer_factory, 'play_wav', play, create=True), \
                        patch.object(self.ctx.sideband_factory, 'events', events):
                    result = await probes.run_probe(self.ctx, args)
                expected = context_frames('fixture override') if context else []
                expected.append({'type': 'response.create'})
                self.assertEqual(self.bands[-1].sent, expected)
                self.assertEqual(result['frames_sent'], [frame['type'] for frame in expected])
                self.assertTrue(result['pre_create_spoken'])
                self.assertEqual(result['spoken'], post_spoken)
                self.assertTrue(result['pre_create_window_complete'])
                self.assertTrue(result['spoken_window_complete'])
                self.assertEqual(result['pre_create_assistant_text'], ['input 0'])
                self.assertEqual(result['spoken_window_assistant_text'], ['input 0'] if post_spoken else [])
                self.assertEqual(result['later_assistant_text'], ['input 0'])
                self.assertEqual(played, [args.second_audio])
                self.assertEqual(result['errors'], [rejection])
                self.assertTrue(result['pass'])

    async def test_p10_exact_shapes_errors_and_optional_second_audio(self):
        script = self.script
        for shape in ('A', 'B', 'C'):
            args = self.args('P10')
            args.shape = shape
            args.text_file = args.update_instructions_file
            args.spoken_window_s = 0.01
            if shape == 'C':
                args.second_audio = None
            message = 'field complaint: ' + 'x' * 283
            rejection = {'type': 'error', 'error': {
                'code': 'invalid_request_error', 'param': 'output', 'message': message}}
            play = AsyncMock()

            async def events(band):
                yield script[1]
                yield rejection
                yield {'type': 'accepted.fixture'}
                yield {'type': 'output_transcript.added', 'text': 'spoken'}
                yield script[1]
                await asyncio.sleep(0.02)
                yield script[3]

            with self.subTest(shape=shape), \
                    patch.object(self.ctx.peer_factory, 'play_wav', play, create=True), \
                    patch.object(self.ctx.sideband_factory, 'events', events):
                result = await probes.run_probe(self.ctx, args)
            expected = {'type': 'delegation.function_call_output.create'}
            expected['call_id' if shape == 'C' else 'delegation_item_id'] = 'test-delegation'
            expected.update({'content': [{'type': 'input_text', 'text': 'fixture override'}]}
                            if shape == 'B' else {'output': 'fixture override'})
            self.assertEqual(self.bands[-1].sent, [expected])
            self.assertEqual(result['errors'], [rejection])
            self.assertEqual(result['post_append_errors'], [rejection])
            self.assertIn(message, self.ctx.recorder.summary)
            self.assertIn('accepted.fixture', result['post_frame_non_error_event_types'])
            self.assertNotIn('error', result['post_frame_non_error_event_types'])
            self.assertTrue(result['spoken'] and result['spoken_window_complete'])
            self.assertEqual(result['later_assistant_text'], ['input 0'])
            self.assertTrue(result['pass'])
            self.assertTrue(self.peers[-1].closed and self.bands[-1].closed)
            self.assertEqual(play.await_count, 0 if args.second_audio is None else 1)

    async def test_p9_audio_activity_resets_at_response_create(self):
        args = self.args('P9')
        args.create_delay_s = 0.02
        args.spoken_window_s = 0.02
        args.second_audio = None
        script = self.script

        async def events(band):
            yield script[1]
            await asyncio.sleep(0.005)
            self.peers[-1].received_audio_ms += 10
            await asyncio.sleep(0.05)

        with patch.object(self.ctx.sideband_factory, 'events', events):
            result = await probes.run_probe(self.ctx, args)
        self.assertTrue(result['pre_create_spoken'])
        self.assertFalse(result['spoken'])
        self.assertTrue(result['spoken_window_complete'])

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
