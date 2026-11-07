import contextlib
import asyncio
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import ModuleType, SimpleNamespace
from unittest.mock import patch

import run
from lab.errors import LabError


class RunTests(unittest.TestCase):
    def args(self, experiment='E0'):
        args = ['--experiment', experiment, '--account-id', '123456789',
                '--db-copy', 'secret-db', '--key-file', 'secret-key',
                '--persona-file', 'secret-persona']
        if experiment != 'E0':
            args += ['--audio', 'input.wav' if experiment == 'E1' else 'audio']
        return args

    def test_dry_run_only_reads_cases_and_never_starts_execution(self):
        original = Path.read_text
        reads = []

        def read(path, *args, **kwargs):
            self.assertEqual(path.parent, run.ROOT / 'cases')
            reads.append(path.name)
            return original(path, *args, **kwargs)

        for index in range(8):
            output = io.StringIO()
            with patch.object(Path, 'read_text', read), patch.object(run, 'execute', side_effect=AssertionError), \
                    patch('socket.socket', side_effect=AssertionError), contextlib.redirect_stdout(output):
                self.assertEqual(run.main(self.args(f'E{index}') + ['--dry-run']), 0)
            text = output.getvalue()
            for secret in ('123456789', 'secret-db', 'secret-key', 'secret-persona'):
                self.assertNotIn(secret, text)
            plan = json.loads(text)
            self.assertEqual(plan['experiment'], f'E{index}')
            self.assertNotIn('instructions', plan['session'])
            self.assertEqual(plan['instructions_file'], {
                'name': 'instructions.txt',
                'length': len(original(run.ROOT / 'cases' / 'instructions.txt', encoding='utf-8'))})
        self.assertEqual(set(reads), {'instructions.txt', 'utterances.txt'})

    def test_invalid_arguments(self):
        invalid = [[], self.args() + ['--account-id', '0'], self.args() + ['--timeout-s', '0'], self.args() + ['--timeout-s', 'nan'],
                   self.args() + ['--timeout-s', '301'], self.args() + ['--experiment', 'E8'],
                   self.args() + ['--experiment', 'E1'],
                   self.args('E1') + ['--audio', 'folder'],
                   self.args('E2') + ['--audio', 'file.wav']]
        for args in invalid:
            with self.subTest(args=args), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                run.main(args + ['--dry-run'])

    def test_main_path_wires_fixed_contract_with_fake_modules(self):
        modules = {name: ModuleType(name) for name in
                   ('lab.account', 'lab.record', 'lab.upstream', 'lab.sideband', 'lab.webrtc', 'lab.experiments')}
        account = SimpleNamespace(token='test-token', proxy='test-proxy', device_id='test-device')
        modules['lab.account'].load_account = lambda *args: account
        captures = {}

        def recorder(path, secrets):
            captures['secrets'] = secrets
            return object()

        modules['lab.record'].Recorder = recorder
        modules['lab.record'].make_run_dir = lambda *args: Path('unused')
        modules['lab.sideband'].Sideband = object
        modules['lab.sideband'].context_frames = object
        modules['lab.sideband'].user_text_frame = object
        modules['lab.webrtc'].Peer = object
        modules['lab.experiments'].Context = lambda **kw: SimpleNamespace(**kw)

        async def experiment(context):
            captures['context'] = context
            return {'pass': True}

        modules['lab.experiments'].EXPERIMENTS = {'E0': experiment}
        args = run.parser().parse_args(self.args())
        # Also shadow package attributes: another test may already have imported a real module.
        import lab
        with patch.dict(sys.modules, modules), patch.object(lab, 'upstream', modules['lab.upstream'], create=True), \
                patch.object(Path, 'read_text', return_value='{"OAI-Device-ID":"persona-device"}'):
            result = asyncio.run(run.execute(args))
        self.assertTrue(result['pass'])
        self.assertEqual(captures['secrets'], ['test-token', 'test-proxy', 'test-device', 'persona-device'])
        self.assertIs(captures['context'].account, account)
        self.assertEqual(captures['context'].paths['instructions'], run.ROOT / 'cases' / 'instructions.txt')
        args.instructions_file = Path('custom-prompt.txt')
        with patch.dict(sys.modules, modules), patch.object(lab, 'upstream', modules['lab.upstream'], create=True), \
                patch.object(Path, 'read_text', return_value='{}'):
            asyncio.run(run.execute(args))
        self.assertEqual(captures['context'].paths['instructions'], args.instructions_file)

    def test_instructions_option_and_dry_run_metadata_only(self):
        self.assertEqual(run.parser().parse_args(self.args()).instructions_file,
                         run.ROOT / 'cases' / 'instructions.txt')
        with tempfile.TemporaryDirectory(prefix='voice-lab-instructions-') as temporary:
            path = Path(temporary) / 'custom-prompt.txt'
            text = 'Private fixture instruction text.'
            path.write_text(text, encoding='utf-8')
            output = io.StringIO()
            with patch.object(run, 'execute', side_effect=AssertionError), contextlib.redirect_stdout(output):
                self.assertEqual(run.main(self.args('E2') + ['--instructions-file', str(path), '--dry-run']), 0)
            plan = json.loads(output.getvalue())
            self.assertEqual(plan['instructions_file'], {'name': path.name, 'length': len(text)})
            self.assertEqual(plan['session']['instructions_length'], len(text))
            self.assertNotIn(text, output.getvalue())
            self.assertNotIn(str(path.parent), output.getvalue())

    def test_device_ids_are_redacted_from_all_recorded_outputs(self):
        account = SimpleNamespace(token='fixture-token', proxy='', device_id='account-device')
        persona = {'OAI-Device-ID': 'persona-device'}
        payload = 'account-device persona-device'

        async def experiment(context):
            context.recorder.event('observation', {payload: payload})
            context.recorder.write_session({'instructions': payload})
            context.recorder.write_summary('# Fixture\n' + payload)
            return {'pass': True}

        args = run.parser().parse_args(self.args())
        with tempfile.TemporaryDirectory(prefix='voice-lab-redaction-') as directory:
            with patch('lab.account.load_account', return_value=account), \
                    patch('lab.record.make_run_dir', return_value=Path(directory)), \
                    patch('lab.experiments.EXPERIMENTS', {'E0': experiment}), \
                    patch.object(Path, 'read_text', return_value=json.dumps(persona)), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertTrue(asyncio.run(run.execute(args))['pass'])
            paths = list(Path(directory).iterdir())
            self.assertEqual({path.name for path in paths}, {'events.jsonl', 'session.json', 'summary.md'})
            for text in [output.getvalue()] + [path.read_text() for path in paths]:
                self.assertNotIn(account.device_id, text)
                self.assertNotIn(persona['OAI-Device-ID'], text)

    def test_main_suppresses_all_error_details(self):
        for error_type in (LabError, ValueError, RuntimeError):
            with self.subTest(error=error_type.__name__), \
                    patch.object(run, 'execute', side_effect=error_type('fixture-secret')), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(run.main(self.args()), 1)
            self.assertEqual(output.getvalue(), 'Experiment failed; sensitive error details suppressed.\n')


if __name__ == '__main__':
    unittest.main()
