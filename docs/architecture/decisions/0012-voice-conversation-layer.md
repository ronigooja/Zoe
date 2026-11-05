# ADR 0012: Zoe-owned conversation layer for real-time voice

Status: proposed direction; nothing here is implemented or accepted. Extends [ADR 0009](0009-voice-steward-impact-policy.md). Evidence: [evidence register](../../acceptance/evidence.md#live-voice-call-avas-route--2026-11-05).

## Context

Observed on the AVAS route (one call each, single account):

- Today every committed voice turn creates a `zoe-steward` task. A task answered about 2 minutes later; during that time the upstream model could only improvise (a filler such as "soon").
- The upstream model rewrote a long Steward answer and contradicted it (rain probability).
- Memory is loaded once at call start as opening context ([ADR 0006](0006-memory-authority-and-derived-stores.md)). There is no memory access during a call.
- Probe on 2026-11-06: after `delegation.created`, a `delegation.context.append` on channel `speakable` was confirmed by upstream in 139 ms and spoken verbatim, preceded by an upstream-added lead-in sentence. Speech of about 30 characters took about 6 s to play; this is playback time, not waiting.

The problem is control, not speed: the upstream model leads the conversation and Zoe is an asynchronous side effect.

## Decision

1. Zoe owns what is said. The upstream model listens and speaks; it does not decide content that Zoe can answer.
2. Every client-target delegation is routed by the Zoe control plane into one of two paths:
   - **Fast path**: answered within a latency budget from local authoritative data only (memory, task state, simple facts). The answer is returned as a short `speakable` context append.
   - **Slow path**: starts or appends to a `zoe-steward` task as in [ADR 0009](0009-voice-steward-impact-policy.md). The immediate `speakable` reply states that work has started.
3. Progress is a first-class input. A question about a running task (for example "is it done yet") is a fast-path question answered from the task record, never from model guesswork.
4. Results are written for speech: short sentences, key figures first, one `speakable` append per chunk. Steward output that is long is summarized by Zoe, not by the upstream model.
5. Delivery channel: when a call is live, results use the gateway's sideband or `/context` channel; otherwise a notification. Spoken delivery never marks a task complete; only authoritative task events do ([ADR 0009](0009-voice-steward-impact-policy.md) item 5).
6. Authority is unchanged: voice and the conversation layer create no commands directly. Impact-level rules of [ADR 0009](0009-voice-steward-impact-policy.md) apply to the slow path only; the fast path is read-only.
7. The gateway stays transport-only (media, accounts, sideband). It carries no conversation logic.

The stateless `zoe-triage` classifier is a configurable, startup-loaded client only; it is not wired into voice behavior. When enabled, it classifies one sentence through the configured Responses endpoint and callers retain the slow-path fallback on any failure.

## Open questions (to measure before implementation)

- Fast-path time to first audio end to end, not only upstream acknowledgement.
- Whether long `speakable` content is summarized or altered by upstream; whether the lead-in sentence can be suppressed.
- Behavior with several delegations in one call and with barge-in.
- Routing rule: rules in Zoe, a model call, or both; cost and failure behavior of each.
- Which memory reads are allowed on the fast path and their size bound.

## Consequences

- Replaces "create a task for every turn" with routing; task creation becomes a decision.
- Adds a read-only fast-path service in the control plane with a latency budget and a fallback to the slow path on timeout.
- The AVAS route depends on a reverse-engineered protocol with no stability contract; each upstream change must be re-measured. If upstream cannot be made to speak Zoe's text verbatim, the fallback is Zoe-owned speech recognition and synthesis, which is a separate decision.
