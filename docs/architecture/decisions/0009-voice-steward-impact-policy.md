# ADR 0009: Voice front desk, steward routing, and impact-level authorization

Status: accepted direction; implementation and acceptance remain open. Supersedes the explicit-submission sentences in [ADR 0005](0005-voice-task-and-lease-boundaries.md). Principles: [target architecture](../../design/target-architecture.md). Delivery status: [master plan](../../master-plan.md); evidence: [evidence register](../../acceptance/evidence.md).

## Context

Requiring explicit transcript submission for every voice request made speaking a request insufficient and left voice with no path to the task authority. Voice also cannot do the work: it has no verified tools. Thinking belongs to agents.

## Decision

1. Voice is the front desk: it listens, speaks, and hands the user's words to the control plane. It holds no authority and performs no work.
2. A steward (a warm OpenCode agent profile) routes each handoff: answer directly, add to an active task, or start a task. Steward, Worker, and Super are profiles of one runtime, not separate programs.
3. A spoken request is the instruction. The control plane creates the command on an authenticated path bound to the call and turn identity, with stable turn identity, revisions, and deduplication unchanged. Voice and the steward never create commands directly.
4. Authorization follows server-set impact levels derived from the profile's declared capabilities and scopes, never from the model's description. Low-impact, reversible work executes on the spoken request. High-impact or irreversible work requires a scoped confirmation challenge bound to task, run, action, expected version, and expiry, answered by voice or screen and recorded by the server. Ambiguous words authorize nothing.
5. Spoken output and transport continuity never establish completion or approval; only authoritative task events do.
6. The active-run ceiling is an operator setting, default four.

## Consequences

The "transcript review/submission remains explicit" and "voice never silently submits" rules no longer apply to low-impact work; the high-impact floor remains server-validated. Voice transport details (call creation route, sideband, handoff events) are gateway implementation and stay outside this decision. Open measurements: steward latency and cost against the ceiling, metering of OpenCode internal sub-agents, handoff event behavior on the live route, and transcript availability.
