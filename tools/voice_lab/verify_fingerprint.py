#!/usr/bin/env python3
"""Offline ClientHello feasibility measurements; all sockets bind to loopback."""

import hashlib
import json
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
from pathlib import Path


def digest(value, algorithm='sha256', size=12):
    return hashlib.new(algorithm, value.encode('ascii')).hexdigest()[:size]


def grease(value):
    return value & 0x0f0f == 0x0a0a and value >> 8 == value & 255


def words(data):
    return list(struct.unpack('!' + 'H' * (len(data) // 2), data))


def fingerprints(version, ciphers, extensions, groups, formats, signatures, versions, alpn, sni):
    ciphers = [x for x in ciphers if not grease(x)]
    extensions = [x for x in extensions if not grease(x)]
    groups = [x for x in groups if not grease(x)]
    ja3 = ','.join([str(version), '-'.join(map(str, ciphers)),
                    '-'.join(map(str, extensions)), '-'.join(map(str, groups)),
                    '-'.join(map(str, formats))])
    selected = max([x for x in versions if not grease(x)] or [version])
    tls = {769: '10', 770: '11', 771: '12', 772: '13'}.get(selected, '00')
    protocol = (chr(alpn[0]) + chr(alpn[-1])) if alpn else '00'
    a = f't{tls}{"d" if sni else "i"}{len(ciphers):02d}{len(extensions):02d}{protocol}'
    cipher_text = ','.join(f'{x:04x}' for x in sorted(ciphers))
    ext_text = ','.join(f'{x:04x}' for x in sorted(extensions) if x not in (0, 16))
    sig_text = ','.join(f'{x:04x}' for x in signatures)
    return {'ja3': ja3, 'ja3_md5': digest(ja3, 'md5', 32),
            'ja4': a + '_' + digest(cipher_text) + '_' + digest(ext_text + ('_' + sig_text if sig_text else '')),
            'cipher_count': len(ciphers), 'extension_count': len(extensions),
            'ciphers': ciphers, 'extensions': extensions, 'groups': groups,
            'signatures': signatures, 'versions': versions, 'alpn': bool(alpn)}


def parse_hello(data):
    if data[0] != 1:
        raise ValueError('Expected ClientHello')
    data = memoryview(data)[4:]
    version = int.from_bytes(data[:2])
    offset = 34
    offset += 1 + data[offset]
    length = int.from_bytes(data[offset:offset + 2]); offset += 2
    ciphers = words(data[offset:offset + length]); offset += length
    offset += 1 + data[offset]
    length = int.from_bytes(data[offset:offset + 2]); offset += 2
    end = offset + length
    extensions, groups, formats, signatures, versions, alpn = [], [], [], [], [], b''
    while offset < end:
        kind, length = struct.unpack('!HH', data[offset:offset + 4]); offset += 4
        value = bytes(data[offset:offset + length]); offset += length
        extensions.append(kind)
        if kind == 10:
            groups = words(value[2:])
        elif kind == 11:
            formats = list(value[1:])
        elif kind == 13:
            signatures = words(value[2:])
        elif kind == 43:
            versions = words(value[1:])
        elif kind == 16:
            alpn = value[3:3 + value[2]]
    return fingerprints(version, ciphers, extensions, groups, formats, signatures,
                        versions, alpn, 0 in extensions)


def receive_exact(connection, size):
    result = b''
    while len(result) < size:
        piece = connection.recv(size - len(result))
        if not piece:
            raise ValueError('Truncated TLS record')
        result += piece
    return result


def capture_http(client, context):
    result = {}
    with socket.socket() as server:
        server.bind(('127.0.0.1', 0)); server.listen(1); server.settimeout(4)
        port = server.getsockname()[1]

        def listen():
            try:
                with server.accept()[0] as raw:
                    raw.settimeout(3)
                    # This feasibility probe expects one complete TLS record.
                    # Fail explicitly rather than hash a partial ClientHello.
                    header = raw.recv(5, socket.MSG_PEEK)
                    size = 5 + int.from_bytes(header[3:5])
                    record = raw.recv(size, socket.MSG_PEEK)
                    if len(record) != size:
                        raise ValueError('Partial peek')
                    result.update(parse_hello(record[5:]))
                    with context.wrap_socket(raw, server_side=True) as connection:
                        data = b''
                        while b'\r\n\r\n' not in data:
                            data += connection.recv(8192)
                        lines = data.split(b'\r\n\r\n')[0].split(b'\r\n')
                        result['header_order'] = [line.split(b':', 1)[0].decode('ascii') for line in lines[1:]]
                        if lines[0].startswith(b'GET'):
                            import base64
                            key = next(line.split(b':', 1)[1].strip() for line in lines if line.lower().startswith(b'sec-websocket-key:'))
                            accept = base64.b64encode(hashlib.sha1(key + b'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest())
                            connection.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + b'\r\n\r\n')
                        else:
                            connection.sendall(b'HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
            except Exception as exc:
                result['capture_error'] = type(exc).__name__

        thread = threading.Thread(target=listen); thread.start()
        try:
            client(port)
        except Exception as exc:
            result['client_error'] = type(exc).__name__
        thread.join()
    return result


def validate_samples():
    sample = '769,47-53-5-10-49161-49162-49171-49172-50-56-19-4,0-10-11,23-24-25,0'
    assert digest(sample, 'md5', 32) == 'ada70206e40642a3e4461f35503241d5'
    result = fingerprints(771, [int(x, 16) for x in '002f 0035 009c 009d 1301 1302 1303 c013 c014 c02b c02c c02f c030 cca8 cca9'.split()],
                          [int(x, 16) for x in '0000 0005 000a 000b 000d 0010 0012 0017 001b 0023 002b 002d 0033 4469 fe0d ff01'.split()],
                          [], [], [int(x, 16) for x in '0403 0804 0401 0503 0805 0501 0806 0601'.split()],
                          [772, 771], b'h2', True)
    assert result['ja4'] == 't13d1516h2_8daaf6152771_02713d6af862', result['ja4']


POST_CIPHERS = [255, 49196, 49195, 49188, 49187, 49162, 49161, 49160,
                49200, 49199, 49192, 49191, 49172, 49171, 49170, 157, 156,
                61, 60, 53, 47, 10]
WS_CIPHERS = [4866, 4865, 4867, 49196, 49195, 52393, 49200, 49199, 52392, 255]


def measure(context):
    from curl_cffi import CurlOpt
    from curl_cffi.fingerprints import Fingerprint
    from curl_cffi.requests import Session
    from curl_cffi.requests.impersonate import TLS_CIPHER_NAME_MAP

    for kind, ciphers, groups, signatures, extensions in [
        ('post', POST_CIPHERS, ['P-256', 'P-384', 'P-521'],
         ['rsa_pkcs1_sha256', 'rsa_pkcs1_sha1', 'rsa_pkcs1_sha384', 'rsa_pkcs1_sha512',
          'ecdsa_secp256r1_sha256', 'ecdsa_sha1', 'ecdsa_secp384r1_sha384', 'ecdsa_secp521r1_sha512'],
         '0-10-11-13-5-18-23'),
        ('websocket', WS_CIPHERS, ['X25519MLKEM768', 'X25519', 'P-256', 'P-384'],
         ['ecdsa_secp384r1_sha384', 'ecdsa_secp256r1_sha256', 'ecdsa_secp521r1_sha512',
          'ed25519', 'rsa_pss_rsae_sha512', 'rsa_pss_rsae_sha384', 'rsa_pss_rsae_sha256',
          'rsa_pkcs1_sha512', 'rsa_pkcs1_sha384', 'rsa_pkcs1_sha256'],
         '51-35-0-43-23-13-11-5-10-45'),
    ]:
        from lab.fingerprint import settings
        options, kwargs = settings(websocket=kind == 'websocket')
        order = (['openai-alpha', 'x-session-id', 'session-id', 'thread-id', 'authorization',
                  'chatgpt-account-id', 'content-type', 'originator', 'user-agent', 'accept', 'content-length']
                 if kind == 'post' else
                 ['Host', 'Connection', 'Upgrade', 'Sec-WebSocket-Version', 'Sec-WebSocket-Key',
                  'user-agent', 'originator', 'chatgpt-account-id', 'authorization', 'thread-id',
                  'openai-alpha', 'x-session-id', 'session-id'])
        options[CurlOpt.HTTPHEADER_ORDER if kind == 'post' else CurlOpt.WS_HTTPHEADER_ORDER] = ','.join(order)
        headers = {name: 'fixture' for name in order if name.lower() not in
                   {'host', 'connection', 'upgrade', 'sec-websocket-version', 'sec-websocket-key'}}
        if kind == 'post':
            headers.update({'content-type': 'application/json', 'accept': '*/*', 'content-length': '2'})
        with Session(trust_env=False, curl_options=options) as session:
            def client(port):
                url = ('https' if kind == 'post' else 'wss') + f'://localhost:{port}/'
                request_kwargs = dict(kwargs, timeout=2, verify=False, headers=headers)
                if kind == 'post':
                    session.post(url, data=b'{}', **request_kwargs)
                else:
                    session.ws_connect(url, **request_kwargs)
            result = capture_http(client, context)
        reference = json.loads((Path(__file__).parent / 'cases' / 'codex_fingerprint.json').read_text())[kind]
        output = {key: result[key] for key in ('ja3_md5', 'ja4', 'cipher_count', 'extension_count', 'header_order', 'client_error', 'capture_error') if key in result}
        output.update({'transport': kind, 'ja4_match': result.get('ja4') == reference['ja4']})
        print(json.dumps(output, sort_keys=True))
        if kind == 'post':
            post_result = result
    return post_result, result


def gap_probes(context):
    """One bounded retry per suspected gap; synthetic loopback inputs only."""
    from curl_cffi import CurlOpt
    from curl_cffi.requests import Session
    from lab.fingerprint import settings
    for label in ('scsv', 'padding', 'pss-pss', 'pss-pss-numeric'):
        websocket = label.startswith('pss')
        options, kwargs = settings(websocket)
        fp = kwargs['impersonate']
        if label == 'scsv':
            fp.tls_ciphers.insert(0, 'TLS_EMPTY_RENEGOTIATION_INFO_SCSV')
        elif label == 'padding':
            # Explicit extension order already excludes 21; repeat with a
            # shorter supported signature list to test size-driven padding.
            fp.tls_signature_hashes = ['rsa_pkcs1_sha256']
        elif label == 'pss-pss':
            fp.tls_signature_hashes += ['rsa_pss_pss_sha256', 'rsa_pss_pss_sha384', 'rsa_pss_pss_sha512']
        else:
            options[CurlOpt.SSL_SIGNATURE_ALGORITHMS] = '0x0503:0x0403:0x0603:0x0807:0x0806:0x0805:0x0804:0x0601:0x0501:0x0401:0x0904:0x0905:0x0906'
        with Session(trust_env=False, curl_options=options) as session:
            def client(port):
                request = dict(kwargs, timeout=2, verify=False)
                url = ('wss' if websocket else 'https') + f'://localhost:{port}/'
                if websocket:
                    session.ws_connect(url, **request)
                else:
                    session.post(url, data=b'{}', **request)
            result = capture_http(client, context)
        print(json.dumps({'gap_probe': label, **{key: result[key] for key in
                         ('ja3_md5', 'ja4', 'cipher_count', 'extension_count', 'client_error', 'capture_error') if key in result}}, sort_keys=True))


if __name__ == '__main__':
    validate_samples()
    print('published_samples=2 passed=2')
    with tempfile.TemporaryDirectory(prefix='vl-fp-', dir='/tmp') as temporary:
        cert, key = Path(temporary) / 'cert.pem', Path(temporary) / 'key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-keyout', str(key), '-out', str(cert), '-days', '1',
                        '-subj', '/CN=localhost'], check=True, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        gap_probes(context)
        measure(context)
