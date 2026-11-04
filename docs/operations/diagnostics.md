# Client diagnostics operations

This runbook applies when a native client reports a concise error or an
operator needs to investigate a connection, voice, task, transport, storage,
notification, export, capability, or infrastructure symptom. The client does
not display detailed exception, response, runtime, account, or transport
text. When the client is authenticated and transport is available, it queues a
redacted diagnostic and best-effort submits it to `POST /v1/diagnostics/client`.
The upload must not block interaction, change command authority, or change
receipt/retry semantics.

This is an operator investigation path, not a client-facing error-detail
channel. Follow the repository's [credential handling rule](../../AGENTS.md),
and do not ask a user to copy tokens, headers, response bodies, audio,
transcripts, SDP/ICE, or local exception text.

## What the endpoint actually accepts

Clients read policy from authenticated `GET /v1/client/policy`, which returns
the current `log_level`, `poll_interval_seconds`, and monotonic `revision` with
`Cache-Control: no-store`. Operators use `zoe client-policy show` or `zoe
client-policy set --level error|info|debug [--interval SECONDS]`. These CLI
operations pause the managed server, take the normal SQLite storage lock, read
or update the policy, and resume the server; `--offline` is available when the
operator has stopped it manually.

The endpoint is registered on the authenticated device route:

```text
POST /v1/diagnostics/client
```

The caller must be authenticated and have `client:read` or `client:command`.
The request body is JSON with an `events` array containing 1--50 events. Each
event has these persisted fields:

| Field | Meaning and operational use |
| --- | --- |
| `client_request_id` | Client-side correlation ID. It is required and indexed in SQLite. |
| `occurred_at` | Client event time; must be RFC3339. Compare with `received_at` for offline queueing or clock skew. |
| `app_version`, `build_version`, `source_revision`, `os_version` | Bounded client identity/version context. `build_version` is `CFBundleVersion`; CI sets `source_revision` to the 12-character GitHub commit prefix. Local builds use the stable `unknown` fallback. |
| `device_id_hash` | SHA-256 hex of the `X-Zoe-Installation-ID` value, or the authenticated principal device ID when that header is absent. The submitted value must match the server-computed hash. |
| `phase` | Bounded stage label, such as `config_request`. Use it as the primary symptom-stage filter. |
| `request_path` | Bounded request path, not a response body. |
| `http_status`, `response_bytes` | Status and response size only. |
| `error_type`, `error_summary` | Bounded, redacted classification and summary. |
| `level` | Optional `error`, `info`, or `debug`; omitted means `error`. More verbose events are accepted but dropped under the current policy. |
| `voice_session_id`, `voice_generation`, `voice_media_epoch`, `voice_peer_state`, `voice_data_channel_state` | Nullable bounded voice lifecycle context. These fields contain no media, SDP, tokens, transcripts, or account identifiers. |

The server rejects sensitive keys and sensitive markers, including
authorization/token/secret/API-key/header/body/SDP/ICE/audio/transcript or
credential fields. It also rejects an event larger than 16 KiB and a request
body larger than 1 MiB. A batch is accepted with HTTP `202` and returns
generated `diagnostic_ids` and a `dropped` count; malformed, unauthenticated, forbidden, sensitive,
oversized, or rate-limited submissions are not evidence that an event was
stored.

The per-device limit is at most 200 stored events in any rolling one-minute
window, counting the events in the incoming batch. The server deletes rows
older than 30 days when a diagnostic batch is accepted. This is short-lived
best-effort telemetry, not an immutable audit log or a complete client trace.

## Confirm that a client returned a diagnostic

1. Record the user's approximate time, timezone, app version, OS version, the
   concise client state, and (if the client exposes it through the supported
   support flow) the `client_request_id`. Do not request raw logs or secrets.
2. Confirm that the user was already authenticated when the error occurred.
   An unauthenticated client cannot use this route; a later upload may be
   delayed until authentication and transport return.
3. Check the server service state and the authenticated server path using the
   deployment procedure in [deployment operations](deployment.md). A health
   response proves process readiness only; it does not prove diagnostic
   acceptance or voice/task success.
