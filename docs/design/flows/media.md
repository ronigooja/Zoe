# Voice media, leases, activity and recovery

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. The target retains existing valid new-evidence semantics rather than weakening quarantine. User-visible recovery reflects actual state; ending media does not stop a task.

## Inputs and outputs

AVAS only. Paths are relative to the receiving HTTPS origin. Request bodies
and responses are JSON unless explicitly empty. Fields are required unless
marked optional. IDs are nonempty strings, generations and versions are
positive integers, times are UTC RFC3339 with optional fractional seconds.
Lifecycle operation names are documentation labels: the method and path
select the operation, with no `type`, `payload` or event ID envelope. Errors
use `{"error":{"code":"…","message":"…"}}` (optional `error.details`);
branch on status and code, not message text.

### Apple signaling and configuration

| Endpoint | Request | Success |
|---|---|---|
| Zoe `GET /v1/voice/config` | Instance bearer; no body. | `200`: existing `version`, `defaults`, `voices`, `voice_modes`, `languages`, `webrtc`, plus required string `gateway_dial_origin` (HTTPS scheme, host, optional port; no path, credentials, query or fragment). |
| Gateway `POST /client/v1/voice/calls` | Instance bearer; `request_id`, `offer_sdp`, optional top-level `voice`, `voice_mode`, `language_code` strings, as in Zoe's create payload. | `200`: `answer_sdp`, `gateway_session_id` (opaque hangup handle), `voice_session_id` (Zoe ID), `generation` (Zoe generation, for Zoe recovery). No lease fields or ICE server list. |
| Gateway `POST /client/v1/voice/calls/{gateway_session_id}/hangup` | Instance bearer; no body. | `204`, empty: the exact call's gateway close attempt finished or already finished. Not termination evidence or confirmation of pool reuse. |

Apple creates its offer without ICE servers; the gateway supplies a public host
candidate. Use the advertised settings IDs and defaults (omitted or empty settings select
the defaults). Zoe returns `503 voice_unavailable` if a usable dial origin
is not configured; Apple does not fall back to dialing Zoe. A dial requires
a nonblank `request_id` and nonblank `offer_sdp` (at most 1 MiB UTF-8).
Use the same `request_id` and fields on a retry; a new call uses a fresh ID.
Dial errors: `400 invalid_request` for malformed input or
unsupported settings, `401 unauthenticated` for missing or invalid instance
bearer, `409 voice_lease_busy` for a different active/unresolved attempt or
`409 idempotency_conflict` for changed input under the same `request_id`,
`503 voice_pool_quarantined` for a blocked pool, or `503 voice_unavailable`
for an unavailable service/negotiation or a dial replay with no live answer.
Hangup uses `401 unauthenticated`, `404 not_found` for an unknown/unowned
reference, or `503 voice_unavailable` if authorization or close cannot be
resolved. Apple keeps polling Zoe's `GET /v1/voice/pool/status` with its
instance bearer; dial and hangup do not replace that status.

### Gateway lifecycle reports to Zoe

