# Voice media, leases, activity and recovery

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. The target retains existing valid new-evidence semantics rather than weakening quarantine. User-visible recovery reflects actual state; ending media does not stop a task.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Client create attempt: session, generation, request identity and pool source | Server/gateway attempt and outcome lookup remain correlated to the exact attempt; distinguish media generation from client generation. |
| Gateway `media.connected` | Record only; does not replace client `/connected`. |
| Gateway `media.ended`: reason and close time | Report only after both media legs have terminated, not on transient disconnect. The report can trigger release but is not termination evidence. |
| Gateway never_established or released outcome with evidence | Recovery may clear a matching **unbound failed create** only when the evidence matches session, generation, request and pool source. |
| Gateway media release/termination evidence | Server reconciles the exact bound media/lease; expiry or silence alone never proves release. |
| Valid user/voice interaction activity | Silence timer ends voice after default 60 seconds without valid interaction; retain configurable timeout. |

## Required behavior

Track actual interaction activity separately from transport health/events. Heartbeats, SSE, polling, retries, stale/duplicate events and render chatter do not indefinitely reset silence.

Bind `voice_sessions.gateway_session_id` and `gateway_media_generation` in one
atomic update. Lifecycle reports use the same [transcript channel and
authorization](execution.md#required-behavior), with `gateway_session_id` and
`media_generation` identifying the binding and the same
[10-second, nonpersistent retry](execution.md#failure-and-recovery) as
delegation. Zoe validates the binding and acknowledges unknown, stale or
replaced bindings without acting on them.

Client heartbeat, hangup, silence timeout and `/connected` remain in place.
For a matching binding, `media.ended` is only an early trigger for the existing
`active` to `ending` claim and release flow. Racing hangup and `media.ended`
use the same atomic claim so only one starts release. Transition to `ended`
still requires matched termination evidence; otherwise quarantine the lease.

## Persistence and idempotency

- Create: durable attempted creation, binding and attestation/evidence identity.
- Failed-create recovery: persist matched outcome/evidence and idempotent release transition.
- Release/termination: durable evidence and fenced lease transition.

## Failure and recovery

Timeout is unknown, not proof no media exists; retain held/quarantined resource. Never release a bound/replacement session using an old failed request; missing/conflicting evidence remains unknown/held. Unknown outcomes trigger automatic checks; retain held state while unresolved, without unsafe account reuse.

A stuck `ending` session is a fault: log an explicit error. Do not add
automatic recovery for this state.

## Implementation locations

[Account media](../../../internal/voice/account_media.go), [gateway HTTP](../../../internal/voice/account_media_http.go), [service recovery](../../../internal/voice/service.go).

## Verification obligations

Verify matching unbound failed-create never_established/released evidence, old-generation/pool-source rejection, bound-session protection, automatic unknown-state checks without premature release, real gateway reconnection and task continuity. Verify stale reports are ignored, racing release triggers share one claim, and `media.ended` cannot substitute for termination evidence. Verify valid-activity-only silence60. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
