import contextlib
import io
import json
import re
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from lab.record import Recorder, make_run_dir, redact, slim_event


class RecordTests(unittest.TestCase):
    def test_audio_is_slimmed_in_events_and_nested_observations(self):
        events = [{'type': kind, field: 'A' * 10000, 'start_ms': 12, 'end_ms': 34,
                   'extra': 'fixture-secret'} for kind, field in
                  (('session.input_audio.append', 'audio'), ('session.output_audio.delta', 'delta'))]
        untouched = {'type': 'turn.delta', 'delta': 'spoken words', 'audio': 'keep'}
        with tempfile.TemporaryDirectory(prefix='voice-lab-test-') as temporary:
            recorder = Recorder(make_run_dir(temporary, 'E1'), ['fixture-secret'])
            for event in events + [untouched]:
                recorder.event('in', event)
            recorder.event('observation', {'events': events})
            rows = [json.loads(line)['event'] for line in recorder.events_path.read_text().splitlines()]
            for index, field in enumerate(('audio', 'delta')):
                expected = {**events[index], field: '<10000 chars>', 'extra': '[redacted]'}
                self.assertEqual(rows[index], expected)
                self.assertEqual(rows[3]['events'][index], expected)
                self.assertEqual(slim_event(slim_event(events[index])), slim_event(events[index]))
            self.assertEqual(rows[2], untouched)
            self.assertLess(len(recorder.events_path.read_text()), 1500)
            self.assertEqual(len(events[0]['audio']), 10000)

    def test_literal_redaction(self):
        self.assertEqual(redact('a+b and a+b', ['a+b', '']),
                         '[redacted] and [redacted]')
        self.assertEqual(redact('abcdef', ['abc', 'abcdef']), '[redacted]')

    def test_order_offsets_and_shape(self):
        with tempfile.TemporaryDirectory(prefix='voice-lab-test-') as temporary:
            run_dir = make_run_dir(temporary, 'E0')
            self.assertRegex(run_dir.name, r'^\d{8}T\d{6}\.\d{6}Z-E0$')
            with patch('lab.record.time.monotonic', side_effect=[10, 10.125, 10.25]):
                recorder = Recorder(run_dir, [])
                recorder.event('send', {'type': 'first'})
                recorder.event('receive', {'type': 'second'})
            rows = [json.loads(line) for line in
                    (run_dir / 'events.jsonl').read_text().splitlines()]
            self.assertEqual(rows, [
                {'t_ms': 125, 'dir': 'send', 'event': {'type': 'first'}},
                {'t_ms': 250, 'dir': 'receive', 'event': {'type': 'second'}}])

    def test_secrets_never_reach_files_or_stdout(self):
        secrets = ['fixture-token', 'https://fixture-proxy.invalid:9876',
                   'fixture-device', 'fixture-key', 'fixture@example.invalid',
                   'escaped"\\secret', '\u79d8\u5bc6-value']
        stdout = io.StringIO()
        with tempfile.TemporaryDirectory(prefix='voice-lab-test-') as temporary:
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stdout):
                recorder = Recorder(make_run_dir(temporary, 'E1'), secrets)
                recorder.event('receive', {'type': 'fixture', 'payload': secrets,
                                           secrets[0]: secrets[1]})
                recorder.write_session({'instructions': ' '.join(secrets), 'nested': secrets})
                recorder.write_summary('# Result\n' + ' '.join(secrets))
            for name in ['events.jsonl', 'summary.md', 'session.json']:
                text = (recorder.run_dir / name).read_text()
                self.assertIn('[redacted]', text)
                for secret in secrets:
                    self.assertNotIn(secret, text)
                    self.assertNotIn(json.dumps(secret)[1:-1], text)
            self.assertFalse(re.search('[\u3400-\u9fff]',
                                       (recorder.run_dir / 'summary.md').read_text()))
        self.assertEqual(stdout.getvalue(), '')


if __name__ == '__main__':
    unittest.main()
