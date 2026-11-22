# Voice media, leases, activity and recovery

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. The target retains existing valid new-evidence semantics rather than weakening quarantine. User-visible recovery reflects actual state; ending media does not stop a task.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Apple dial: SDP offer, settings and `request_id` | Client sends one request to the gateway. |
| Gateway `call.start` | Zoe enforces one active call per owner, including unresolved attempts. Zoe creates an `allocating` `voice_sessions` row and returns the Zoe session ID, [opening items](results.md#required-behavior) and instructions. |
| Gateway `call.established`: Zoe session reference, `gateway_session_id`, `media_generation`, `account_lease_id` and `account_lease_generation` | Zoe commits the binding and transitions `allocating` to `active` in one atomic update. |
| Gateway `call.ended`: `gateway_session_id`, `media_generation`, reason and termination evidence when available | Zoe reconciles the exact session and lease. The report alone is not evidence. |

## Required behavior

### AVAS dial authentication

The client stores only Zoe's address and instance key. It gets the gateway's
HTTPS dial origin from Zoe's [authenticated voice configuration](configuration.md#voice-configuration),
not from the installation link. The client validates the gateway's TLS
certificate and hostname using platform trust. Deployment must make the
gateway's HTTPS dial endpoint reachable from the client's network.

The client sends the same instance key as a bearer over HTTPS to Zoe and to the
gateway. The gateway does not verify, hash or retain the key; it forwards the
presented key in memory only in `call.start` over HTTPS, alongside its own runner
credential. Zoe checks both identities: the runner identifies the gateway and
must be authorized for `call.start` under the same workspace-scoped runner
principal model as [transcript ingest](execution.md#required-behavior). Zoe
validates the client's key using the [existing instance-key check and rotation](../../../internal/control/auth.go)
(constant-time SHA-256 digest comparison and credential generation), and
establishes the workspace, owner and device from the validated key. Rotation
takes effect immediately without synchronization to the gateway.

Neither service logs, persists or echoes the presented instance key, including
in errors or telemetry. The gateway rejects a dial without a key locally.

### Call lifecycle

Zoe uses its existing gateway bearer when calling the gateway.

The gateway owns the AVAS call. It must receive a successful `call.start`
response before allocating relay ports, selecting an upstream account, or
creating the upstream session or client relay. After negotiation the gateway
sends `call.established` and must receive Zoe's
acknowledgement of the committed binding before returning the answer SDP or
forwarding transcript, delegation or end reports. The gateway returns the
answer SDP only in the authenticated HTTPS dial response. The media connection
is the one negotiated by that offer and answer, including the SDP's ICE
credentials and DTLS fingerprints. `call.established` targets the row created
at `call.start`; later ingest sees the complete binding. Lifecycle reports use
the gateway's runner identity, workspace authorization and
[transcript channel](execution.md#required-behavior).

Zoe retains the owner, device, Zoe session ID/generation, `gateway_session_id`,
`gateway_media_generation` and opening manifest/reservations. Results, fast
replies, text input and UI feedback sent upstream still require an active call
and are fenced by both session identities and generations.

The client signals hangup to the gateway. Hangup, silence timeout, upstream
`voice.end` or media loss ends the call at the gateway, which closes both legs
and reports `call.ended` with the result of close. The gateway observes liveness
from the media connection. Its configurable silence timer (default 60 seconds)
tracks valid interaction separately from transport health; polling, retries,
stale or duplicate events and render chatter do not count as activity. The
client does not send session create, connected, heartbeat or hangup to Zoe.
Zoe does not reap calls by heartbeat or initiate gateway create or release.

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

- Zoe deduplicates `call.start` by (owner, `request_id`) and rejects reuse with
  different parameters. A replay must not allocate another session or reserve
  opening items again. The gateway records its upstream create outcome against
  the exact attempt and resolves uncertainty by lookup, not by blindly retrying
  creation.
- Persist lifecycle transitions idempotently against the exact session and
  generation. Zoe transitions to `ended` only with durable gateway termination
  evidence matching the bound session, media generation and lease ID/generation.
  Evidence requires clean closure of both peers, data channels, tunnels and any
  retired legs, and includes a durable evidence ID and valid observation time.
  It attests gateway-local closure, not upstream account reclamation. Persist
  the evidence and corresponding lease transition together.

## Failure and recovery

`call.start` is synchronous: Zoe unavailability or refusal (including an invalid
or rotated-out key) fails the dial visibly without creating an upstream session.
Lifecycle reports use the [short in-memory retry](execution.md#failure-and-recovery), with no persistent
outbox. Exhaustion and terminal failures (including 404/409 on ingest) are
visible faults, not reasons to bypass the binding or retry creation.

Zoe never infers termination from a missing report, silence or a timeout. For a
lost `call.ended` or unknown evidence, Zoe actively queries the gateway, which
must provide an authoritative, read-only outcome/evidence lookup scoped to the
exact session, media generation and lease. Missing or conflicting evidence
leaves the session held or quarantined. Only matching `never_established` or
evidenced `released` outcomes can clear an unbound failed create, fenced by
session, generation, request and pool source. An old failed attempt cannot
clear a bound or replacement session. Log an explicit error for a stuck
`ending` or unknown session; no automatic compensation beyond lookup.

Rollout: ship Zoe's `call.start` and lifecycle handling plus transactional
delegation ingest and worker first. Ship gateway-owned dialing, lifecycle
reporting and direct delegation next, without client injection. Ship the new
Apple client without the relay path last, then remove the old Zoe session
create/connected/heartbeat/hangup endpoints and `voice.turn.commit` and
`voice.turn.handoff` commands. Installed client `c6fc5b5` is unsupported after
the change; there is no dial compatibility path.

## Implementation locations

[Account media](../../../internal/voice/account_media.go), [gateway HTTP](../../../internal/voice/account_media_http.go), [service recovery](../../../internal/voice/service.go).

## Verification obligations

Verify fail-closed and duplicate dials, binding before forwarding, and rejection
of stale reports. Verify all gateway end triggers, valid-activity-only silence,
matched evidence and lookup on report loss, and no reuse on unknown evidence.
Verify unbound failed-create reconciliation rejects old generations and pool
sources, and cannot clear a bound or replacement call. Verify real gateway
reconnection and task continuity. Follow the
[acceptance boundary](../../development.md#evidence-and-acceptance).