Use the [voice runner credential](execution.md#required-behavior) on every
row. `{session_id}` is the Zoe `voice_session_id`, not the gateway handle.

| Operation and endpoint | Request | Committed response |
|---|---|---|
| `call.start`: `POST /runner/v1/voice/calls` | `X-Zoe-Instance-Key: <instance key>` (raw token, no `Bearer` prefix). Body is exactly the Apple dial fields above. | `201` first time, `200` on replay: `voice_session_id`, `generation`, `gateway_request_id` (durable Zoe-issued gateway attempt identity), `instructions` (string, may be empty), `opening_context: {kind, version, items}`, `opening_context_hash`. |
| `call.established`: `POST /runner/v1/voice/calls/{session_id}/established` | `generation` (Zoe), `gateway_session_id`, `media_generation`, `account_lease_id`, `account_lease_generation`, optional `opening_context` (gateway's create receipt, below). | `200 {"ok":true}` after atomic bind and activation, or exact replay. |
| `call.ended`: `POST /runner/v1/voice/calls/{session_id}/ended` | `generation` (Zoe), `gateway_session_id`, `media_generation`, `account_lease_id`, `account_lease_generation`, `reason`, `closed_at` (UTC time the gateway finished attempting close), optional `termination_evidence` (record below). | `200 {ok:true,state:"ended",upstream_termination:"verified",pool_blocked}` with verified closure, or `202 {ok:true,state:"quarantined",upstream_termination:"unverified",pool_blocked}` without it; `pool_blocked` is boolean. Both acknowledge durable reconciliation, not upstream reclamation. |

`call.start` uses the dial input validation, conflict and 503 codes. Missing
or invalid runner credentials use `401 unauthenticated`; missing or invalid
instance keys use that same error with `error.details.credential: "instance_key"`.
Disallowed runner scope returns `403 forbidden`. On this route
the gateway maps only the instance-key 401 to the dial's 401; runner auth
failure maps to `503 voice_unavailable`.
For `call.established` and `call.ended`, use the same 400/401/403 codes,
`404 not_found` for an unknown Zoe session and `409 voice_generation_stale`
for a stale binding or generation; changed replays use `409 idempotency_conflict`.
Unexpected server errors are 5xx. `call.ended.reason` is exactly
`user_hangup`, `upstream_voice_end`, `media_lost`, `gateway_shutdown` or `failure`.
Reject `closed_at` more than five minutes ahead of Zoe's receive time with
`400 invalid_request`.

The opening manifest is the existing `kind: "full"`, owner-scoped `version`
and ordered `items` array of `{role,text}` strings (empty array if none).
Use [opening item selection](results.md#required-behavior).
`opening_context_hash` is lowercase hex SHA-256 of Go `encoding/json.Marshal`
of that manifest, in `kind`, `version`, `items` order and `role`, `text` order
per item, with default escaping. The gateway applies the manifest and
instructions at create. The opening receipt attests that the gateway applied
and persisted the manifest locally at create, not that upstream consumed or
spoke it. Report the create path's `opening_context` receipt as produced:
boolean `applied`, `silent` when present (only JSON boolean `true` qualifies),
integer `version` and lowercase hex `content_hash` when present. Omit the
receipt if the create path produced none, including on an empty opening.

### Zoe reconciliation reads from the gateway

Use `Authorization: Bearer <gateway API key>` (Zoe's existing
`ZOE_VOICE_GATEWAY_API_KEY`). The gateway must make Apple-dialed sessions,
attempts, evidence, and in-call API operations available in the same
downstream owner namespace as this configured API key, not under an instance
key or browser login. The gateway selects the corresponding owner namespace
from deployment configuration, never from client input.

| Endpoint | Lookup key (no body) | Response |
|---|---|---|
| Gateway `GET /v1/voice/sessions/{gateway_session_id}/termination-evidence` | Path handle plus query `media_generation`, `account_lease_id`, `account_lease_generation`; both generations are decimal integers. | `200 {outcome,termination_evidence?}`: `released` with a verified evidence record; `not_yet` for a matched active/closing row; `unknown` if no exact trusted observation exists (including unknown/unowned records). Omit evidence unless `released`. `400 invalid_request` for malformed keys, `401 invalid_api_key` for bad/missing bearer, `503 voice_unavailable` for a failed lookup. |
| Gateway `GET /v1/voice/requests/{request_id}/outcome` (existing) | Path `request_id` is `gateway_request_id` from `call.start`, not the Apple `request_id`. | `200 {request_id,outcome,evidence_id?}`: `never_established`, `open`, `released` (requires `evidence_id`), or `unknown`; `404 not_found` if no visible attempt exists. |

The evidence record is the same on the lookup and `call.ended`: the existing
lease observation fields `voice_session_id` (the gateway handle),
`voice_session_generation` (the media generation), `account_lease_id`,
`account_lease_generation`, `account_id` (opaque gateway account alias, never
an upstream ID or credential), `terminated`, `evidence_id`, `observed_at`;
plus `reason` (stored close reason string) and the media row's
`peer_a_closed`, `peer_b_closed`, `data_channels_closed`, `tunnels_closed`
(booleans), `retired_legs`, `retired_legs_unclean` (nonnegative integers).
Export it only by joining the immutable `voice_termination_evidence` record
to the exact terminated media row and lease; the read never closes media or
mutates a lease. `terminated` and all four flags must be true,
`retired_legs_unclean` must be zero, and `evidence_id` must match
`ev_[0-9a-f]{32}`. Zoe checks all these fields, exact bound identity, a
nonblank account alias (store only its hash), and a valid, nonzero, nonfuture
`observed_at`. This attests gateway-local closure only.

## Required behavior

### AVAS dial authentication

The client stores only Zoe's address and instance key. It gets the gateway's
HTTPS dial origin from Zoe's voice configuration above, not from the
installation link. The client validates the gateway's TLS
certificate and hostname using platform trust. Deployment must make the
gateway's HTTPS dial endpoint reachable from the client's network.
The gateway client listener must sit behind a trusted TLS-terminating proxy:
the instance key travels as a bearer, and that listener neither enforces TLS
nor provides an admission rate limiter on the dial route.

The client sends the same instance key as a bearer over HTTPS to Zoe and to the
gateway. The `/client/v1/` routes do not use gateway `/v1/` API-key or browser
middleware. The gateway does not verify, hash or retain the instance key;
`call.start` forwards it in memory only alongside the runner bearer. Zoe checks
both identities and validates the client's key using the
[existing instance-key check and rotation](../../../internal/control/auth.go)
(constant-time SHA-256 digest comparison and credential generation), and
establishes the owner and device from the validated key. Rotation
takes effect immediately without synchronization to the gateway. For hangup,
the gateway forwards the currently presented bearer in memory only to Zoe's
existing owner-scoped `GET /v1/voice/sessions/{session_id}` for the Zoe ID bound
to the hangup handle (use `-` as a nonexistent Zoe ID for an unknown handle).
Require `200` with matching `voice_session_id` and
`generation` before closing that exact call, regardless of the returned state.
Keep this binding for idempotent hangup after close. Zoe `401`/`404` map to
hangup `401`/`404`; an unavailable or unverified response cannot close media.

Neither service logs, persists or echoes the presented instance key, including
in errors or telemetry. Redact `Authorization` and `X-Zoe-Instance-Key` from
logging and tracing; do not follow redirects on credentialed signaling or
lifecycle requests. The gateway rejects missing client bearers locally.

### Call lifecycle

The gateway owns the AVAS call. It must receive a successful `call.start`
response before allocating relay ports, selecting an upstream account, or
creating the upstream session or client relay. After negotiation the gateway
sends `call.established` and must receive Zoe's
acknowledgement of the committed binding before returning the dial response or
forwarding transcript, delegation or end reports. The gateway returns the
answer SDP only in the authenticated HTTPS dial response. The media connection
is the one negotiated by that offer and answer, including the SDP's ICE
credentials and DTLS fingerprints. `call.established` targets the row created
at `call.start`; later ingest sees the complete binding. In one transaction,
Zoe binds both media and lease identities, activates the session and, on a
strict receipt (`applied` and `silent` both true, exact `version` and
`content_hash`), consumes the opening reservations and records the
[background events](results.md#conversation-context-events). A missing receipt
does not consume or release reservations; a present nonmatching receipt releases
them without recording delivery. Release remaining unconsumed reservations when
the session reaches `failed` or `ended`. Acknowledge only after commit.

Zoe retains the authorizing runner ID with the call; reports from a different
runner are forbidden. Zoe retains the owner, device, Zoe session ID/generation,
`gateway_session_id`, `gateway_media_generation` and opening manifest/reservations. Results, fast
replies, text input and UI feedback sent upstream still require an active call
and are fenced by both session identities and generations.

The client owns the silence timer. Only valid interaction resets it; polling,
retries, stale or duplicate events, render chatter and transport health do not
count as activity. On silence expiry the client closes local media and sends a
normal gateway hangup, reported as `user_hangup`. The gateway does not time
silence. It observes media liveness, closes both legs on any end trigger and
reports `call.ended` with the close result. Zoe no longer calls gateway
`POST /v1/voice/sessions`, `DELETE /v1/voice/sessions/{id}`
or `POST /v1/voice/account-leases/{lease_id}/release`. The DELETE is not a Zoe
operator or reconciliation path. Zoe does not end calls on a context budget.

For client-initiated hangup with live media, the client requests gateway hangup
before closing local media; on media loss or gateway-initiated ending it closes
local media first.

The gateway selects the account and is authoritative for media admission,
account occupancy and quarantine. It must keep an unresolved lease unavailable
for reuse. `accounts` is the gateway inventory; `voice_creation_outcomes`,
`voice_media_sessions` and `voice_termination_evidence` record attempt, media
and close state. Zoe mirrors the gateway's opaque lease identity and
quarantine in its session records and `voice_pool_state`, and surfaces
`voice_pool_quarantined` when the pool is blocked. `gateway_unbound` and
`gateway_unbind_unknown` record unbinding knowledge, not proof of termination
or authority to reuse an account.

## Persistence and idempotency

- `call.start` deduplicates by `(owner, request_id)` before admission.
  Hash the decoded create payload with SHA-256 over Go `encoding/json.Marshal`:
  fields in order `request_id`, `offer_sdp`, `voice`, `voice_mode`,
  `language_code`; omit empty optional strings and do not normalize/default
  field values before hashing. The first success atomically stores the hash,
  `allocating` row, opening reservations and complete replay response. Enforce
  one active or unresolved attempt per owner. Exact replay returns that saved
  response regardless of subsequent state, with no new allocation or reservation.
  A row stranded in `allocating` by a lost `call.start` response is checked
  after two minutes: unresolved outcomes produce an explicit error log;
  only a matching gateway outcome can automatically release the hold.
  Gateway reporting and evidence lookup resolve known bindings; the operator
  path below requires the source to be `quarantined` or `ended`, not `allocating`.
- The gateway uses `gateway_request_id` to fence a single create attempt and
  record its durable outcome before upstream allocation. A duplicate dial
  may share in-flight completion or replay the original answer for a still-live
  binding; if there is no such answer it must not start a second upstream
  attempt for that ID. Resolve uncertainty by lookup; an ended dial is not
  resumable by replay.
- Replay an identical `call.established` ACK without binding or consuming again,
  even if the call subsequently ended; it does not reactivate the call.
  For `call.ended`, exact retry reports the current committed result and does
  not duplicate evidence or change `ended_at`. Retry with the same reason,
  `closed_at` and identity. Missing or unverified evidence can be supplemented
  with matching evidence, but never downgrade verified closure.
  Persist verified evidence and session/lease/pool transition together. Use
  `closed_at` capped at the first report's receive time for `ended_at`, or
  `observed_at` when lookup establishes closure before the report is received;
  later reports do not rewrite that timestamp.
  Clear only the matching pool source, never a replacement call's fence.

## Failure and recovery

`call.start` is synchronous: Zoe unavailability or refusal (including an invalid
or rotated-out key) fails the dial visibly without creating an upstream session.
Lifecycle reports use the [short in-memory retry](execution.md#failure-and-recovery).
Exhaustion and terminal failures are visible faults, not reasons to bypass the
binding or retry upstream creation.

Zoe never infers termination from a missing report, silence or a timeout. Zoe
reconciles bound active and unresolved calls through the required evidence
lookup, including when no `call.ended` arrived; reconcile promptly on a report
without evidence. `not_yet` does not end or quarantine a still-active call.
Absent or conflicting evidence never clears a hold or quarantine. Only matching
`never_established` or evidenced `released` outcomes can clear an unbound
failed create, fenced by session, generation, request and pool source. An old
failed attempt cannot clear a bound or replacement session. Log an explicit
error for a stuck `ending` or unknown session; no automatic compensation
beyond lookup.

When the pool remains quarantined and neither the gateway report nor Zoe's
evidence lookup resolves it, use the operator command `voice.pool.reconcile`.
It requires `voice:pool:reconcile` or `client:admin`, `session_id`,
`generation >= 1`, `confirmed_out_of_band_termination: true`, and a trimmed
12–500 character `reason`. Refuse with `409 voice_sessions_active` while any
session is `allocating`, `active` or `ending`; otherwise require a blocked pool
(`409 voice_pool_not_blocked`), its exact source session, and that session at
the supplied generation in `quarantined` or `ended`
(`409 voice_reconciliation_stale`). Atomically record an `operator_attestation`
in `voice_pool_reconciliations`, clear the pool source/hold and emit
`voice.pool.reconciled`. This does not close gateway media or rewrite session
state or termination evidence. A quarantined source remains unverified;
late end reports and evidence lookups are stale after its pool source is
cleared and cannot alter a replacement call's hold.

## Release

Ship Zoe, the gateway and Apple together in one release. Remove Zoe's
`POST /v1/voice/sessions`, `POST /v1/voice/sessions/{session_id}/connected`,
`/heartbeat` and `/hangup` siblings, the `voice.turn.commit` and
`voice.turn.handoff` commands, the Apple voice-turn relay path, heartbeat
reaper, handoff preflight wait and background classification scan. Keep the
`voice.turn.handoff` event. Deploy with a fresh Zoe
database; no migration from prior data or compatibility with older clients
or gateways is required.

## Implementation locations

[Call admission and binding](../../../internal/voice/calls.go), [termination and evidence reconciliation](../../../internal/voice/call_end.go).

## Verification obligations

Verify fail-closed and duplicate dials, binding before forwarding, and rejection
of stale reports. Verify all gateway end triggers, client silence hangup on
valid-activity-only timing, matched evidence and lookup on report loss, and no
reuse on unknown evidence.
Verify unbound failed-create reconciliation rejects old generations and pool
sources, and cannot clear a bound or replacement call. Verify real gateway
reconnection and task continuity. Follow the
[acceptance boundary](../../development.md#evidence-and-acceptance).
