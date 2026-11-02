# Target architecture: guiding principles

Status: maintainer-endorsed direction, not an implementation or acceptance claim. Accepted decisions: [ADR 0009](../architecture/decisions/0009-voice-steward-impact-policy.md). Implemented baseline: [architecture](../architecture.md). Evidence and open measurements: [evidence register](../acceptance/evidence.md).

## Principles

1. **Speaking is instructing.** A spoken request is the instruction. Low-impact work needs no extra confirmation step.
2. **One authority.** The Zoe control plane alone owns commands, tasks, runs, memory, policy, and screen content. Nothing else creates business effects.
3. **Agents think; programs coordinate.** Thinking is done by OpenCode agents. Roles are profiles (system prompt, tools, model route, capabilities, child policy), not separate programs. A new role is a new profile.
4. **Voice is the front desk.** It listens, speaks, and hands the user's words to Zoe. It holds no authority and does not do the work.
5. **Context outlives the call.** Every call starts with context assembled by Zoe; every call ends by writing back to memory. A new call never starts from nothing.
6. **Replaceable edges.** Voice transport, runtime, model route, and client change behind stable interfaces.

## Roles

| Role | Does | Never |
|---|---|---|
| Voice (upstream) | Listen, speak, hand off | Hold authority; execute; confirm high-impact work |
| Gateway | Call transport, account pool, media relay, normalized events to Zoe | Business logic |
| Zoe control plane | Commands, tasks, runs, receipts, events, memory versions, policy, screen documents, audit | Execute tasks |
| Steward (warm OpenCode agent) | Short system prompt, small context. Reads each handoff, consults memory and active tasks, then answers directly, adds to a task, or starts a task | Touch workspaces; heavy work |
| Worker (OpenCode agent) | Think and act inside a workspace: research, edit, write files; may use internal sub-agents | Grant itself authority |
| Super (OpenCode agent) | Plan and decompose complex goals; on demand | Run permanently |
| Runner | Create and supervise runtimes per admission; report evidence | Decide what runs |
| Memory (Mem0, Qdrant) | Derived retrieval over the Zoe memory authority | Be a source of truth |
| Client | Render server-authored screens; capture audio | Fabricate content; decide outcomes |

## Flow

```
user <-> voice --handoff--> gateway --> Zoe --> Steward --> answer directly | add to task | new task
                                                                                  |
                                                       admission (budget, ceiling, authority)
                                                                                  |
                                                                        runner -> Worker
                                                                                  |
                              authoritative task events --> Zoe --> context append --> voice speaks
```

- Voice and Steward cannot create commands. The handoff reaches Zoe, which creates the command on an authenticated path bound to the call and turn identity.
- Small talk is answered directly. Work that needs tools or has side effects becomes a task.
- Every run, whoever asked for it, passes the same admission path.
- Spoken output and the Steward's reply never establish completion; only authoritative task events do.
- Result delivery: with a live call, Zoe appends a short summary for voice to speak; with none, the result becomes a notification. Full results stay on the task.

## Authority floors

These hold regardless of role or route: the client never fabricates content; the runner and agents cannot grant their own authority; high-impact actions need server-validated scoped confirmation; the menu-bar item and microphone-use indication always remain.

## Impact levels

Impact is set by server policy from a profile's declared capabilities and scopes, never by the model's own description.

| Level | Examples | Handling |
|---|---|---|
| Low impact, reversible | Research, writing inside the task workspace, summaries | Executes on the spoken request. Voice restates what it understood; the screen shows status; the user can stop it |
| High impact | Delete, external send, spend, deploy, permission change | Scoped confirmation challenge bound to task, run, action, expected version, and expiry. Answered by voice or screen, recorded by the server. Ambiguous words authorize nothing |

## Context and memory

- Opening context is assembled by Zoe per call and given once when the call is created: role rules, recent-conversation summary, active tasks, relevant memory (scoped retrieval, permission and version recheck). Mid-call additions are appends only.
- Transcripts are captured by the gateway and stored by Zoe against stable turn ids.
- After a call, a summary is written through the existing path: authority, transactional outbox, extraction and indexing, scoped retrieval. Explicit user decisions register through policy; model inferences stay candidates.
- Zoe memory is authoritative. The upstream conversation thread is best effort and may not survive an account change.

## Sub-agents

| | OpenCode internal sub-agents | Zoe child tasks |
|---|---|---|
| Visible to Zoe | No | Yes: separate task, stoppable, audited |
| Isolation | Same container and permissions | Own profile and isolation |
| Counts toward ceiling | With the parent run | Each counts |
| Use | Division of thought inside one workspace | Different permissions, long-running, user-visible work |

## Extension points

- New role: add a profile.
- New trigger (schedule, event): same admission path; program first, Worker for semantic judgment, Super for complex coordination.
- New voice transport: only the gateway changes; Zoe sees normalized events and a small API.
- New client: consumes the screen document and event stream.
- Run ceiling: operator setting, default in [ADR 0011](../architecture/decisions/0011-profiles-permissions-and-context-hygiene.md).
