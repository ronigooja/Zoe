# Data-flow overview

Status: approved target, 2026-11-16.
Approval does not establish source implementation, deployment, or acceptance.

## Scope

The component contracts define model inputs and outputs, producers and consumers,
persistence, recovery, and verification obligations for these flows. Code links
identify implementation locations, not conformance.

## Instance boundary

A Zoe instance is the product boundary. There is no product workspace in
the UI, wire contracts, persisted state, or runner authorization. Within the
instance, retain owner, device, task, and runner scope and assignment checks
where they apply. Event cursors are instance-local; command idempotency remains
owner-scoped. Apple filters local history and pending command replay by the
authenticated instance identity, not server URL alone. Key rotation does not
change instance identity; a fresh instance does not inherit the previous
instance's pending commands or history.

Memory uses immutable instance lineage plus owner to namespace external data.
Capability Docker resources are also namespaced by lineage. Fenced restore
retains lineage while changing the active instance identity and epoch.

Runner execution directories and their filesystem permissions are separate
from product identity and remain unchanged.

Removal requires a fresh Zoe instance, with no migration of old Zoe state and
no compatibility with older Zoe clients or runners. The gateway wire contract
and gateway database are unchanged.

## Main flow

```text
Apple dial -> gateway -> Zoe call authorization and opening -> gateway bridges Apple media to AVAS
AVAS client-targeted delegation -> gateway transcript ingest -> Zoe worker -> Register
  chat -----------> upstream conversation
  memory_write ---> memory worker
  recall ---------> scoped search -> unchanged text delivery
  task -----------> Steward -> task handling -> Worker when required
  task_ui --------> Steward -> task handling -> UI_Controller
  ui -------------> UI_Controller -> presentation -> render ACK
  chat_input -----> UI_Controller -> conversation-text input

Authoritative results -> voice delivery and requested UI
Bound screen input --> the identified task or pending question
Original dialogue and confirmed context events -> scheduled memory ingestion
```

The component contracts define route semantics, action semantics, identities,
persistence, and recovery.

## Read by task

| When changing... | Read... |
|---|---|
| Routing and complete contextual requests | [Register](flows/register.md) |
| Task selection, action fields, and stale decisions | [Steward](flows/steward.md) |
| Delegation ingest, Worker input, append, answer, and continuation | [Execution](flows/execution.md) |
| Authoritative results, voice chunks, receipts, and next-call context | [Results](flows/results.md) |
| UI_Controller, render ACKs, bound input, and event recovery | [Screen](flows/screen.md) |
| Memory writes, recall, ingestion, and compact/raw coverage | [Memory](flows/memory.md) |
| Gateway-owned dial and hangup, unresolved-create fencing, Zoe session binding, leases, and termination evidence | [Media](flows/media.md) |
| Client WebSocket endpoint, channels, heartbeat, frame limit, and reconnection | [Client WebSocket](flows/client-ws.md) |
| Worker calls to MCP servers running on the Mac client, per-MCP switches, and relay failures | [Client MCP](flows/client-mcp.md) |
| Independent model consumers, atomic configuration, and retained deployment state | [Configuration](flows/configuration.md) |
| Live-only model activity monitor in the Mac client | [Model monitor](flows/model-monitor.md) |

Use [development](../development.md) for documentation checks and the acceptance
boundary, and [operations](../operations.md) for configuration and deployment
entry points.
