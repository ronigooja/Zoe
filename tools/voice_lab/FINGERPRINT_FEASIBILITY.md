# Codex fingerprint feasibility

Applies when evaluating the voice lab against the supplied Codex CLI 0.167.0 macOS arm64 capture. This is offline evidence, not AVAS acceptance evidence. Following explicit approval, the approximate profile is implemented as the default for both transports; legacy-browser remains selectable. Session body and media behavior are unchanged.

## Approved approximate profile verification

OBSERVED: the implementation verifier ran once, including four bounded gap probes. Adding the named SCSV cipher did not emit 255. Reducing the POST signature list did not eliminate padding. Both named and numeric attempts to add WebSocket PSS-PSS algorithms failed before a parseable ClientHello. INFERRED: these remain TLS-engine limitations for the tested options; unsupported signature names/IDs are the likely boundary, not a cipher-suite failure. Exact engine error diagnosis and individual-algorithm isolation remain UNVERIFIED.

OBSERVED POST: JA3 `ae5ca63f3c7ca1d2fc1cd10f92330dcc`, JA4 `t12d210800_f000828355c1_b9b2cd098495`; DIFFERS from reference JA3 `e4d448cdfe06dc1243c1eb026c74ac9a`, JA4 `t12d220700_0d4ca5d4ec72_3304d8368043` by missing SCSV and added padding.

OBSERVED WebSocket: JA3 `59bde577bd23dc375013ae6564d619b2`, JA4 `t13d091000_f91f431d341e_f9531d972513`; DIFFERS from reference JA4 `t13d101000_61a7ad8aa9b6_0d308c48d2a3` by missing SCSV and final three signature algorithms. JA3 changes with extension order. The implementation randomizes the explicit ten-extension allowlist once per connection, rather than enabling the engine's unrestricted permutation. Randomization distribution and repeated-connection behavior are UNVERIFIED. No reference WebSocket JA3 hash was supplied.

OBSERVED: the same run validated both published hash samples (2/2) and exact WebSocket header order/case; POST custom header order matched with required HTTP/1.1 Host last. Both transports omit ALPN. The verifier uses the production TLS settings but synthetic sync clients; async lifecycle is covered only by fake tests.

OBSERVED: the initial implementation suite run executed 88 tests and failed with 16 failures and 17 errors. The legacy secret-list contract, Codex default-UA validation, and runner fixtures were corrected afterward. A subsequently authorized full offline run with all pinned requirements installed passed: 88 tests, OK (1.515 seconds); no further fixes or repeat runs were needed. New fake tests cover ordered headers, absent browser fields, per-call/fixed IDs, persona allowlists, dry-run privacy, proxy pass-through, close handling, and connect cancellation. Existing tests retain legacy and E/P orchestration coverage. This does not verify real proxy interoperability or live AVAS behavior.

The sections below preserve the initial feasibility investigation; statements that integration was not implemented describe that earlier stage, not the approved implementation above.

## Evidence

OBSERVED locally with the latest stable package selected by pip, `curl_cffi==0.17.0`, on macOS: a stdlib loopback TLS server received one POST and one WebSocket upgrade using custom settings. No upstream service, real account database, credentials, or proxy was used. The installed Python implementation was inspected directly (`requests/utils.py`, `requests/session.py`, `requests/websockets.py`, `requests/impersonate.py`, `fingerprints.py`, `const.py`). Package source: <https://pypi.org/project/curl-cffi/0.17.0/>. Temporary installation destination: `/tmp/vl-fp-venv`; removed after investigation.

The JA3 canonical Salesforce example validates to `ada70206e40642a3e4461f35503241d5`. The FoxIO example validates to `t13d1516h2_8daaf6152771_02713d6af862`. Both assertions passed in the completed measurement invocation. An initial invocation caught an incorrectly transcribed FoxIO extension list; it was corrected before measurement. The existing unit suite was not run or changed.

Reference capture hashes are in [cases/codex_fingerprint.json](cases/codex_fingerprint.json). [verify_fingerprint.py](verify_fingerprint.py) is an experimental verifier, not a live-client adapter. It uses generated loopback-only TLS certificate material and synthetic header values, never account inputs. Output contains TLS counts/hashes and header names only. The capture expects one complete TLS record in a socket peek and explicitly fails on partial records; fragmented ClientHello support remains outside this feasibility probe.

## Measured differences

| Property | Create-call POST | WebSocket |
| --- | --- | --- |
| JA4 reference | `t12d220700_0d4ca5d4ec72_3304d8368043` | `t13d101000_61a7ad8aa9b6_0d308c48d2a3` |
| JA4 measured | `t12d210800_f000828355c1_b9b2cd098495` | `t13d091000_f91f431d341e_f9531d972513` |
| JA3 measured | `ae5ca63f3c7ca1d2fc1cd10f92330dcc` | `9fcc4953c6ae05cb48c583af31c8924f` |
| Ciphers | All 21 actual suites match, in order; missing leading SCSV `255` | All nine actual suites match, in order; missing trailing SCSV `255` |
| Extensions | Reference seven, in order, plus automatic padding `21` | Reference ten, in supplied order; permutation not measured |
| Groups | Exact `23,24,25` | Exact `4588,29,23,24` |
| Signature algorithms | Exact eight, in order | First ten match; omitted final `2308,2309,2310` |
| TLS versions | TLS 1.2 only, no supported_versions | TLS 1.3 + supported_versions `772,771` |
| ALPN | Absent | Absent |

