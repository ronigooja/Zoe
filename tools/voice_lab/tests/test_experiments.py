import asyncio
import importlib.util
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from lab import experiments as ex
from lab.errors import LabError


class Recorder:
    def __init__(self):
        self.events = []
        self.sessions = []
        self.summary = ''

    def event(self, direction, obj):
        self.events.append((direction, obj))

    def write_session(self, obj):
        self.sessions.append(obj)

    def write_summary(self, text):
        self.summary = text


class ExperimentTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        cases = Path(self.tmp.name)
        (cases / 'instructions.txt').write_text('Delegate every request.', encoding='utf-8')
        (cases / 'utterances.txt').write_text('\n'.join('input ' + str(i) for i in range(7)), encoding='utf-8')
        self.peers = []
        self.bands = []
        self.fail = None
        self.hang = False
        self.script = [
            {'type': 'turn.done', 'turn': {'id': 'user-turn', 'role': 'user', 'transcript': 'input'}},
            {'type': 'delegation.created', 'item': {'id': 'test-delegation', 'content': [{'type': 'input_text', 'text': 'input'}]}},
            {'type': 'mystery', 'unknown': True},
            {'type': 'turn.done', 'turn': {'role': 'assistant', 'transcript': 'input 0'}},
            {'type': 'turn.done', 'turn': {'role': 'assistant', 'transcript': 'input 0'}},
            {'type': 'delegation.completed'},
            {'type': 'turn.done', 'turn': {'role': 'user', 'transcript': 'next'}},
        ]
        owner = self

        class Peer:
            received_audio_ms = 500
            closed = False

            def __init__(self, recorder=None):
                owner.peers.append(self)

            async def create_offer(self, wav):
                self.wav = wav
                if owner.fail == 'offer':
                    raise RuntimeError('must not escape into summary')
                return 'fake-offer'

            async def set_answer(self, sdp):
                if owner.fail == 'answer':
                    raise RuntimeError('private')

            async def close(self):
                self.closed = True

        class Sideband:
            closed = False

            def __init__(self, call_id, token, persona, proxy='', connector=None, recorder=None):
                self.sent = []
                owner.bands.append(self)

            async def connect(self):
                if owner.fail == 'connect':
                    raise RuntimeError('private')

            async def send(self, frame):
                self.sent.append(frame)

            async def events(self):
                if owner.fail == 'events':
                    raise RuntimeError('private')
                if owner.hang:
                    await asyncio.Future()
                for event in owner.script:
                    yield event

            async def close(self):
                self.closed = True

        async def create_call(request, transport, proxy='', secrets=None):
            self.assertEqual(secrets, ['fake-token', 'fake-proxy', 'fake-device'])
            self.assertEqual(await transport(request), 'fake-response')
            return SimpleNamespace(status=201, call_id='fake-call', answer_sdp='fake-answer')

        async def transport(request):
            return 'fake-response'

        upstream = SimpleNamespace(build_session=lambda **kw: kw,
                                   build_message_item=lambda role, text: {'role': role, 'text': text},
                                   build_create_request=lambda *args: args,
                                   create_call=create_call)
        self.ctx = ex.Context(SimpleNamespace(token='fake-token', proxy='fake-proxy', device_id='fake-device'),
                              {}, Peer, transport, None, Recorder(), upstream, Sideband,
                              lambda text, delegation_id=None: [{'text': text, 'delegation_item_id': delegation_id}],
                              lambda text: {'user_text': text},
                              paths={'cases': cases, 'audio': cases}, timeout_s=1, silence_s=0)

    async def test_all_experiments(self):
        counts = [1, 1, 7, 3, 1, 2, 2, 14]
        for index, count in enumerate(counts):
            with self.subTest(experiment=index):
                self.peers.clear()
                self.bands.clear()
                result = await ex.EXPERIMENTS[f'E{index}'](self.ctx)
                self.assertEqual(result['pass'], index != 6, result)
                self.assertEqual(len(result['runs']), count)
                self.assertTrue(all(peer.closed for peer in self.peers))
                self.assertTrue(all(band.closed for band in self.bands))
                self.assertIn('FAIL' if index == 6 else 'PASS', self.ctx.recorder.summary)
                if index == 1:
                    self.assertEqual(result['runs'][0]['user_transcripts'], ['input'])
                    self.assertEqual(result['runs'][0]['assistant_text'], ['input 0'])
                    self.assertEqual(result['runs'][0]['delegated_text'], ['input'])
                    self.assertIn('mystery', result['runs'][0]['event_order'])
                if index == 3:
                    self.assertEqual([run['variant'] for run in result['runs']], ['session', 'open', 'answered'])
                    self.assertTrue(result['runs'][2]['answered_delegation_reused'])
                    self.assertEqual(len(self.bands[2].sent), 2)
                if index == 4:
                    self.assertIn('delegation.completed', result['runs'][0]['completion_like_types'])
                if index == 6:
                    self.assertEqual([run['initial_role'] for run in result['runs']], ['user', 'developer'])
                    self.assertFalse(result['runs'][0]['fact_used'])
                if index == 7:
                    self.assertEqual([run['ack_filler'] for run in result['runs']], [True] * 7 + [False] * 7)

    async def test_instruction_file_default_and_override(self):
        self.assertEqual(ex.inputs(self.ctx)[0], 'Delegate every request.')
        custom = Path(self.tmp.name) / 'custom-prompt.txt'
        custom.write_text('Custom fixture instructions.', encoding='utf-8')
        self.ctx.paths['instructions'] = custom
        result = await ex.E2(self.ctx)
        self.assertTrue(result['pass'])
        self.assertEqual(ex.inputs(self.ctx)[0], 'Custom fixture instructions.')
        self.assertTrue(all(session['instructions'] == 'Custom fixture instructions.'
                            for session in self.ctx.recorder.sessions))

    async def test_experiments_use_real_generator_output_names(self):
        spec = importlib.util.spec_from_file_location(
            'voice_lab_generator_fixture', Path(__file__).resolve().parents[1] / 'cases' / 'gen_audio.py')
        generator = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(generator)
        output = (Path(self.tmp.name) / 'generated-audio').resolve()
        generated = []

        def runner(command, **kwargs):
            if command[0] == 'afconvert':
                path = Path(command[-1])
                path.touch()
                generated.append(path)

        generator.generate_audio(self.ctx.paths['cases'] / 'utterances.txt', output,
                                 runner=runner, which=lambda name: '/fake/' + name, platform='darwin')
        self.assertEqual(len(generated), 7)
        self.assertTrue(all(path.is_file() for path in generated))
        self.ctx.paths['audio'] = output
        result = await ex.E2(self.ctx)
        self.assertTrue(result['pass'])
        self.assertEqual([peer.wav for peer in self.peers], generated)
        self.peers.clear()
        result = await ex.E1(self.ctx)
        self.assertTrue(result['pass'])
        self.assertEqual([peer.wav for peer in self.peers], generated[:1])

    async def test_each_experiment_closes_on_error(self):
        for name, driver in ex.EXPERIMENTS.items():
            with self.subTest(experiment=name):
                self.peers.clear()
                self.bands.clear()
                self.fail = 'answer'
                result = await driver(self.ctx)
                self.assertFalse(result['pass'])
                self.assertTrue(all(peer.closed for peer in self.peers))
                self.assertTrue(all(band.closed for band in self.bands))
                self.assertNotIn('private', self.ctx.recorder.summary)

    async def test_setup_and_event_errors_close(self):
        for stage in ('offer', 'connect', 'events'):
            self.fail = stage
            result = await ex.E1(self.ctx)
            self.assertFalse(result['pass'])
            self.assertTrue(all(peer.closed for peer in self.peers))
            self.assertTrue(all(band.closed for band in self.bands))

    async def test_missing_device_id_fails_before_peer_or_transport(self):
        self.ctx.account.device_id = ''
        result = await ex.E0(self.ctx)
        self.assertFalse(result['pass'])
        self.assertEqual(result['error'], 'LabError')
        self.assertEqual(self.peers, [])
        self.assertEqual(self.bands, [])

    async def test_media_input_errors_are_handled_as_lab_errors(self):
        def invalid_peer(**kwargs):
            raise LabError('fixture-secret input error')

        self.ctx.peer_factory = invalid_peer
        result = await ex.E1(self.ctx)
        self.assertFalse(result['pass'])
        self.assertEqual(result['error'], 'LabError')
        self.assertNotIn('fixture-secret', self.ctx.recorder.summary)

    async def test_timeout_bounded(self):
        self.hang = True
        self.ctx.timeout_s = 0.03
        self.ctx.observation_s = 0.03
        result = await asyncio.wait_for(ex.E1(self.ctx), 0.25)
        self.assertFalse(result['pass'])
        self.assertTrue(all(peer.closed for peer in self.peers))
        self.assertTrue(all(band.closed for band in self.bands))

    async def test_queue_timeout_does_not_cancel_delayed_generator(self):
        tasks = []
        finished = []
        script = self.script[:4]

        async def events(self):
            tasks.append(asyncio.current_task())
            try:
                # The first queue read expires at 0.25 s, before delegation arrives.
                await asyncio.sleep(0.3)
                for event in script:
                    yield event
                await asyncio.Future()
            finally:
                finished.append(True)

        with patch.object(self.ctx.sideband_factory, 'events', events):
            result = await ex.E1(self.ctx)
        self.assertTrue(result['pass'], result)
        self.assertTrue(result['runs'][0]['delegation_created'])
        self.assertFalse(result['runs'][0]['reader_ended_early'])
        self.assertEqual(len(tasks), 1)
        self.assertTrue(tasks[0].done())
        self.assertEqual(finished, [True])

    async def test_reader_finishes_before_close_on_success_error_and_timeout(self):
        for outcome in ('success', 'error', 'timeout', 'cancel'):
            tasks = []
            finished = []
            script = self.script[:4]
            self.ctx.observation_s = 0.01

            async def events(self):
                tasks.append(asyncio.current_task())
                try:
                    if outcome == 'error':
                        raise RuntimeError('fixture-private-detail')
                    if outcome == 'success':
                        for event in script:
                            yield event
                    await asyncio.Future()
                finally:
                    finished.append(True)

            async def close(band):
                self.assertEqual(len(tasks), 1)
                self.assertTrue(tasks[0].done())
                band.closed = True

            with self.subTest(outcome=outcome), \
                    patch.object(self.ctx.sideband_factory, 'events', events), \
                    patch.object(self.ctx.sideband_factory, 'close', close):
                if outcome == 'cancel':
                    self.ctx.observation_s = 20
                    with self.assertRaises(asyncio.TimeoutError):
                        await asyncio.wait_for(ex._call(self.ctx, {}, None), 0.01)
                else:
                    result = await ex.E1(self.ctx)
                    run = result['runs'][0]
                    self.assertTrue(run['clean_close'])
                    self.assertEqual(run['reader_ended_early'], outcome == 'error')
                    self.assertEqual(result['pass'], outcome == 'success')
                    if outcome == 'error':
                        self.assertEqual(run['reader_error'], 'RuntimeError')
                        self.assertNotIn('fixture-private-detail', self.ctx.recorder.summary)
            self.assertEqual(len(tasks), 1)
            self.assertTrue(tasks[0].done())
            self.assertEqual(finished, [True])

    async def test_reader_end_sentinel_is_neutral_and_in_summary(self):
        self.script = [{'type': 'session.started', 'session': {'id': 'fixture'}}]
        result = await ex.E1(self.ctx)
        run = result['runs'][0]
        self.assertTrue(run['reader_ended_early'])
        self.assertNotIn('reader_error', run)
        self.assertIn('"reader_ended_early": true', self.ctx.recorder.summary)
        self.assertEqual(run['events'], self.script)

    async def test_final_turn_extraction_and_legacy_fallback(self):
        for role in ('user', 'assistant'):
            for event in ({'type': 'turn.done', 'turn': {'role': role, 'transcript': 'final'},
                           'role': 'wrong', 'text': 'wrong'},
                          {'type': 'turn.done', 'role': role, 'text': 'final'}):
                self.assertTrue(ex.turn(event, role))
                self.assertEqual(ex.turn_text(event), 'final')
        self.assertFalse(ex.turn({'type': 'turn.created', 'turn': {'role': 'user'}}, 'user'))

    async def test_delegation_extraction(self):
        event = self.script[1]
        self.assertEqual(ex.delegation_id(event), 'test-delegation')
        self.assertEqual(ex.delegated_text(event), 'input')
        for malformed in (None, {}, {'type': 'delegation.created', 'item': {'content': []}}):
            self.assertIsNone(ex.delegated_text(malformed))

    async def test_observation_slims_audio_and_ends_at_reply(self):
        self.script.insert(0, {'type': 'session.input_audio.append', 'audio': 'A' * 10000})
        self.script.insert(1, {'type': 'session.output_audio.delta', 'delta': 'B' * 10000})
        result = await ex.E1(self.ctx)
        run = result['runs'][0]
        self.assertTrue(result['pass'])
        self.assertEqual(run['events'][0]['audio'], '<10000 chars>')
        self.assertEqual(run['events'][1]['delta'], '<10000 chars>')
        self.assertEqual(run['assistant_text'], ['input 0'])
        self.assertNotIn('delegation.completed', run['event_order'])
        self.assertLess(len(self.ctx.recorder.summary), 5000)
        self.assertTrue(run['clean_close'])

    async def test_plain_answer_waits_for_late_delegation(self):
        self.script = [self.script[0], self.script[3], self.script[1], self.script[4]]
        result = await ex.E1(self.ctx)
        self.assertTrue(result['pass'])
        self.assertEqual(result['runs'][0]['assistant_text'], ['input 0', 'input 0'])

    async def test_plain_answer_grace_is_bounded(self):
        self.ctx.delegation_grace_s = 0.005
        script = [self.script[0], self.script[3]]

        async def events(self):
            for event in script:
                yield event
            await asyncio.Future()

        with patch.object(self.ctx.sideband_factory, 'events', events):
            result = await asyncio.wait_for(ex.E1(self.ctx), 0.25)
        self.assertFalse(result['pass'])
        self.assertTrue(result['runs'][0]['delegation_grace_elapsed'])
        self.assertTrue(result['runs'][0]['clean_close'])

    async def test_full_window_and_experiment_budget(self):
        self.assertEqual(self.ctx.observation_s, 20.0)
        self.assertEqual(self.ctx.delegation_grace_s, 8.0)
        self.assertGreaterEqual(ex.experiment_timeout(self.ctx, 'E7'), 14 * 20)
        self.ctx.timeout_s = 10000
        self.assertEqual(ex.experiment_timeout(self.ctx, 'E7'), ex.HARD_CAP_S)
        self.ctx.timeout_s = 1
        waits = []
        original = asyncio.wait_for

        async def wait_for(awaitable, timeout):
            waits.append(timeout)
            return await original(awaitable, timeout)

        with patch.object(ex.asyncio, 'wait_for', wait_for):
            result = await ex.E7(self.ctx)
        self.assertTrue(result['pass'])
        self.assertEqual(waits[0], ex.experiment_timeout(self.ctx, 'E7'))
        self.assertEqual(waits.count(51.0), 14)
        self.assertEqual(waits.count(0.25), 14 * 4)

    async def test_e1_requires_nonempty_final_user_text(self):
        self.script[0]['turn']['transcript'] = '   '
        self.script.insert(0, {'type': 'input_transcript.added', 'item': {'text': 'fragment'}})
        result = await ex.E1(self.ctx)
        self.assertFalse(result['pass'])

    async def test_cleanup_uses_full_default_budget_without_sleeping(self):
        self.assertEqual(self.ctx.cleanup_timeout_s, 15.0)
        original_wait_for = asyncio.wait_for
        attempted = []
        budgets = []
        virtual_elapsed = [0.0]

        async def sideband_close():
            attempted.append('sideband')
            # Scaled virtual seconds: 2.1 s is beyond the old cap, below 15 s.
            virtual_elapsed[0] += 0.0021

        async def peer_close():
            attempted.append('peer')

        async def virtual_wait_for(awaitable, timeout):
            if awaitable.cr_code in (sideband_close.__code__, peer_close.__code__):
                budgets.append(timeout)
                start = virtual_elapsed[0]
                await awaitable
                if virtual_elapsed[0] - start >= timeout / 1000:
                    raise asyncio.TimeoutError()
                return
            return await original_wait_for(awaitable, timeout)

        with patch.object(self.ctx.sideband_factory, 'close', lambda self: sideband_close()), \
                patch.object(self.ctx.peer_factory, 'close', lambda self: peer_close()), \
                patch.object(ex.asyncio, 'wait_for', virtual_wait_for):
            result = await ex.E0(self.ctx)
        self.assertTrue(result['pass'], result)
        self.assertTrue(result['runs'][0]['clean_close'])
        self.assertNotIn('cleanup_errors', result['runs'][0])
        self.assertEqual(attempted, ['sideband', 'peer'])
        self.assertEqual(budgets, [15.0, 15.0])

    async def test_cleanup_timeouts_name_resources_and_attempt_both(self):
        self.ctx.cleanup_timeout_s = 0.005
        for hanging in ({'sideband'}, {'peer'}, {'sideband', 'peer'}):
            attempted = []

            async def close(name):
                attempted.append(name)
                if name in hanging:
                    await asyncio.Future()

            with self.subTest(hanging=hanging), \
                    patch.object(self.ctx.sideband_factory, 'close', lambda self: close('sideband')), \
                    patch.object(self.ctx.peer_factory, 'close', lambda self: close('peer')):
                result = await ex.E0(self.ctx)
            self.assertFalse(result['pass'])
            self.assertFalse(result['runs'][0]['clean_close'])
            expected = [name + ':TimeoutError' for name in ('sideband', 'peer') if name in hanging]
            self.assertEqual(result['runs'][0]['cleanup_errors'], expected)
            self.assertEqual(attempted, ['sideband', 'peer'])
            for error in expected:
                self.assertIn(error, self.ctx.recorder.summary)

    async def test_unknown_shapes_do_not_raise(self):
        self.script = [None, [], {'type': 'delegation.created', 'item': []}, {'type': 'turn.done'}]
        result = await ex.E1(self.ctx)
        self.assertFalse(result['pass'])
        self.assertEqual(result['runs'][0]['events'], self.script)


if __name__ == '__main__':
    unittest.main()
