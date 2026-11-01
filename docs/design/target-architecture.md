# Target architecture: guiding principles

Status: maintainer-endorsed direction, not an implementation or acceptance claim. Accepted decisions live in [ADR 0009](../architecture/decisions/0009-voice-steward-impact-policy.md); the implemented baseline lives in [architecture](../architecture.md). Open measurements stay in the [evidence register](../acceptance/evidence.md).

## Principles

1. **Speaking is instructing.** A spoken request is the instruction. Low-impact work needs no extra confirmation step.
2. **One authority.** The Zoe control plane alone owns commands, tasks, runs, memory, policy, and screen content. Nothing else creates business effects.
3. **Agents think; programs coordinate.** Thinking is done by OpenCode agents. Roles are profiles (system prompt, tools, model route, capabilities, child policy), not separate programs. A new role is a new profile.
4. **Voice is the front desk.** It listens, speaks, and hands the user's words to Zoe. It holds no authority and does not do the work.
5. **Context outlives the call.** Zoe assembles context for every call and writes summaries back to memory. Zoe memory is authoritative; the upstream thread is best effort.
6. **Replaceable edges.** Voice transport, runtime, model route, and client change behind stable interfaces.

## Roles

| Role | Does | Never |
|---|---|---|
| Voice (upstream) | Listen, speak, hand off | Hold authority; execute |
| Gateway | Call transport, account pool, media relay, normalized events to Zoe | Business logic |
| Zoe control plane | Authority, policy, admission, memory, screen documents, audit | Execute tasks |
| Steward (warm OpenCode agent) | Route each handoff: answer directly, add to a task, or start a task | Touch workspaces; heavy work |
| Worker (OpenCode agent) | Think and act inside a workspace; may use internal sub-agents | Grant itself authority |
| Super (OpenCode agent) | Plan and decompose; on demand | Run permanently |
| Runner | Create and supervise runtimes per admission | Decide what runs |
| Client | Render server-authored screens; capture audio | Fabricate content |

## Flow

```
user <-> voice --handoff--> gateway --> Zoe --> Steward --> answer | add to task | new task
                                                                         |
                                                      admission -> runner -> Worker
                                                                         |
                          authoritative task events --> Zoe --> context append --> voice speaks
```

Voice and Steward cannot create commands. The handoff reaches Zoe, which creates the command on an authenticated path bound to the call and turn identity. Spoken output never establishes completion; only authoritative task events do.

## Impact levels

Impact is set by server policy from a profile's declared capabilities and scopes, never by the model's own description.

- **Low impact, reversible** (research, writing inside the task workspace, summaries): executes on the spoken request. Voice restates what it understood; the screen shows status; the user can stop it.
- **High impact** (delete, external send, spend, deploy, permission change): Zoe issues a scoped confirmation challenge bound to task, run, action, expected version, and expiry. The user answers by voice or screen. Ambiguous words authorize nothing.

## Sub-agents

OpenCode internal sub-agents divide thought inside one workspace and are opaque to Zoe. Zoe child tasks are separate, stoppable, audited, count toward the run ceiling, and may use a different profile and isolation. Use child tasks when work needs different permissions, runs long, or must be visible.

## Extension points

New role: add a profile. New trigger: same admission path (program, then Worker, then Super). New voice transport: only the gateway changes. New client: consumes the screen document and event stream. Run ceiling: operator setting, default four.
