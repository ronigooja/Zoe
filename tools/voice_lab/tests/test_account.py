import base64
import dataclasses
import sqlite3
import tempfile
import unittest
from pathlib import Path

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from lab.account import Account, load_account, open_sealed, parse_key


KEY = bytes(range(32))


def seal(text):
    nonce = bytes(range(12))
    raw = nonce + AESGCM(KEY).encrypt(nonce, text.encode(), None)
    return 'enc1.' + base64.urlsafe_b64encode(raw).decode().rstrip('=')


class AccountTests(unittest.TestCase):
    def test_key_encodings(self):
        for value in [KEY.hex(), base64.b64encode(KEY).decode(),
                      base64.b64encode(KEY).decode().rstrip('='),
                      base64.urlsafe_b64encode(KEY).decode(),
                      base64.urlsafe_b64encode(KEY).decode().rstrip('=')]:
            self.assertEqual(parse_key(' \n' + value + '\n'), KEY)
        for value in ['', '00', 'not-a-key', 'g' * 64]:
            with self.assertRaises(ValueError):
                parse_key(value)

    def test_known_vector_and_plaintext(self):
        self.assertEqual(open_sealed(KEY, seal('fixture-token')), 'fixture-token')
        self.assertEqual(open_sealed(KEY, 'legacy plaintext'), 'legacy plaintext')
        self.assertEqual(open_sealed(KEY, ''), '')

    def test_wrong_key_and_truncation(self):
        with self.assertRaises(ValueError):
            open_sealed(bytes(reversed(KEY)), seal('fixture-token'))
        for value in ['enc1.', 'enc1.YQ', seal('fixture-token')[:-4], 'enc1.%%%%']:
            with self.assertRaises(ValueError):
                open_sealed(KEY, value)

    def test_read_only_copy_join_and_no_mutation(self):
        with tempfile.TemporaryDirectory(prefix='voice-lab-test-') as temporary:
            root = Path(temporary)
            db = root / 'copy.db'
            key = root / 'key'
            key.write_text(KEY.hex())
            connection = sqlite3.connect(db)
            connection.executescript(
                'CREATE TABLE accounts (id INTEGER PRIMARY KEY, access_token TEXT, proxy TEXT);'
                'CREATE TABLE bridge_accounts (account_id INTEGER, device_id TEXT);')
            connection.execute('INSERT INTO accounts VALUES (?, ?, ?)',
                               (7, seal('fixture-token'), seal('fixture-proxy')))
            connection.execute('INSERT INTO bridge_accounts VALUES (?, ?)',
                               (7, seal('fixture-device')))
            connection.commit()
            connection.close()
            before = db.read_bytes()
            account = load_account(str(db), str(key), 7)
            self.assertEqual(account, Account('acct-7', 'fixture-token', 'fixture-proxy',
                                             'fixture-device'))
            self.assertEqual(db.read_bytes(), before)
            for secret in ['fixture-token', 'fixture-proxy', 'fixture-device']:
                self.assertNotIn(secret, repr(account))
                self.assertNotIn(secret, str(account))
            with self.assertRaises(dataclasses.FrozenInstanceError):
                account.token = 'replacement'
            with self.assertRaises(ValueError):
                load_account(str(db), str(key), 99)
            missing = root / 'missing.db'
            with self.assertRaises(ValueError):
                load_account(str(missing), str(key), 7)
            self.assertFalse(missing.exists())


if __name__ == '__main__':
    unittest.main()
