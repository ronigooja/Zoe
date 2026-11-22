# Data-flow overview

Status: approved target, 2026-11-16.
Approval does not establish source implementation, deployment, or acceptance.

## Scope

The component contracts define model inputs and outputs, producers and consumers,
persistence, recovery, and verification obligations for these flows. Code links
identify implementation locations, not conformance.

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
| Gateway-owned dial and hangup, Zoe session binding, leases, and termination evidence | [Media](flows/media.md) |
| Independent model consumers, atomic configuration, and retained deployment state | [Configuration](flows/configuration.md) |

Use [development](../development.md) for documentation checks and the acceptance
boundary, and [operations](../operations.md) for configuration and deployment
entry points.
