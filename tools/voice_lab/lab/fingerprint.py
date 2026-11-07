"""Codex header identity and deliberately approximate, lazily loaded TLS settings."""

from uuid import uuid4
import random

USER_AGENT = 'codex_cli_rs/0.167.0 (Mac OS 15.5.0; arm64) Apple_Terminal/455'
CODEX_PERSONA_HEADERS = frozenset({'user-agent', 'originator', 'x-session-id',
                                    'session-id', 'thread-id'})
POST_ORDER = ('openai-alpha', 'x-session-id', 'session-id', 'thread-id',
              'authorization', 'chatgpt-account-id', 'content-type', 'originator',
              'user-agent', 'accept', 'content-length')
WS_ORDER = ('Host', 'Connection', 'Upgrade', 'Sec-WebSocket-Version',
            'Sec-WebSocket-Key', 'user-agent', 'originator', 'chatgpt-account-id',
            'authorization', 'thread-id', 'openai-alpha', 'x-session-id', 'session-id')


class Persona(dict):
    """Keep transport selection and private account identity out of header maps."""
    def __init__(self, values=(), fingerprint='codex', account_id=''):
        super().__init__(values)
        self.fingerprint = fingerprint
        self.account_id = account_id

    def __repr__(self):
        return '<Persona redacted>'


def is_codex(persona):
    return getattr(persona, 'fingerprint', 'legacy-browser') == 'codex'


def for_call(persona):
    if not is_codex(persona):
        return persona
    if any(name not in CODEX_PERSONA_HEADERS for name in persona):
        raise ValueError('Codex persona contains unsupported headers')
    result = Persona(persona, account_id=persona.account_id)
    identifier = str(uuid4())
    # UNVERIFIED whether real Codex uses equal values; one UUID per call here.
    for name in ('x-session-id', 'session-id', 'thread-id'):
        result.setdefault(name, identifier)
    result.setdefault('user-agent', USER_AGENT)
    result.setdefault('originator', 'codex_cli_rs')
    return result


def headers(persona, token, *, body_length=None):
    common = {'openai-alpha': 'quicksilver=v2', 'authorization': 'Bearer ' + token,
              'chatgpt-account-id': persona.account_id,
              'originator': persona.get('originator', 'codex_cli_rs'),
              'user-agent': persona.get('user-agent', USER_AGENT),
              **{name: persona[name] for name in ('x-session-id', 'session-id', 'thread-id')}}
    order = WS_ORDER[5:]
    if body_length is not None:
        common.update({'content-type': 'application/json', 'accept': '*/*',
                       'content-length': str(body_length)})
        order = POST_ORDER
    return {name: common[name] for name in order}


def settings(websocket=False):
    """No impersonation service, ambient proxies, browser defaults, or ALPN."""
    from curl_cffi import CurlOpt
    from curl_cffi.fingerprints import Fingerprint
    from curl_cffi.requests.impersonate import TLS_CIPHER_NAME_MAP
    ciphers = ([4866, 4865, 4867, 49196, 49195, 52393, 49200, 49199, 52392]
               if websocket else
               [49196, 49195, 49188, 49187, 49162, 49161, 49160, 49200, 49199,
                49192, 49191, 49172, 49171, 49170, 157, 156, 61, 60, 53, 47, 10])
    signatures = (['ecdsa_secp384r1_sha384', 'ecdsa_secp256r1_sha256',
                   'ecdsa_secp521r1_sha512', 'ed25519', 'rsa_pss_rsae_sha512',
                   'rsa_pss_rsae_sha384', 'rsa_pss_rsae_sha256', 'rsa_pkcs1_sha512',
                   'rsa_pkcs1_sha384', 'rsa_pkcs1_sha256'] if websocket else
                  ['rsa_pkcs1_sha256', 'rsa_pkcs1_sha1', 'rsa_pkcs1_sha384',
                   'rsa_pkcs1_sha512', 'ecdsa_secp256r1_sha256', 'ecdsa_sha1',
                   'ecdsa_secp384r1_sha384', 'ecdsa_secp521r1_sha512'])
    extensions = ['51', '35', '0', '43', '23', '13', '11', '5', '10', '45']
    if websocket:
        # Explicit allowlist + randomized order avoids enabling extra extensions.
        random.SystemRandom().shuffle(extensions)
    fp = Fingerprint(tls_ciphers=[TLS_CIPHER_NAME_MAP[x] for x in ciphers],
                     tls_supported_groups=(['X25519MLKEM768', 'X25519', 'P-256', 'P-384']
                                           if websocket else ['P-256', 'P-384', 'P-521']),
                     tls_signature_hashes=signatures, tls_alpn=False,
                     tls_extension_order=('-'.join(extensions) if websocket
                                          else '0-10-11-13-5-18-23'),
                     tls_session_ticket=websocket,
                     tls_signed_cert_timestamps=not websocket)
    options = {} if websocket else {CurlOpt.SSLVERSION: 6 | (6 << 16)}
    options[CurlOpt.WS_HTTPHEADER_ORDER if websocket else CurlOpt.HTTPHEADER_ORDER] = ','.join(WS_ORDER if websocket else POST_ORDER)
    return options, dict(impersonate=fp, default_headers=False, http_version='v1',
                         accept_encoding=None, allow_redirects=False)
