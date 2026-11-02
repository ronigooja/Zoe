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
5. OpenCode permissions are the only enforcement layer. By operator decision there is no OS sandbox: `zoe-opencode` runs as root without unit hardening, so a run that is allowed `shell` can act anywhere on the host, including reading `/etc/zoe`. Tool-level rules (read-only tier without `edit` and `shell`, unlisted tools denied, `external_directory` for file tools, `ask` for confirmation) still hold; path and impact limits do not hold for `shell`. Measured on the live host: a `workspace_read_write` run wrote outside its workspace through `shell`. Child sessions inherit the parent's session permissions.
6. The active-run ceiling is an operator setting (`ZOE_MAX_RUNS`), default 12.
7. Web research uses `webfetch` and the explicitly scoped Scrapling MCP server; `websearch` is not enabled because this deployment has no default search provider.

### Asking the user

8. A permission `ask` that still occurs is detected by the runner, reported as a pending action, and answered only after the server validates a scoped confirmation or applies policy. Expiry or no answer means reject.
8. A Worker question ends the turn and puts the task in `awaiting_user` with the run kept. The answer is prompted into the same OpenCode session. After the wait limit (default 30 minutes, operator setting) the run is terminated and its slot released; the answer then starts a new run that continues from task messages and workspace files.

### Bounded context

9. Every long-lived conversation is rotated when the latest assistant usage exceeds 50% of the selected model's `/api/model` `limit.context`; `ZOE_AGENT_CONTEXT_WINDOW_RATIO` defaults to `0.5` and must be in `(0,1)`. If the model window cannot be determined, rotation is disabled for that run. `ZOE_AGENT_CONTEXT_TOKEN_BUDGET`, when set, is an optional absolute cap and can only lower the threshold; a claim `MaxContextTokens` can lower it further. If a fresh session itself starts above the threshold, it rotates again until `ZOE_AGENT_MAX_ROTATIONS`, then ends with `context_rotation_limit_exceeded`.

### Memory and voice context

10. Zoe assembles opening context from the memory authority (role rules, recent summary, active tasks, scoped memory with recheck) and gives it to a call once at creation, to a Steward per handoff, and to a Worker in its initial prompt. Nothing but Zoe's authority writes claims; call summaries and task outcomes enter as sources, explicit user decisions as `user_confirmed`, model inferences as proposals.
12. A Worker goal is only a relevance hint: when it has no keyword match, Zoe retries scoped-memory retrieval without keywords, then applies the same scope, task-access, source, and budget checks.
11. The gateway reports on session creation whether the upstream conversation was resumed or is new. Resumed: Zoe sends only changes since the last context version sent to that conversation. New: Zoe sends the full opening context. Zoe records the last version sent in the call context manifest.

## Consequences

High-impact confirmation covers only actions OpenCode itself asks about; a `shell` command is not classified, and `workspace_read_write` and `external_paths` differ only for file tools. `allow *` and the fixed ceiling are removed. The runner gains permission-request and waiting-run states. Capacity accounting must count waiting runs that keep their session. Memory retrieval is keyword-scored today; semantic retrieval and time filters are separate work and gate how useful recall is. Open measurements: memory pressure at the configured ceiling, token thresholds, resume behavior on the live route.