4. Inspect the service log around the reported `received_at` time. A successful
   event writes a line shaped like:

   ```text
   client diagnostic id=<generated-id> device_hash=<sha256> phase=<phase> status=<0..599> error_type=<type>
   ```

   The log line does **not** include `client_request_id`, request bodies,
   credentials, or transcripts. Treat a matching hash/phase/time as
   corroboration, not as a unique match when several events are nearby.
5. Confirm storage with the read-only SQLite queries below. There is currently
   no supported diagnostic search CLI and no diagnostic read API. Do not invent
   one, and do not alter the database to manufacture an acceptance result.

If no row or log line is found, possible explanations include authentication
failure, unavailable transport, local queue loss, the client's best-effort
behavior, rate limiting, clock differences, a rejected sensitive/invalid
payload, service restart before receipt, or retention cleanup. Absence is not
proof that the client did not encounter the error.

## Read-only database investigation

Use the actual `ZOE_DATABASE_PATH` from deployment configuration. Read it on the
database-owning host under the service account or an approved read-only copy;
do not print environment files or secret references. Stop writes only through
the normal deployment maintenance procedure. Prefer a read-only SQLite
connection (for example, `sqlite3 'file:/path/to/zoe.db?mode=ro'`) and keep
query output in the approved operator workspace, not in tickets or chat.

Use narrow projections and a bounded time window:

```sql
SELECT id, workspace_id, subject_id, device_hash, client_request_id,
       occurred_at, received_at, app_version, os_version, phase,
       request_path, http_status, response_bytes, error_type, error_summary
FROM client_diagnostic_events
WHERE received_at >= '2026-10-29T00:00:00Z'
  AND received_at <  '2026-10-29T01:00:00Z'
ORDER BY received_at;
```

For a known client request ID:

```sql
SELECT id, workspace_id, subject_id, device_hash, client_request_id,
       occurred_at, received_at, app_version, os_version, phase,
       request_path, http_status, response_bytes, error_type, error_summary
FROM client_diagnostic_events
WHERE client_request_id = 'SUPPORT_PROVIDED_ID'
ORDER BY received_at;
```

For a known installation hash and phase, use the exact lowercase SHA-256 hex
value supplied by the authenticated support process; never hash or paste a
raw installation ID into an incident record:

```sql
SELECT id, client_request_id, occurred_at, received_at, phase,
       request_path, http_status, response_bytes, error_type, error_summary
FROM client_diagnostic_events
WHERE device_hash = 'SHA256_INSTALLATION_ID'
  AND phase = 'config_request'
  AND received_at >= '2026-10-29T00:00:00Z'
ORDER BY received_at;
```

The table has indexes on `(device_hash, received_at)` and
`client_request_id`. `received_at` is server time; `occurred_at` is client
time. Redact or hash identifiers again before sharing query output. Do not
select, dump, or attach unrelated tables, database files, WAL/SHM files,
environment files, instance keys, runner credentials, voice gateway keys,
provider keys, or user content.

## Interpret the event without overclaiming

- A row proves that the authenticated endpoint accepted and committed the
  bounded event. It does not prove that the original request was accepted,
  rejected for the reported reason, retried safely, or completed.
- A `request_path`, status, response size, phase, or error summary is client
  context, not a server trace. Correlate it with server logs and authoritative
  task/voice records where available.
- A successful `202` and returned `diagnostic_ids` prove endpoint acceptance
  only. The client upload is deliberately best effort and may be unavailable
  during the original failure.
- Diagnostics are independent of the business event stream and are not a
  replacement for receipts, task events, runner journals, or voice lifecycle
  evidence.
- Do not treat a missing row after 30 days as a negative result. Retention is
  enforced during accepted uploads, not documented as a continuously measured
  wall-clock deletion guarantee.

## Voice account-pool isolation checks

Use this section when a diagnostic phase points to voice configuration,
negotiation, reconnect, media closure, or an unavailable voice pool. The
current baseline uses client-direct WebRTC media and the selected
`chatgpt-web-voice` gateway's normal sticky account pool. The Zoe-side optional
account-media adapter is not part of the supported baseline and must remain
disabled unless a separately reviewed gateway release supplies the required
lease identity and exact-release contract.

First correlate the diagnostic's time, subject/workspace, and device context
with the server-owned session record. Use a narrow, read-only query and do not
select `offer_answer` or any media payload:

