# Voice media, leases, and activity contract

## Purpose and boundary

Approved target, 2026-11-16; not implementation, deployment, or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. Gateway media/account leases, client voice generations, durable tasks, and runtime sessions have separate identities/lifetimes. Ending media does not stop a task. Muting capture, stopping playback, ending voice, and stopping work are distinct operations. [Execution](execution.md) owns task handoff; [results](results.md) owns text delivery; [screen](screen.md) owns coordinated event recovery.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Client create attempt: session, generation, request identity, pool source | Server/gateway attempt and outcome lookup correlate to exact attempt; distinguish gateway media generation from client generation. |
| Gateway `never_established`/`released` outcome with evidence | May clear only matching **unbound failed create**, matching session, generation, request, and pool source. |
| Gateway release/termination evidence | Reconcile exact bound media/lease, retaining unknown outcomes as held. |
| Valid user/voice interaction | Activity tracker drives configurable silence timeout, default 60 seconds without valid interaction. |

## Required behavior

Use only AVAS gateway voice. Opening context is included in session creation, not a legacy post-create endpoint. Reject explicitly configured non-AVAS route at startup; do not silently reinterpret it. Historical WM data/lease evidence remain readable, but no WM create/resume/delta path remains. The external gateway independently uses AVAS; Zoe neither configures it nor infers route from successful media.

Selected gateway is the Zoe-compatible `ronigooja/chatgpt-web-voice` fork with gateway-relayed media/account pool. Gateway termination evidence attests local closed relay peer, not upstream account reclamation, and is available only for gateway-relayed sessions; client-direct media remains fail-closed. Retain valid new-evidence semantics, never weaken quarantine.

On macOS continuous voice is on by default after connection/identity initialization, subject independently to OS mic permission, audio-session policy, app lifecycle/background limits, network, pool capacity, and server/account policy. Truthfully report connecting, active, interrupted, reconnecting, unavailable, permission-denied, and stopped; never claim unavailable media active. Temporary failures may reconnect; permission/policy failures require Settings/system-settings recovery. Text remains usable without voice. This preserves the ambient-client baseline rather than the superseded Settings-opt-in interface.

Track valid interaction separately from transport health/events. Heartbeats, SSE, polling, retries, stale/duplicate events, and render chatter cannot indefinitely reset silence. Default 60-second timeout is configurable and counts only when no voice interaction is in progress. On timeout the client ends media; durable server context enables later continuation, not guaranteed live-audio transfer. Voice on/off and silence timeout remain AI-mutable within [settings rules](screen.md#tools-settings-presence-and-native-obligations).

On macOS, Settings selects voice from connected gateway's public catalog. Persist preference locally per server for new calls/reconnects; unavailable selection falls back to gateway default. Changes never interrupt active call. Raw audio is not retained by default. Both speakers' original full text persists independently of delegation/raw audio for [memory coverage](memory.md).

## Persistence and idempotency

Durably retain attempted creation, binding, attestation/evidence identity, and exact attempt correlation. Device/account/media leases retain holder, generation, expiry, termination evidence; gateway media generation is separate from client-facing generation. Persist matched outcome/evidence and idempotent fenced release transition. Account change creates new media while preserving task associations; old media must end before account reuse.

## Failure and recovery

Create timeout means unknown, not “no media exists”: retain held/quarantined resource. Never release a bound/replacement session using an old failed request. Missing/conflicting evidence remains unknown/held. Expiry/silence alone never proves release. Automatically check unknown outcomes without unsafe account reuse; only matching new evidence can clear matching unbound failed create or reconcile exact bound lease. User-facing recovery reflects actual state. Reconnect must preserve task continuity and cannot manufacture task authority/completion from transport continuity.

## Implementation locations

[Account media](../../../internal/voice/account_media.go), [gateway HTTP](../../../internal/voice/account_media_http.go), [service recovery](../../../internal/voice/service.go), [voice deployment](../../../deploy/voice/README.md), [Apple client](../../../clients/apple/Packages/ZoeKit/Sources/).

## Verification obligations

Verify matching failed-create `never_established`/`released` evidence, rejection of old generation/request/pool source, bound/replacement protection, automatic checks without premature release, actual gateway reconnection, and task continuity. Test valid-interaction-only silence60, truthful states, independent permission/policy/lifecycle constraints, preference selection/fallback, and no active-call interruption. The [real-device voice journey](../../development.md#product-verification-obligations) remains an obligation, not an accomplished result. Follow [evidence and acceptance](../../development.md#evidence-and-acceptance). Gateway health and local termination evidence do not establish upstream release, speech, or real end-to-end acceptance.
