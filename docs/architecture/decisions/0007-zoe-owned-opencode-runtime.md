# ADR 0007: Zoe-owned OpenCode runtime

Status: historical baseline; runtime and isolation are superseded by [ADR 0010](0010-host-opencode-runtime.md). Implementation and acceptance evidence are tracked in [the acceptance evidence register](../../acceptance/evidence.md), while delivery status remains in [the master plan](../../master-plan.md). This supersedes [ADR 0002](0002-ao-runtime-ownership.md).

## Context

The AO layer and Codex runtime ownership model add a second lifecycle authority and leave uncertain container effects difficult to prove. Zoe needs one runtime boundary with deterministic creation, fail-closed recovery, and termination evidence. The existing CLIProxyAPI deployment must remain an unchanged external provider.

## Decision

OpenCode is the only Zoe execution runtime. Codex is removed. A model selection is an OpenCode provider/model string. CLIProxyAPI is used only as an OpenAI-compatible OpenCode provider; its existing deployment, account selection, quota handling, and proxying remain unchanged.

The AO layer is removed. For each run, Zoe's runner creates exactly one container, with name and labels derived from `run_id`, attempt, and fence. The container runs exactly one OpenCode session. The runner authenticates `opencode serve` with a random password, grants only a scoped key, and provides no direct network access: provider traffic reaches the configured provider through a bridge or Unix socket. The runner remains the trusted host-side lifecycle authority and the control plane remains the business-state authority.

Creation is idempotent. Container names and labels are deterministic. If a create result is unknown, the runner resolves it by looking up the expected labels. If that lookup cannot establish the outcome, the run is `effect_unknown` and is not replayed automatically. The runner fails closed rather than creating a second container.

Stopping aborts the OpenCode session, force-removes the exact container ID, and verifies that the container is absent. Container removal is the termination proof; process exit, timeout, lease expiry, or an unverified API response is not.

Runtime contracts use neutral `Runtime*` names; AO-specific fields are compatibility data during migration only. Runtime proofs are limited to:

- `create-rejected`: a local deterministic failure before an external create effect;
- `no-effect`: no container with the expected run/attempt/fence labels exists;
- `terminated`: the exact container has been removed and its absence verified.

Forward migration maps `codex` and empty runtime values to `opencode`; unknown values are quarantined. Legacy `ao_*` columns remain until their data is backfilled. The four uncertain historical runs and retained legacy containers were abandoned by operator decision; no reconciliation gate remains for them. The [CY-05-* requirement rewrite](../../product.md) is follow-up work and is not part of this ADR change.

## Open verification

A probe of the pinned `opencode-ai@1.18.33` (2026-10-30, local fake OpenAI-compatible provider, no real credentials) gave a conditional go.

Confirmed:

- `opencode serve` supports Basic Auth (`OPENCODE_SERVER_PASSWORD`) and `--hostname`/`--port` bind control.
- `POST /session`, `POST /session/:id/prompt_async` (204), `GET /session/status`, `GET /session/:id/message`, `GET /event` (SSE), and `POST /session/:id/abort` exist.
- A custom `@ai-sdk/openai-compatible` provider with a `provider/model` string routes through a single TCP endpoint, so the existing Zoe bridge pattern fits.
- Session state is SQLite under the XDG data directory; a restarted server over the same directory lists and reads earlier sessions.

Rules derived from the probe:

- Session `title` is not unique; two creates with one title produce two sessions. The runner stores the run identity itself and uses container labels, not session titles, for idempotency.
- Completion is not `prompt_async` returning 204. The terminal state combines `/session/status` no longer `busy` with the latest assistant message (`time.completed`, `finish`, error parts); events only reduce polling.
- OpenCode has no per-run tool-step limit. The runner enforces tool-step, time, and budget limits externally through events and `abort`.

Still open:

- Final `finish` and token/cost propagation under a correct streaming provider and through the bridge.
- Recovery of in-flight work after a crash; only persisted sessions are confirmed after a clean restart.
- Existing budget gaps are unchanged.

## Consequences

Zoe owns runtime creation, session lifecycle, reconciliation identity, and stop evidence; one container and one OpenCode session define each run's runtime boundary. Deterministic labels make unknown creation outcomes queryable without granting replay authority. Container removal gives a concrete termination boundary, while unresolved identity or absence remains fail-closed.

The migration temporarily carries legacy AO schema and rollback state. Provider integration remains dependent on the unchanged CLIProxyAPI deployment. Streaming completion and metering, in-flight crash recovery, and existing budget gaps require acceptance evidence before they become delivery claims.

## Sources

[ADR 0002, superseded](0002-ao-runtime-ownership.md); [architecture runtime admission and model routing](../../architecture.md#runtime-admission-and-model-routing); [CLIProxyAPI deployment boundary](../../architecture.md#system-and-authority); [product requirements](../../product.md).
