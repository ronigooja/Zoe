# ADR 0011: Profiles, per-run permissions, waiting runs, and bounded context

Status: accepted direction; implementation and acceptance remain open. Extends [ADR 0009](0009-voice-steward-impact-policy.md) and [ADR 0010](0010-host-opencode-runtime.md). Principles: [target architecture](../../design/target-architecture.md). Evidence: [evidence register](../../acceptance/evidence.md).

## Context

Runs use one wildcard allow rule, so impact levels cannot follow capabilities. A Worker question and a permission request have no path to the user. Neither voice calls nor agent sessions bound their context, and long contexts degrade model quality. The memory authority exists, but nothing assembles context from it for a call or a run, and nothing writes call or task outcomes back.

## Decision

### Profiles and permissions

1. Steward, Worker, and Super are named OpenCode agents (system prompt, model) in the generated OpenCode config. A run selects one at session creation. Super is a profile slot only; orchestration is out of scope.
2. The server computes one effective permission set per run at admission from the profile defaults and the task scope, and carries it in the claim. The runner only translates it into OpenCode session `permissions` (`{action, resource, effect}`, last match wins) and never widens it. Rules are written deny-all first, then explicit allows, so no request reaches `ask`.
3. Workspace-external access is allowed but is high impact. Widening beyond the profile default needs a scoped confirmation.
4. The Steward chooses among server-defined tiers (read-only research, workspace read-write, external paths). It never writes individual rules. Individual rules come only from server policy or a recorded confirmation.
5. OpenCode permissions are a policy layer, not isolation. The enforced boundary stays the runner's host user and unit sandbox. Child sessions inherit the parent's session permissions.
6. The active-run ceiling is an operator setting (`ZOE_MAX_RUNS`), default 12.

### Asking the user

7. A permission `ask` that still occurs is detected by the runner, reported as a pending action, and answered only after the server validates a scoped confirmation or applies policy. Expiry or no answer means reject.
8. A Worker question ends the turn and puts the task in `awaiting_user` with the run kept. The answer is prompted into the same OpenCode session. After the wait limit (default 30 minutes, operator setting) the run is terminated and its slot released; the answer then starts a new run that continues from task messages and workspace files.

### Bounded context

9. Every long-lived conversation has a context budget and is replaced by a fresh one seeded from a Zoe summary when it is exceeded. Agent sessions are measured by the sum of the last assistant message's token usage reported by OpenCode. Voice calls are measured by transcript characters, because the upstream reports no tokens. Thresholds are operator settings. The summary is written through the memory path before the replacement starts.

### Memory and voice context

10. Zoe assembles opening context from the memory authority (role rules, recent summary, active tasks, scoped memory with recheck) and gives it to a call once at creation, to a Steward per handoff, and to a Worker in its initial prompt. Nothing but Zoe's authority writes claims; call summaries and task outcomes enter as sources, explicit user decisions as `user_confirmed`, model inferences as proposals.
11. The gateway reports on session creation whether the upstream conversation was resumed or is new. Resumed: Zoe sends only changes since the last context version sent to that conversation. New: Zoe sends the full opening context. Zoe records the last version sent in the call context manifest.

## Consequences

`allow *` and the fixed ceiling are removed. The runner gains permission-request and waiting-run states. Capacity accounting must count waiting runs that keep their session. Memory retrieval is keyword-scored today; semantic retrieval and time filters are separate work and gate how useful recall is. Open measurements: memory pressure at the configured ceiling, token thresholds, resume behavior on the live route.
