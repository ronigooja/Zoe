import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from .limits import decode, defaults, load_limits, reject_legacy_env


class LimitsTests(unittest.TestCase):
    def test_obsolete_environment_is_not_silently_ignored(self):
        with patch.dict('os.environ', {'ZOE_MAX_RUNS': ''}, clear=True):
            with self.assertRaisesRegex(ValueError, 'ZOE_MAX_RUNS.*max_concurrent_runs'):
                reject_legacy_env()

    def test_shared_loader_cases(self):
        root = Path(__file__).resolve().parents[2]
        source = (root / 'deploy/limits.json').read_bytes()
        self.assertEqual(defaults(), load_limits(root / 'deploy/limits.json'))
        for case in json.loads((root / 'internal/limits/testdata/cases.json').read_text()):
            with self.subTest(case=case['name']):
                data = source
                if 'raw' in case:
                    data = case['raw'].encode()
                if 'append' in case:
                    data += case['append'].encode()
                if 'hex' in case:
                    data = bytes.fromhex(case['hex'])
                if 'size' in case:
                    data = b' ' * case['size']
                if 'value' in case:
                    fields = json.loads(source)
                    fields[case.get('key', 'opening_context_runes')] = 'replacement'
                    data = json.dumps(fields).replace('"replacement"', case['value']).encode()
                if 'error' in case:
                    with self.assertRaises(ValueError) as error:
                        decode(data)
                    self.assertEqual(str(error.exception), case['error'])
                else:
                    result = decode(data)
                    with self.assertRaises(TypeError):
                        result['opening_context_runes'] = 1

    def test_path_override_and_read_failures(self):
        with tempfile.TemporaryDirectory() as root:
            file = Path(root) / 'limits.json'
            with patch.dict('os.environ', {'ZOE_LIMITS_FILE': str(file)}):
                with self.assertRaisesRegex(ValueError, '^limits_missing$'):
                    load_limits()
                file.touch(mode=0)
                with self.assertRaisesRegex(ValueError, '^limits_unreadable$'):
                    load_limits()
                file.chmod(0o600)
                with self.assertRaisesRegex(ValueError, '^limits_empty$'):
                    load_limits()
                with self.assertRaisesRegex(ValueError, '^limits_unreadable$'):
                    load_limits(root)
