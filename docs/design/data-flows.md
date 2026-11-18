# Design overview

Status: approved target, reorganized from the 2026-11-16 data-flow contract.
Design approval does not establish source conformance, deployment, or acceptance.

## Authority and scope

This overview defines shared system boundaries and routes readers to the
authoritative component contracts. Each contract owns its detailed behavior,
schemas, persistence, recovery, and verification obligations.

Zoe is a private, self-hosted first mate for work that persists beyond a
conversation. The current platform boundary is a Linux backend and execution
host with native macOS, iPhone, and iPad clients. The deployment baseline is
single-instance, single-owner, and multi-project; ownership boundaries remain
explicit for future evolution. The macOS interface is an ambient screen.
iPhone and iPad do not yet have an approved interface design.

Read the relevant contract before implementing or reviewing behavior. Code
links locate implementation; they do not certify conformance. Report unresolved
conflicts rather than treating old documentation or existing code as an override.

## System responsibilities

| Component | Responsibility |
|---|---|
| Control plane | Durable business records, authorization, commands, tasks, decisions, events, and validated observations |
| Register | Resolve conversational context into a complete request and select its routes |
| Steward | Select task handling from the request and authoritative candidate facts |
| Runner and OpenCode Worker | Execute admitted work, preserve execution identity, and report outcomes |
| Voice gateway | Media transport, normalized voice events, account resources, and spoken delivery |
| Memory worker | Scoped personal-memory search, writes, and scheduled ingestion |
| UI_Controller and native clients | Generate presentations from requests and authoritative facts; render and return bound input |
| Background services | Run authorized long-lived capabilities independently of clients and conversations |

## Main flow

```text
Original dialogue -> Register
  chat -----------> upstream conversation
  memory_write ---> memory worker
  recall ---------> memory worker -> result transport
  task -----------> Steward -> control plane -> runner / Worker
  ui -------------> UI_Controller

Authoritative task results -> voice delivery and requested presentations
Bound screen input --------> the identified task or pending question
Original dialogue and completed-task records -> scheduled memory ingestion
```

Task and UI routes may coexist. The contracts define route exclusivity,
action selection, delivery identities, and recovery behavior.

## Read by task

| When changing... | Read... |
|---|---|
| Routing, contextual requests, or model output parsing | [Register](flows/register.md) |
| Task selection, clarification, questions, or task actions | [Steward](flows/steward.md) |
| Handoff, admission, Worker inputs, continuation, or execution recovery | [Execution](flows/execution.md) |
| Authoritative results, voice chunks, receipts, or next-call availability | [Results](flows/results.md) |
| UI generation, A2UI, render ACKs, bound input, or client event recovery | [Screen](flows/screen.md) |
| Memory writes, recall, ingestion, or compact/raw context coverage | [Memory](flows/memory.md) |
| Media sessions, leases, quarantine, or resource release | [Media](flows/media.md) |
| Model selection, consumer separation, or atomic configuration changes | [Configuration](flows/configuration.md) |

Use [development guidance](../development.md) for checks and document maintenance.
Use [operations](../operations.md) for installation, credentials, deployment,
diagnostics, backup, restore, and deletion procedures.

## Shared invariants

- Business-state changes belong to the control plane. Model output, memory,
  client state, and transport activity cannot independently establish authority.
- Derived requests preserve the user's intent and authority. Original dialogue
  remains provenance and ingestion input, not a duplicate execution instruction.
- Preserve stable identities, scope, versions, and recovery checkpoints across
  retries and reconnects. Unknown acceptance or external effects require
  reconciliation, not blind replay.
- Task execution, media lifetime, presentation, and their acknowledgements have
  distinct meanings. Ending voice does not stop work; delivery does not prove
  execution success or user-visible completion.
- Voice and screen consume authoritative results. Neither summaries nor
  presentations replace the original result or invent business outcomes.
- Use programs for deterministic coordination. Use models for semantic work
  without adding blanket delegation prohibitions or new confirmation gates.
- Configuration and deployment changes preserve keys, data, and working
  connectivity unless an explicitly authorized operation changes them.