OBSERVED: the full WebSocket signature list caused connection failure before a ClientHello was captured. The closest successful settings omit `rsa_pss_pss_sha256`, `rsa_pss_pss_sha384`, and `rsa_pss_pss_sha512`. INFERRED: this package's TLS engine cannot use that requested full signature list; these three algorithms are the differing settings, but their individual support was not exhaustively tested.

OBSERVED POST HTTP/1.1 header order: `openai-alpha, x-session-id, session-id, thread-id, authorization, chatgpt-account-id, content-type, originator, user-agent, accept, content-length, Host`. All requested regular header names/order match, with HTTP/1.1's required Host additionally present. The supplied HTTP/2 pseudo-header capture was not reproduced or decoded; no ALPN is consistent with HTTP/1.1, but the real binary's default request version remains INFERRED. POST HTTP/1.1 lower-case custom header names were preserved.

OBSERVED WebSocket order/case matches exactly: `Host, Connection, Upgrade, Sec-WebSocket-Version, Sec-WebSocket-Key, user-agent, originator, chatgpt-account-id, authorization, thread-id, openai-alpha, x-session-id, session-id`. No browser headers appeared in either measurement. These are verifier-only builders, not proof about the unchanged live lab.

## API feasibility

- VERIFIED in installed source: `ja3=` configures cipher/group/extension ordering but rejects SCSV `255` at its cipher-name lookup. The custom `Fingerprint` interface bypasses that lookup but cannot add SCSV as a regular cipher. Source comments explicitly note padding is TLS-engine-managed.
- OBSERVED: the suspected CBC/3DES limitation does not apply to this wheel. All requested POST suites, including `49162/49161/49160/49172/49171/49170/10`, appear on the wire.
- VERIFIED in source and OBSERVED on loopback: `http_version='v1'`, `default_headers=False`, `accept_encoding=None`, explicit ordered headers, `SSL_ENABLE_ALPN=0` through `Fingerprint.tls_alpn`, and `SSLVERSION=6 | (6 << 16)` produce a TLS-1.2-only POST without supported_versions or ALPN. Do not assume `ja3='771,...'` alone caps TLS at 1.2.
- VERIFIED in source: `akamai=` controls HTTP/2 SETTINGS, window update, stream settings, and pseudo-header order. The real capture supplies no SETTINGS reference; matching those is UNVERIFIED.
- VERIFIED in source: `extra_fp=` supports signature algorithms, GREASE, extension permutation, and regular header ordering. `Fingerprint` additionally supports `ws_header_order`; the corresponding `CurlOpt.WS_HTTPHEADER_ORDER` matched the observed WebSocket header case/order. No manual WebSocket upgrade protocol implementation is necessary.
- VERIFIED in source: `Session.ws_connect` and `AsyncSession.ws_connect` exist, with configurable proxy URLs/auth and custom fingerprints. Session APIs support explicit `trust_env=False`. Async integration, cancellation, close handling, recorder redaction, and fake-transport regression coverage have not been implemented.
- UNVERIFIED: extension permutation's distribution and exact allowed extension set, authenticated HTTP/SOCKS proxy interoperability, actual account proxy URL compatibility, media behavior, HTTP/2 wire order, and live AVAS acceptance. No account URL was inspected.

## Existing lab body and media boundaries

VERIFIED in `lab/upstream.py`: root keys already are `sdp, session`. `build_session` sends `model, instructions, audio.output.voice, delegation, initial_items`. Relative to the supplied real capture it lacks `session.type` and `audio.input.format.{type,rate}`, and adds `delegation` and `initial_items`; key order also differs. These experiment-bearing semantics were intentionally not changed.

VERIFIED in `lab/webrtc.py`: the committed peer constructs aiortc with no ICE servers and has no account-proxy argument. HTTP and sideband do receive the account proxy, but this source does not establish a working proxied media path. Historical RESULTS claims are not independent verification of this committed peer implementation.

## Decision and alternatives

Exact Codex TLS parity cannot be claimed with the measured stock curl_cffi settings. Replacing httpx/websockets now would change both wire implementations while leaving both target TLS hashes unmatched. No transport migration or dependency pin was made, and no branch was pushed.

Closest alternative: explicitly accept a header-faithful, TLS-approximate profile using the measured settings and implement it behind the existing abstractions with separate regression tests. For exact parity, investigate the actual macOS native TLS stack for the HTTP leg and a Rust rustls/tungstenite helper matching the binary's dependencies/configuration for the WebSocket leg, or a maintained custom TLS-engine fork with explicit SCSV/padding/signature controls. Those are proposals, not implementations or verified matches.
