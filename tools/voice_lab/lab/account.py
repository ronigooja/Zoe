"""Read an operator-provided database copy without mutating account state."""

import base64
import binascii
import json
import sqlite3
from dataclasses import dataclass, field
from pathlib import Path

from cryptography.hazmat.primitives.ciphers.aead import AESGCM


def parse_key(raw: str) -> bytes:
    """Accept exactly 32 bytes encoded as hex or standard/URL-safe base64."""
    value = raw.strip()
    try:
        key = bytes.fromhex(value) if len(value) == 64 else b''
        if len(key) == 32:
            return key
    except ValueError:
        pass
    try:
        key = base64.b64decode(value + '=' * (-len(value) % 4), altchars=b'-_', validate=True)
        if len(key) == 32:
            return key
    except (ValueError, binascii.Error):
        pass
    raise ValueError('Key must encode exactly 32 bytes')


def open_sealed(key: bytes, stored: str) -> str:
    if not stored.startswith('enc1.'):
        return stored
    try:
        encoded = stored[5:]
        raw = base64.b64decode(encoded + '=' * (-len(encoded) % 4),
                               altchars=b'-_', validate=True)
        if len(key) != 32 or len(raw) < 28:
            raise ValueError()
        return AESGCM(key).decrypt(raw[:12], raw[12:], None).decode('utf-8')
    except Exception:
        raise ValueError('Unable to decrypt sealed value') from None


@dataclass(frozen=True, repr=False)
class Account:
    label: str
    token: str = field(repr=False)
    proxy: str = field(repr=False)
    device_id: str = field(repr=False)

    def __repr__(self):
        return '<Account redacted>'


def load_account(db_copy: str, key_file: str, account_id: int) -> Account:
    """Caller must supply a disposable, consistent SQLite backup, not a live DB."""
    try:
        if not isinstance(account_id, int) or isinstance(account_id, bool) or account_id <= 0:
            raise ValueError()
        key = parse_key(Path(key_file).read_text(encoding='ascii'))
        uri = Path(db_copy).resolve().as_uri() + '?mode=ro'
        connection = sqlite3.connect(uri, uri=True)
        try:
            connection.execute('PRAGMA query_only = ON')
            row = connection.execute(
                "SELECT access_token, COALESCE(proxy, ''), "
                "COALESCE((SELECT device_id FROM bridge_accounts "
                "WHERE account_id = accounts.id), '') FROM accounts WHERE id = ?",
                (account_id,),
            ).fetchone()
        finally:
            connection.close()
        if row is None:
            raise ValueError()
        values = [open_sealed(key, value) for value in row]
        if not values[0]:
            raise ValueError()
        return Account(f'acct-{account_id}', *values)
    except Exception:
        raise ValueError('Unable to read the selected account from the database copy') from None


PERSONA_HEADERS = frozenset({
    'accept', 'accept-language', 'cache-control', 'pragma', 'priority', 'origin',
    'referer', 'user-agent', 'sec-ch-ua', 'sec-ch-ua-arch', 'sec-ch-ua-bitness',
    'sec-ch-ua-full-version', 'sec-ch-ua-full-version-list', 'sec-ch-ua-mobile',
    'sec-ch-ua-model', 'sec-ch-ua-platform', 'sec-ch-ua-platform-version',
    'sec-fetch-dest', 'sec-fetch-mode', 'sec-fetch-site', 'oai-device-id',
    'oai-session-id', 'oai-language', 'oai-client-version', 'oai-client-build-number',
})


def load_persona(path):
    """Read a JSON header-name to header-value map; do not load gateway config."""
    try:
        raw = json.loads(Path(path).read_text(encoding='utf-8'))
        if not isinstance(raw, dict):
            raise ValueError()
        result = {}
        for name, value in raw.items():
            if not isinstance(name, str) or name.lower() not in PERSONA_HEADERS:
                raise ValueError()
            if not isinstance(value, str) or '\r' in value or '\n' in value:
                raise ValueError()
            result[name.lower()] = value
        required = {'user-agent', 'oai-session-id', 'oai-language'}
        if not required.issubset(result):
            raise ValueError()
        return result
    except Exception:
        raise ValueError('Persona must be a JSON map of gateway persona headers') from None