```sql
SELECT id, workspace_id, owner_id, device_id, request_id, generation,
       state, gateway_session_id, account_lease_id,
       account_lease_generation, gateway_media_generation, account_id_hash,
       gateway_unbound, gateway_unbind_unknown,
       created_at, updated_at, heartbeat_at, ended_at, last_error_code
FROM voice_sessions
WHERE workspace_id = 'WORKSPACE_ID'
  AND owner_id = 'SUBJECT_ID'
  AND updated_at >= '2026-10-29T00:00:00Z'
ORDER BY updated_at;
```

The gateway media generation is tracked separately from Zoe's session generation.
Termination evidence is a gateway-attested local observation of relay shutdown,
not proof that the upstream account is free, and is available only for
gateway-relayed media. Client-direct sessions have no such evidence and remain
fail-closed for account reuse.

Then inspect the single pool-state row:

```sql
SELECT blocked, reason_code, source_session_id, updated_at, updated_by
FROM voice_pool_state
WHERE id = 1;
```

Interpretation:

- `state` values include `allocating`, `active`, `ending`, `ended`, `failed`,
  and `quarantined`. An active/ending session or a blocked pool is a server
  lifecycle condition, not proof of a client bug.
- `reason_code` and `source_session_id` identify the server's pool fence.
  Never clear `blocked` by editing SQLite. Use the implemented authenticated
  voice lifecycle/reconciliation authority path; this document does not add a
  new command.
- `account_id_hash`, `account_lease_id`, and
  `account_lease_generation` are correlation fields. They do not reveal an
  account and do not, by themselves, prove that an upstream account was
  released or that media stopped.
- `gateway_unbind_unknown=1` or a `quarantined` session means termination is
  uncertain. Do not tell an operator to reuse the account or force pool
  unblocking based only on expiry or a client disconnect.

If and only if the separately enabled trusted account-media adapter is in use,
inspect its bounded termination evidence without exposing hashes as account
identities:

```sql
SELECT session_id, generation, account_id_hash, lease_id_hash,
       evidence_id, observed_at, adapter_kind
FROM voice_account_terminations
WHERE session_id = 'SESSION_ID'
ORDER BY observed_at;
```

The current client-direct baseline does not provide this trusted account-scoped
termination proof. Local fixtures or a gateway session-delete response are not
production evidence of safe account reuse. For pool isolation, retain only the
minimum session IDs, generations, reason codes, and evidence IDs needed to
correlate the incident; never copy gateway credentials, imported account data,
SDP/ICE, audio, or transcripts.

## User recovery decisions

Ask the user to reconnect only when the evidence indicates a temporary
transport, authentication-session, or service-availability condition and no
authoritative task submission is unknown. A reconnect is not a command retry.
Preserve the original `client_request_id` and receipt identity when asking for
support; do not ask the user to resubmit an unknown task merely because the
client lost its response.

Ask the user to check permissions or system settings when the client state
indicates microphone permission denial, audio-session policy, lifecycle or
background restriction, or another OS-controlled voice condition. These are
not ordinary reconnectable transport errors. Text remains the fallback when
voice is unavailable.

If the server shows an active, ending, quarantined, or pool-blocking voice
session, stop asking the user to repeatedly reconnect. Escalate through the
operator voice lifecycle/reconciliation path and keep the affected account or
session quarantined until authoritative termination evidence exists. If the
diagnostic only shows a client-side symptom and no server row, treat the cause
as unresolved rather than assigning blame to the account pool.

## Evidence boundary and escalation record

Record separately:

1. client evidence: concise displayed state, app/OS versions, event time,
   `client_request_id`, and accepted diagnostic row IDs;
2. server evidence: service status, bounded log lines, diagnostic table rows,
   and relevant authoritative task/voice state;
3. deployment evidence: release revision, configuration references (names and
   paths only), gateway revision, and the applicable service/recovery action;
4. acceptance limitations: whether this was source review, a local fixture,
   hosted CI, installation, or real-device/end-to-end evidence.

Do not claim that a diagnostic row proves successful voice, model execution,
task completion, account isolation, secure media termination, or physical data
erasure; consult the [acceptance evidence register](../acceptance/evidence.md)
when closing an incident.
