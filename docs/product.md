# Zoe product requirements

This is the current product requirements home. Requirements express required behavior, not implementation or acceptance claims. [The master plan](master-plan.md) owns delivery status and acceptance boundaries; [architecture](architecture.md) owns responsibilities and [decision records](architecture.md#decision-records).

The complete scope retains all 22 chapters, all 75 stable IDs, scenarios A–E, and all fifteen quality dimensions. This document carries forward the valid details of the [original complete PRD](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/archive/research/product-requirements.md) and its [stable-ID mapping](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/archive/research/reports/01-requirements.md). Those immutable sources are provenance, not competing current plans. Current Linux/native-client, Zoe-owned OpenCode runtime, instance-key, and assistant-UI decisions take precedence over superseded proposals.

## 1. Product Positioning

Zoe is a private, self-hosted system that keeps authorized work, projects, memory, and services alive beyond a conversation or client session. The full product scope comprises all 22 chapters; sequencing delivery does not remove an in-scope requirement.

### Name and role

Zoe takes its name from Zoë Washburne, Captain Malcolm Reynolds's second-in-command in *Firefly*. The product expresses the role of a dependable first mate: the user sets direction, and Zoe helps carry the work forward with continuity, judgment, and reliable execution.

When describing Zoe's role in product copy or interaction design, use this first-mate relationship: understand the user's intent, act within granted authority, coordinate ongoing work, report actual outcomes, and bring decisions back to the user when their judgment or authorization is needed.

<a id="cy-01-01"></a>

**Disposition: Required.**

### 1.1 What Zoe Is

**CY-01-01.** Long-running cross-device system with durable memory, autonomous execution, coordination, and extensible capabilities.

It interacts with users through natural language and speech, understands requests, performs tasks, manages projects, coordinates different AI agents, and, when necessary, creates and deploys ordinary programs so the system gains new capabilities that can run over time.

Zoe is not a chat tool that works only when the user issues a command.

It has a background runtime independent of its clients and can continuously maintain tasks, memory, services, and runtime state.

Authorized background work can continue even when the user does not keep Zoe open.

<a id="cy-01-02"></a>

**Disposition: Required.**

### 1.2 Core Design Principles

**CY-01-02.** Apply the ten principles: deterministic work outside models, independent workers, replaceable modules, efficiency, reliability, and future ownership boundaries.

Zoe is not intended to make AI responsible for everything. Instead, different kinds of execution units should take on the work best suited to them.

The system follows these principles:

1. **AI provides intelligence; ordinary programs handle deterministic, continuous execution.**
2. **Super handles complex coordination and does not intervene in every simple operation.**
3. **Workers can complete specialist tasks independently; not everything must pass through Super.**
4. **The system can create, deploy, and manage new long-running capabilities.**
5. **Tasks, memories, and execution state must be stored long term and must not depend on a single conversation.**
6. **Users can continue the same work from different devices.**
7. **Zoe should not lock users into its own client.**
8. **AI models, agents, voice services, and other components should be independently replaceable.**
9. **Accuracy, reliability, and efficient token use take priority over unnecessary AI calls.**
10. **The product is not limited to personal use; its design leaves room to expand to teams and other scenarios.**

<a id="cy-01-03"></a>

**Disposition: Required.**

### 1.3 Product Boundaries

**CY-01-03.** Use the host OS and existing specialist tools; Zoe does not recreate an OS, IDE, or terminal desktop.

Zoe does not need to replace macOS, Linux, or Windows.

It runs on an existing operating system and uses the operating system's computing, file, network, and process-management capabilities.

Zoe also does not need to recreate a full code editor, terminal, or operating-system desktop.

For existing specialist software such as Codex, Zoe should make direct use of native capabilities whenever possible and remain interoperable with the official tools.

## 2. Overall System Composition

The six functional responsibilities are listed below; their technical ownership is defined in [architecture](architecture.md).

<a id="cy-02-01"></a>

**Disposition: Required.**

**CY-02-01.** Keep six system responsibilities explicit and run deterministic state and event handling without model calls.

Zoe separates its clients from its server.

It has six functional parts:

| System part | Core responsibility |
|---|---|
| Client | Voice and text interaction, task progress, result display, and notifications |
| Core management system | User requests, tasks, projects, state, permissions, and event management |
| Super Agent | Understanding complex requirements, planning, decision-making, and coordination across tasks |
| Worker Agents | Specialist task execution and local intelligent decisions |
| Background services | Scheduled tasks, event listeners, long-running services, and automation programs |
| Data and memory system | Persistent data, history, project knowledge, long-term memory, and retrieval |

Each part must have clearly defined responsibilities.

The system should not require every event, state update, or background operation to pass through a large language model.

## 3. Client Requirements

The macOS client is an ambient screen defined in [macOS ambient screen](#macos-ambient-screen) and [ADR 0008](architecture/decisions/0008-ambient-screen-client.md). iPhone and iPad have no approved interface design yet. Behavior boundaries every client must keep are in [Client behavior boundaries](#client-behavior-boundaries).

End voice and Stop task are distinct actions. Voice acceptance includes a 30-minute mixed Mandarin–English session on each of a real iPhone and iPad while locked, plus interruption, reconnect, audio-route changes, and durable-task continuity; this is a target, not a completed result. APNs only prompts a durable-inbox refresh; a push is not the notice authority.

<a id="cy-03-01"></a>

**Disposition: Required.**

### 3.1 Cross-Device Access

**CY-03-01.** macOS, iPhone, and iPad clients share one authoritative backend and consistent project/task/memory state.

Zoe should support access to the same backend system from different devices.

Target clients include:

- macOS
- iPhone
- iPad

The architecture should leave room to add other devices and platforms in the future.

Different devices accessing the same server should see consistent projects, tasks, memories, and runtime state.

Clients should not maintain separate, conflicting task states.

<a id="cy-03-02"></a>

**Disposition: Required.**

### 3.2 Minimal Interaction Interface

**CY-03-02.** Provide a simple voice/text interface for tasks, progress, results, projects, services, approvals, notices, and optional technical detail.

The Zoe client is primarily for communicating with the system, not for managing large amounts of technical detail.

Core features include:

- Natural voice conversation
- Text input and output
- Viewing current tasks
- Viewing task progress
- Displaying results
- Viewing project status
- Viewing background-service status
- Proactive notifications
- User authorization and confirmation
- Alerts for errors, exceptions, and issues requiring human attention

The interface should be as simple as possible.

Ordinary users should be able to use Zoe without understanding how Super, Workers, databases, or background processes work.

Advanced status and operation details should still be available when needed.

<a id="cy-03-03"></a>

**Disposition: Required.**

### 3.3 Natural Voice Interaction

**CY-03-03.** Support contextual real-time voice, interruption, cross-device continuation, visible results, and task lifetimes independent from voice sessions.

Natural speech is one of Zoe's primary interaction methods.

Requirements:

**Natural conversation**

Users should be able to make requests as they would in an ordinary conversation, without memorizing fixed commands.

**Continuous context**

The system should understand how the current conversation relates to earlier content.

**Real-time interaction**

Support fluent voice input and output while minimizing perceived latency.

**Allow interruption**

Users can interrupt the system while it is speaking, correct it, or change their request.

**Separate voice from tasks**

Ending a voice session does not end a background task.

**Continue across devices**

A user can begin a conversation on one device and continue related work on another.

**Provide text or visual results when appropriate**

Complex reports, task lists, programming outcomes, and validation summaries must be readable as text or visual results rather than expressed entirely through speech. Source code, raw execution logs, terminals, diff viewers, and test consoles are not native assistant surfaces; work products remain available through authorized external tools and artifact access.

<a id="cy-03-04"></a>

**Disposition: Required.**

### 3.4 Proactive Interaction

**CY-03-04.** Proactively notify users about completion, failure, mail, approvals, scheduled outcomes, and unrecoverable conditions with source links.

Zoe should not merely wait for users to ask questions.

It should be able to proactively notify users when important events occur in the background.

Examples include:

- A development task has completed
- A service has encountered an error
- An important email needs attention
- A task requires user authorization
- Scheduled work has produced an important result
- A background operation failed and could not recover automatically

A notification should identify its event source and link to the relevant project or task.

### macOS ambient screen

Zoe on macOS is an ambient screen, not a chat app: no sidebar, tabs, or persistent composer. The AI decides what the screen shows, so the user should not need to operate it. Wire shapes, component library, and tools are in [the screen-document contract](design/screen-document.md).

- **Voice first.** Continuous voice is on by default after connection and identity initialization, within the truthful-state limits below. After a silence timeout (default 60 seconds, counted only when no voice interaction is in progress) the client ends the voice session itself; the user can wake it later and continue because context stays on the server. Voice on/off and the timeout are AI-mutable settings.
- **Text fallback.** A subtle button reveals a transient text-input bar; the AI may also show or hide it. Submissions follow the rules in Client behavior boundaries.
- **Settings stay out of the foreground.** Command-Comma opens Settings, which hold connection and key management, account, memory, capabilities, notifications, data, and advanced options, following [the instance-key decision](architecture/decisions/0004-instance-key-authentication.md).
- **AI-controlled surfaces.** The AI opens, updates, and closes its own surfaces (windows, panels, menu-bar item, popover, Dock badge) and sets their size, position, display, level, opacity, click-through, focus, fullscreen, and Dock visibility. There are no fixed regions; the AI composes any layout.
- **Generic display.** Each surface shows a versioned tree of layout and content primitives, data-bound nodes, and standard components named by information shape (metric, timeline, decision, collection, and others in the contract). The AI picks components from a client-supplied catalog and chooses semantic emphasis and tone; the client owns colors and motion. Unknown types degrade to their fallback. The client has no hardcoded business concepts: an approval is a `decision`, and a press submits only an opaque intent that the server validates.
- **AI tools and settings audit.** The client exposes tools for surfaces, the component catalog, settings, voice, the text bar, and the silence timeout. Each setting declares type, range, description, and whether the AI may change it. Only the user may change instance key and login credentials, OS microphone permission, and the setting that revokes AI settings access. Every AI settings change shows a subtle notice and is recorded so the user can review and undo it. The menu-bar item always exists, and it shows when the microphone is in use.
- **Before a server document exists,** the client shows a plain chronological list of authoritative tasks, approvals, and notifications through the same renderer, without ranking or invented content.
- **Acceptance coverage.** Hidden app recoverable from the menu bar; microphone state visible while hidden; AI window changes (size, position, display, click-through, focus, Dock visibility) and user-action precedence; multiple surfaces; every standard component and its fallback; size-adaptive rendering; data-binding updates; Mac 840×620 and desktop sizes; empty, loading, and document-update states; transient text fallback; voice lifecycle and silence timeout; AI settings notice, audit, and undo; opaque intent submission; stale, offline, and failed operations; duplicate events and reconnect history; rotated keys, connection clearing, and identity changes; both languages and appearances; VoiceOver, keyboard focus, reduced motion, and Dynamic Type. Command-Comma is the Settings shortcut.
- **Simple appearance, useful density.** Use native typography, restrained semantic colors, compact rows, and clear grouping. Keep more useful conversation and task information visible by reducing oversized headings, excessive whitespace, decorative cards, and repeated metadata. Preserve readability, keyboard access, Dynamic Type, and platform touch targets; density does not mean uniformly smaller text or controls.
- **Conversation leads.** Keep projects and recent conversations/tasks in a compact sidebar. Show task receipts and concise progress/results in context. A compact, collapsible task rail may surface ongoing work, pending decisions, and recent completion on wide windows; collapse secondary surfaces before constraining the conversation in narrow windows. Avoid statistics dashboards and compulsory workflow panels.
- **Voice is Settings-controlled.** Do not place a microphone button in the composer. Continuous voice is off
until the user opts in through Settings. When enabled, the client automatically attempts to establish and
maintain an authorized voice session after connection and identity initialization, subject to OS microphone
permission, audio-session policy, app lifecycle/background limits, network, account-pool capacity, and server
availability. The client must report connecting, active, interrupted, reconnecting, unavailable,
permission-denied, and stopped states truthfully; it must not claim active voice when any required system,
device, network, account, or server condition is unavailable. Automatic reconnect is appropriate for temporary
 failures; permission or policy failures require a Settings/system-settings recovery path. Text chat remains
usable whenever voice is unavailable. Continuous voice transports audio and spoken output only; authorization
follows the impact levels in [ADR 0009](architecture/decisions/0009-voice-steward-impact-policy.md), and voice never
confirms high-impact work. Do not add persistent labels, large waveforms, timers, voice cards, or separate
persistent control strips. Settings controls and status messages need accessible names/state values, keyboard
access, and help on demand; color alone must not carry state. Detailed voice, transport, account, and server
errors are not a client presentation responsibility: the client shows only a concise actionable state and
automatically submits the redacted diagnostic through the authenticated diagnostic channel when available.
- **Automation reduces required interaction.** After the user expresses and authorizes the intent, work
proceeds within its granted scope. Keep ordinary progress concise; emphasize decisions that require the user,
actionable failures, and recorded results. Ordinary assistant replies and ordinary task results are
server-authored content, not client-side Accept/Reject workflows. Users continue through chat by asking for
changes, continuation, or missing information; ambiguous words do not authorize a business decision.
High-impact or irreversible actions require scoped, explicit conversational confirmation tied to the
task/run/action, expected version, and expiry. Continuous voice does not alter these boundaries.
- **Focus on outcomes.** Summarize programming work using its goal, current stage, outcome, validation summary, and decisions that require the user. Users should be able to understand what was accomplished and what remains unresolved through these summaries, without reviewing source code. The same conversation and task surfaces must support research and other authorized work.
- **Client errors are concise; diagnostics are automatic.** This applies to every native-client error surface,
 including connection, voice, task, notification, export, capability, storage, and transport failures. The
client must not display detailed server, infrastructure, runtime, transport, account, or local exception text.
 It shows only a short state and an actionable recovery or retry instruction; detailed context is redacted,
encrypted, queued, and automatically returned to the authenticated diagnostic endpoint when available.
Diagnostic upload is best effort and must not block the user, alter command authority, or change receipt/retry
semantics. Authentication failures and user-input validation may show the minimum information needed to
recover, but must not expose credentials, headers, response bodies, transcripts, audio, SDP/ICE, or
secret-bearing details.
The authenticated client synchronizes its server-controlled diagnostic policy and queued events every 60 seconds by default.

### Client behavior boundaries

- **Truthful state.** Voice reports connecting, active, interrupted, reconnecting, unavailable, permission-denied, and stopped states only when they are true; permission or policy failures need a recovery path, temporary failures may reconnect, and text input remains usable whenever voice is unavailable. Normal connected state has no banner; offline, stale, and failed operations stay visible with an action.
- **Impact-level authorization.** A spoken request is the instruction for low-impact, reversible work; the server sets impact levels from the profile's capabilities and scopes ([ADR 0009](architecture/decisions/0009-voice-steward-impact-policy.md)). Voice never confirms high-impact work. High-impact or irreversible actions require scoped, explicit confirmation tied to the task/run/action, expected version, and expiry; ambiguous words authorize nothing.
- **Server-authored content.** Clients render persisted server data and never fabricate messages, progress, assistant answers, or inactive actions. Ordinary results are server-authored content with no client Accept/Reject workflow; follow-ups are interpreted by the server in task context. Contracts: [HTTP contract](../contracts/http.go) and [task-entry handlers](../internal/control/task_entries.go).
- **Submissions.** A submission is accepted, failed, or unknown. Drafts clear only after durable acceptance; unknown submissions reconcile the original journaled receipt instead of issuing a new command, and the draft stays visible with confirmation pending. Drafts are cleared when the server or identity changes.
- **Concise errors, automatic diagnostics.** Every client error surface shows only a short state and an actionable recovery or retry instruction, never detailed server, infrastructure, runtime, queue-reason, capacity, transport, account, or local exception text, credentials, headers, response bodies, transcripts, audio, or SDP/ICE. Redacted detail is encrypted, queued, and sent best effort to the authenticated diagnostic endpoint without blocking the user or changing command authority or receipt/retry semantics.
- **Outcomes, not developer tools.** Programming work is summarized by goal, stage, outcome, validation summary, and decisions needed. Clients provide no source-code browsing or editing, diff viewer, terminal, or test console, including optional or hidden variants.
- **Platform conventions.** English and Simplified Chinese; system appearance; dates and counts use the system region; server-authored content keeps its original language; accessibility support on each platform. Authority and diagnostics boundaries: [architecture](architecture.md#identity-clients-and-notices).

Real-device, simulator, build, and service evidence stays separate in [the evidence register](acceptance/evidence.md); design references cannot establish Apple compilation, screenshots, interaction, or end-to-end acceptance.

## 4. Server Requirements

When voice calls are ingested, the server retains both speakers' full text under the [transcript lifecycle contract](operations/data-lifecycle.md#voice-transcript-authority-and-ingestion), independently of task delegation and raw audio.

When voice calls end, the server derives dated topic summaries mapped to authoritative raw turns under the [stored summary generation contract](architecture/decisions/0012-voice-conversation-layer.md#stored-call-summary-generation). Summary generation does not change command authority or confirmed user decisions.

The server baseline and migration requirements are defined in [§4.1](#41-independent-background-operation), [§4.2](#42-separation-of-client-and-server), and [§4.3](#43-environment-migration). Native Apple clients do not host the control server. Migration and restore must preserve linked identities and prevent two active copies of the authority.

<a id="cy-04-01"></a>

**Disposition: Required.**

### 4.1 Independent Background Operation

**CY-04-01.** Run the backend independently on supported host OSes; report sleep, shutdown, and network loss accurately and recover safely.

The Zoe server should be able to run as an independent background system.

For the first release, the server and execution host use **Linux**. They can be deployed on a Linux server or another supported Linux environment to execute agent tasks and run ordinary background services.

Running the Zoe server or Worker background services on macOS is outside the current baseline. Native macOS clients remain in scope; there is no browser/PWA client.

The server should not depend on a graphical client remaining open.

Closing the Zoe client should not stop background services that are already running independently.

If the execution environment becomes unavailable because a device is powered off, asleep, or disconnected, the system must be able to identify that condition and continue processing according to task rules when conditions recover.

<a id="cy-04-02"></a>

**Disposition: Required.**

### 4.2 Separation of Client and Server

**CY-04-02.** Keep agent execution, services, data, memory, permissions, events, status, and health on the server.

For delegated voice tasks, Zoe owns stateless Steward admission and the resident OpenCode Worker executes work; the current bootstrap policy and limits are defined once in the [task-manager contract](architecture/decisions/0012-voice-conversation-layer.md#stateless-task-manager).

The client is primarily responsible for interaction and presentation.

The server is responsible for:

- Agent management
- Task execution
- Background-service operation
- Data storage
- Long-term memory
- Permission control
- Event handling
- State management
- Service health monitoring

Users can access the same backend through different clients.

<a id="cy-04-03"></a>

**Disposition: Required.**

### 4.3 Environment Migration

**CY-04-03.** Migrate linked projects, history, agents, tasks, memory, permissions, services, state, and execution files between environments.

Zoe data and work products must remain migratable across supported Linux environments.

When moving to a different Linux backend server or supported execution environment, it should be possible to migrate through backup, export, and restore:

- Project data
- Conversation history
- Agent configuration
- Task records
- Long-term memories
- Permission configuration
- Background-service configuration
- System state
- Necessary execution files and working-directory associations

Native operating-system processes may not migrate directly, but the system should retain enough information to restart or resume the corresponding task or service in the target environment.

## 5. Agent and runtime integration requirements

OpenCode is Zoe's only execution runtime. Zoe's trusted runner creates one host OpenCode session per run through the dedicated service; the control plane remains the business-state authority. CLIProxyAPI is an external OpenAI-compatible provider and is not part of Zoe's deployment lifecycle. Runtime identity, completion, stop, recovery, and usage must be based on runtime APIs and durable Zoe evidence, never on chat text alone.

The first release prioritizes automatic Super planning, independent Workers, multiple workspaces, long-running tasks, task recovery, artifacts, and cost records. Native CLI or desktop-tool handoff is not a required runtime path.

<a id="cy-05-01"></a>

**Disposition: Required.**

### 5.1 Zoe invoking OpenCode

**CY-05-01.** Create and manage OpenCode work with a real workspace, instructions, state/results, task links, supported continuation, and stop.

Zoe must create a deterministic runtime boundary, send instructions through the OpenCode API, record the runtime session identity, observe idle completion outcomes, collect artifacts and usage, and support interrupt plus verified absence from active host sessions. A prompt acknowledgement is not completion. Unknown creation outcomes remain `effect_unknown`; they must never be replayed blindly.

<a id="cy-05-02"></a>

**Disposition: Required.**

### 5.2 OpenCode provider and model routing

**CY-05-02.** Route each selected model through the configured OpenCode provider without exposing provider credentials to the runtime or changing the external CLIProxyAPI deployment.

The runner supplies the provider through the reviewed bridge/socket boundary, keeps the provider key in a protected secret file, and records the selected provider/model route. Provider failure, unavailable credentials, and unsupported models fail closed before or during execution with truthful task evidence.

<a id="cy-05-03"></a>

**Disposition: Required.**

### 5.3 OpenCode completion and usage

**CY-05-03.** Report OpenCode completion, failure, token usage, and cost from provider-backed evidence.

Completion requires a non-busy session and a latest assistant message with a terminal completion, finish, or error fact. Streaming finish and usage propagation through the provider bridge remain acceptance gates; no result may claim metering from a request acknowledgement alone.

<a id="cy-05-04"></a>

**Disposition: Required.**

### 5.4 OpenCode recovery and concurrency

**CY-05-04.** Recover or fail closed after runner, server, or runtime crashes without duplicate execution or concurrent writes.

The runner must use deterministic run identity, one session per run, durable journal identities, and fail-closed handling for uncertain creation. In-flight crash recovery, session continuity, and exact stop evidence require live acceptance. A session title is not an idempotency key.

<a id="cy-05-05"></a>

**Disposition: Required.**

### 5.5 Runtime integrity and tool limits

**CY-05-05.** Preserve workspace, task, runtime, artifact, and tool-step integrity while preventing uncoordinated concurrent writes.

The runner enforces time, budget, and tool-step limits through event observation and abort because OpenCode does not provide a per-run tool-step limit. It must verify exact container removal for termination and keep ambiguous runtime identity unresolved. Shared workspaces require explicit ownership and conflict protection.

<a id="cy-05-06"></a>

**Disposition: Required.**

### 5.6 Runtime independence and migration

**CY-05-06.** Keep Zoe runtime outputs and migration records usable during provider/runtime maintenance and migration.

Projects, code, artifacts, task history, runtime session identity, usage records, and unresolved recovery evidence must remain accessible without inventing unsupported native-tool handoff. The four uncertain historical runs were abandoned by operator decision; they remain historical records and require no migration.

## 6. Agent System Requirements

Separate the coordination role, runtime implementation, and model/provider route. The coordination, routing, and collaboration requirements are defined in [§§6.1–6.4](#61-super-agent). The shared ceiling is the `ZOE_MAX_RUNS` operator setting, defaulting to 12 active runs, including Super, Workers, and memory curator. Adapter registration alone does not establish support or acceptance.

<a id="cy-06-01"></a>

**Disposition: Required.**

### 6.1 Super Agent

**CY-06-01.** Super interprets complex goals, plans dependencies, chooses workers, adjusts execution, and escalates when needed.

Super is the system's high-level intelligent coordinator.

Its core responsibilities include:

- Understanding complex requests
- Breaking complex goals into tasks
- Preparing an execution plan
- Selecting suitable Workers
- Coordinating multiple Workers
- Handling dependencies between tasks
- Adjusting the plan based on task results
- Addressing problems that require reasoning across domains
- Deciding which issues require user involvement
- Maintaining continuity across complex work

Super should not perform every low-level operation.

In particular, the system should not have to invoke Super just to update a status, check the time once, or evaluate an explicit rule.

<a id="cy-06-02"></a>

**Disposition: Required.**

### 6.2 Worker Agents

**CY-06-02.** Workers execute specialist work with scoped context/tools and return independent results, records, and errors.

A Worker is an execution unit responsible for specific intelligent work.

Different Workers can have different specialties, such as:

- Software development
- Code review
- Document analysis
- Email understanding
- Information organization
- Data analysis
- Application operation

A Worker must be able to:

- Receive a clear task
- Obtain necessary context
- Use authorized tools
- Execute a task independently
- Report its results
- Save necessary execution records
- Report problems or request coordination when exceptions occur

A Worker does not have to be created or invoked by Super.

<a id="cy-06-03"></a>

**Disposition: Required.**

### 6.3 Direct Invocation of Workers

**CY-06-03.** Route ordinary events to deterministic programs, semantic judgment to workers, and complex cases to Super as needed.

An ordinary background service can invoke a designated Worker directly according to rules.

For example:

An email-monitoring program receives an email.

It handles a simple system notification itself.

If the importance of the email needs to be assessed, it can invoke an email-analysis Worker.

If the email concerns multiple projects, a complex plan, or an important decision, it can escalate the issue to Super.

The call path should be:

**Background service → Worker → Super when needed**

rather than requiring every action to follow:

Background service → Super → Worker.

<a id="cy-06-04"></a>

**Disposition: Required.**

### 6.4 Multi-Agent Collaboration

**CY-06-04.** Support parallel agents, dependencies, result passing, unified status, and controlled failure, cancel, and recovery.

The system must support multiple Agents handling different tasks at the same time.

This includes:

- Different Workers executing in parallel
- Workers passing results to one another
- Managing dependencies between tasks
- Unified execution status
- Failure handling
- Task cancellation
- Task recovery
- Consolidating results

Agent execution must follow unified permission, task, and state rules rather than maintaining conflicting information independently.

## 7. Autonomous Capability Expansion

Capability expansion covers authorized creation, validated release, constrained deployment manifests, and independently managed installed capabilities with health, configuration, permissions, immutable release artifacts, and persistent-data policy; see [Sections 7.1–7.3](#71-creating-new-capabilities) for the governing lifecycle and execution rules. A generated program is not permission to deploy it. Pluggable domain examples describe extensibility, not a claim that every connector exists.

<a id="cy-07-01"></a>

**Disposition: Required.**

### 7.1 Creating New Capabilities

**CY-07-01.** Identify durable capability needs and create or configure the program only for authorized deployment.

Zoe should not only use existing features; it should also be able to create new system capabilities in response to user needs.

For example, a user says:

“Check my important email every day from now on, and tell me when something important arrives.”

Zoe should understand that this is a request for a long-term capability, not a one-time email lookup.

The system can arrange for a Worker to write a program or configure existing tools and services.

After obtaining the necessary authorization, it can deploy the corresponding background service.

The service can then run independently.

<a id="cy-07-02"></a>

**Disposition: Required.**

### 7.2 Lifecycle of a New Capability

**CY-07-02.** Manage capability install, configuration, authorization, run, pause, resume, update, stop, removal, health, and errors.

Zoe must be able to manage the complete lifecycle of an added capability: creation, installation, configuration, authorization, startup, pause, resume, update, stop, uninstall, runtime status checks, and exception handling.

Users should be able to see which capabilities are installed and whether they are currently running.

<a id="cy-07-03"></a>

**Disposition: Required.**

### 7.3 Independent Operation of New Capabilities

**CY-07-03.** Keep long-running ordinary programs alive independently; call agents only where judgment is needed.

An ordinary program that has been created should not need Super to participate continuously in order to work.

For example, an email-checking service can continuously monitor a mailbox.

Most of the time it does not need to call any large language model.

It calls the relevant Agent only when it encounters content that needs semantic understanding or complex decisions.

This means:

**AI can create a capability without necessarily being responsible for its ongoing execution.**

<a id="cy-07-04"></a>

**Disposition: Required.**

### 7.4 Scope of Capability Expansion

**CY-07-04.** Allow pluggable capabilities across mail, calendar, files, development, web, apps, system, schedules, and monitoring.

Zoe should be able to gain a range of capabilities through modules, external services, or programs it creates autonomously.

Examples include email, calendars, file management, development tools, GitHub, browser automation, third-party apps, system operations, scheduled tasks, information monitoring, and other software services that can be authorized.

Adding a capability should not require changing the entire system's core architecture.

## 8. Task Management Requirements

Task management defines durable execution identity, lifecycle semantics, recovery, and project associations; see [Sections 8.1–8.5](#81-unified-task-system) for the requirements. State represents observed reality rather than conversation or UI animation, and a stop request or agent answer alone does not prove termination or goal completion.

<a id="cy-08-01"></a>

**Disposition: Required.**

### 8.1 Unified Task System

**CY-08-01.** Represent every execution subject with a unified task identity, goal, source, status, times, result, errors, recovery, and records.

Zoe needs a unified system for managing all tasks.

Every task, whether executed by Super, a Worker, Codex, an ordinary program, or another service, should have a clear identity and status.

Task information must include at least:

- Task identifier
- Associated project
- Task objective
- Creation source
- Executing entity
- Current status
- Start time
- Most recent update time
- Execution result
- Error information
- Necessary recovery information
- Related execution records

<a id="cy-08-02"></a>

**Disposition: Required.**

### 8.2 Task Lifecycle

**CY-08-02.** Define real semantics for queued, active, waiting, awaiting user, paused, complete, failed, and canceled states.

The system must support the following states or equivalent semantics:

| Status | Meaning |
|---|---|
| Pending | The task has been created but has not started |
| In progress | The task is executing |
| Waiting | Waiting for a dependency, external event, or other condition |
| Waiting for user | User authorization, confirmation, or additional information is needed |
| Paused | Execution is temporarily stopped |
| Completed | Completion criteria have been met |
| Failed | Execution failed |
| Canceled | The task was canceled |

The system must update status based on actual execution.

It must not mark work as complete without verification merely because an Agent said “done.”

Task status is separate from server-owned interaction state. Interaction state may indicate
`awaiting_message`, `explicit_confirmation`, or exceptional `approval_required`; ordinary completed results
have no interaction requirement. The API name `awaiting_user` remains supported for compatibility and may be
used as a legacy status/label, but it must not be treated as a client-owned workflow. When a waiting task
accepts `task.append`, the server persists the user message, preserves prior result and evidence history,
applies policy and optimistic-version checks, and emits the authoritative event; the client does not
synthesize a task-history entry or decide whether the message is clarification, change, continuation, or
confirmation.

<a id="cy-08-03"></a>

**Disposition: Required.**

### 8.3 Long-Running Tasks

**CY-08-03.** Allow long tasks to outlive sessions and expose current evidence-backed progress later.

Zoe should support tasks that take a long time, such as software development, complex research, or multi-step automation.

Users do not need to keep a conversation open.

They can ask at any time:

“How far along is that task from earlier?”

The system should retrieve its actual current status and explain it using context when needed.

<a id="cy-08-04"></a>

**Disposition: Required.**

### 8.4 Task Recovery

**CY-08-04.** Persist outcomes, external status, pending work, and recovery data; avoid repeating irreversible effects after uncertainty.

When task execution is interrupted, the system should preserve as much as possible of:

- Completed steps
- Execution results
- External resource state
- Uncompleted steps
- Information needed for recovery

Recovery should avoid unnecessary repetition.

For steps that may have irreversible effects, the system must take special care to prevent duplicate execution.

<a id="cy-08-05"></a>

**Disposition: Required.**

### 8.5 Association Between Tasks and Projects

**CY-08-05.** Link project tasks, dependencies, parallel and child work, shared resources, and ambiguous references.

A project can have multiple tasks.

Tasks can have:

- Sequential dependencies
- Parallel relationships
- Parent-child relationships
- Shared-resource relationships

Zoe should understand what the user means by “that earlier project,” “the task from a moment ago,” or “yesterday's development work.”

If several objects are possible, it must not choose one arbitrarily.

## 9. Long-Running Background Services

A finite task and a durable service have different lifetimes; each service trigger creates an execution record. Schedules, listeners, and rule evaluation continue independently of Super and clients, while intelligent work enters the same authorized admission path as other runtime work. Source-linked notices are defined in [Sections 9.1–9.4](#91-a-service-is-not-a-task).

<a id="cy-09-01"></a>

**Disposition: Required.**

### 9.1 A Service Is Not a Task

**CY-09-01.** Separate finite task lifecycles from durable service lifecycles and give each service trigger a run record.

Zoe must clearly distinguish between a **one-time task** and a **long-running service**.

For example:

“Check my email once” is a task.

“Monitor my email from now on” is a service.

They need different lifecycle management.

<a id="cy-09-02"></a>

**Disposition: Required.**

### 9.2 Background-Service Management

**CY-09-02.** Store service identity, purpose, scope, environment, trigger, permission, configuration, status, health, errors, and recovery details.

A service needs its own record, including:

- Service identity
- Service purpose
- Project or scope
- Runtime environment
- Trigger conditions
- Permissions
- Configuration
- Current runtime status
- Most recent execution record
- Health status
- Error record
- Stop and recovery methods

<a id="cy-09-03"></a>

**Disposition: Required.**

### 9.3 Event Triggers

**CY-09-03.** Trigger programs or agents from mail, files, schedules, APIs, task outcomes, failures, and user events.

Background services can perform actions in response to different events, such as:

- A new email arrives
- A file changes
- A timer fires
- An external API returns an event
- A task completes
- The system encounters an abnormal state
- A user issues a command

An event can trigger an ordinary program, a Worker, or Super.

It should not default to triggering Super for every event.

<a id="cy-09-04"></a>

**Disposition: Required.**

### 9.4 Proactive Notifications

**CY-09-04.** Classify and deduplicate routine, completion, failure, and decision notices; prevent noisy repetition.

When a background service produces an event that deserves the user's attention, it should be able to send a notification.

Notifications should distinguish ordinary information, task completion, runtime errors, and events requiring a user decision.

The system should avoid duplicate notifications and frequent, meaningless reminders as far as possible.

## 10. Long-Term Memory Requirements

Long-term memory is layered, source-linked, authority-aware, and scoped; confirmed decisions, execution evidence, and model inference remain distinguishable. Record sources, versions, dates, validity, correction history, and tombstones; automatic summaries cannot overwrite confirmed decisions, curator judgment is on demand, and missing or conflicting history remains uncertainty. Detailed requirements are in [Sections 10.1–10.6](#101-persistent-memory).

<a id="cy-10-01"></a>

**Disposition: Required.**

### 10.1 Persistent Memory

**CY-10-01.** Retain source-linked memory across conversations, tasks, projects, devices, and restarts.

Zoe needs long-term memory across conversations, tasks, projects, and devices, independent of a single model context window and recoverable after client closure, session end, or service restart.

<a id="cy-10-02"></a>

**Disposition: Required.**

### 10.2 Memory Layers

**CY-10-02.** Separate global, project, task, and raw-history memory while preserving project access boundaries.

At minimum, memory includes the categories and boundaries defined below.

#### Global Memory

Store generally applicable information that remains valid over time, such as:

- User or organization preferences
- Communication habits
- General working rules
- Long-term goals
- Confirmed ongoing constraints

#### Project Memory

Store project-specific information such as:

- Project goals
- Product requirements
- Architecture decisions
- Historical changes
- Design rationale
- Project background
- Important discussions

Memories for different projects need clear boundaries.

#### Task Memory

Store the following for a specific task:

- Objective
- Status
- Execution history
- Results
- Unresolved issues
- Recovery context

#### Raw History

Store necessary source material, including:

- Conversation records
- Operation records
- Agent output
- Important events
- Sources of decisions
- Execution logs

Raw history should be available as evidence for other memory content.

<a id="cy-10-03"></a>

**Disposition: Optional.**

### 10.3 Memory Agent

**CY-10-03.** Optional event-driven Memory Agent may identify facts, links, duplicates, staleness, conflicts, and update suggestions.

Zoe can use a separate Memory Agent to organize memory.

Its responsibilities include:

- Identifying information worth retaining long term
- Extracting key facts
- Organizing project history
- Linking related information
- Finding duplicate information
- Finding outdated information
- Identifying contradictions
- Proposing memory updates

The Memory Agent does not need to participate in every user conversation.

The system should allow memory organization to run on background events or suitable triggers.

<a id="cy-10-04"></a>

**Disposition: Required.**

### 10.4 Memory Accuracy

**CY-10-04.** Distinguish confirmed facts, execution evidence, and AI inference; track decision timing and validity.

Zoe must distinguish between:

**Facts explicitly confirmed by the user, results actually produced by the system, and content inferred by AI.**

An inference must not be treated directly as an authoritative fact.

For example:

The user first decides to use SQLite.

Later, the user formally decides to switch to PostgreSQL.

The system should understand:

- What the earlier decision was
- What the new decision is
- When the change happened
- Which decision is currently effective
- What evidence supports it

It must not simply combine old and new information.

<a id="cy-10-05"></a>

**Disposition: Required.**

### 10.5 Separate Authoritative Data from AI Memory

**CY-10-05.** Keep task, permission, configuration, and confirmed-decision authority outside unrestricted memory writes.

Formal task status, permissions, system configuration, and confirmed decisions must be saved in reliable, persistent records.

A Memory Agent can organize this information but must not arbitrarily modify authoritative records without permission.

In particular, an automatic summary must not overwrite a confirmed user decision.

<a id="cy-10-06"></a>

**Disposition: Required.**

### 10.6 Retrieval on Demand

**CY-10-06.** Retrieve only relevant authorized history, cite sources, save context, and surface missing/conflicting evidence as uncertainty.

Zoe should not send the entire history to the model on every call.

The system needs to retrieve only information relevant to the current task.

Goals include:

- Reducing token use
- Avoiding irrelevant context
- Preventing information from different projects from being confused
- Improving traceability of historical facts
- Improving accuracy when continuing work

When retrieval is insufficient or results conflict, the system should state the uncertainty instead of inventing missing history.

## 11. Context Management

Current execution context and long-term memory are separate; the detailed requirement below governs task, runtime, project, and permission-scoped context assembly.

<a id="cy-11-01"></a>

**Disposition: Required.**

**CY-11-01.** Assemble current context separately from long-term memory according to task, agent, and permissions.

Zoe should be able to provide each Agent with context relevant to its task.

For example, a development Worker needs the project structure, technical requirements, and current task.

An email Worker needs the email content, relevant handling rules, and necessary context.

Agents do not need every past conversation. Super may access cross-task information when coordinating complex work, but not all system data unconditionally; context must balance completeness, relevance, token cost, permission boundaries, and memory accuracy.

## 12. Unified State and Cross-Device Continuity

Cross-device continuity uses durable identities and authoritative versions, marks stale/offline views and reconciles after reconnect, handles ambiguous follow-ups, and preserves durable context without promising live-audio transfer; see [Sections 12.1–12.3](#121-unified-source-of-data).

<a id="cy-12-01"></a>

**Disposition: Required.**

### 12.1 Unified Source of Data

**CY-12-01.** Use one versioned state authority across devices and expose stale cache/conflicts clearly.

Different clients viewing the same task must not show unexplained conflicting states.

For example, if a Mac shows a task as completed, an iPhone should not incorrectly continue to show that task as running.

<a id="cy-12-02"></a>

**Disposition: Required.**

### 12.2 Continuing Work Across Devices

**CY-12-02.** Continue one linked project/task across Mac and iPhone and return to the same work context.

When a user resumes work on another device, the typical scenario is:

A user tells Zoe on a Mac:

“Develop a new user authentication module for me.”

The user then leaves the computer.

On an iPhone, the user asks:

“How is the authentication module from earlier coming along?”

Zoe should identify the original task and report its actual progress.

The user then says:

“When the code is done, check it for security issues too.”

The system should associate this request correctly with the original task or a follow-up task.

When the user returns to the Mac, they can continue interacting through Zoe and, where supported, continue development directly in the official Codex tool.

<a id="cy-12-03"></a>

**Disposition: Required.**

### 12.3 Separate State from Conversation

**CY-12-03.** Keep task existence and status independent of any conversation window.

A task remains present when the user starts a new conversation; conversation is only one way to access and control it.

## 13. Modular AI and Services

Model, agent, voice, and external-service implementations are replaceable through explicit contracts; the detailed independence requirements are in [Sections 13.1–13.4](#131-independence-from-ai-models). The existing remote CLIProxyAPI deployment is reused unchanged.

<a id="cy-13-01"></a>

**Disposition: Required.**

### 13.1 Independence from AI Models

**CY-13-01.** Replace models without rewriting task, memory, and service records.

Zoe must support different AI models and services without rewriting task management, long-term memory, or background services.

<a id="cy-13-02"></a>

**Disposition: Required.**

### 13.2 Agent Independence

**CY-13-02.** Integrate heterogeneous agents through shared capability, execution, result, and permission contracts.

Codex can serve as a development Worker while other Agents handle other work. They need not share an implementation, but Zoe must know their capabilities, execution, result, and permission contracts.

<a id="cy-13-03"></a>

**Disposition: Required.**

### 13.3 Independence of Voice Services

**CY-13-03.** Replace speech recognition, synthesis, or live voice without changing core task and project data.

The voice system should be independently replaceable: changing speech recognition, synthesis, or real-time voice must not affect long-term memory, task state, agent coordination, background services, or project data.

<a id="cy-13-04"></a>

**Disposition: Required.**

### 13.4 Independence of External Services

**CY-13-04.** Isolate external service integration failures from unrelated modules.

Email, calendars, GitHub, file systems, and other third-party services should be independently integrated, updated, or removed; an unavailable service must not make unrelated modules fail as a whole.

## 14. Permission and Security Requirements

Authorization applies to the actual action and resource; see [Section 14.2](#142-operation-authorization). Authority delegation, user control, and audit follow [Sections 14.1](#141-separate-permissions), [14.3](#143-user-control), and [14.4](#144-isolation-and-audit).

The current fresh-deployment identity is the fixed owner/workspace using a shared instance key. Runner and memory-worker credentials remain separate. Local client clearing removes local connection data; it does not revoke the shared key or stop server tasks. See the [authentication decision](architecture/decisions/0004-instance-key-authentication.md).

Zoe can run programs, modify files, and operate external applications, so it must have clear permission controls.

<a id="cy-14-01"></a>

**Disposition: Required.**

### 14.1 Separate Permissions

**CY-14-01.** Apply least privilege per agent and service; Super authority does not implicitly transfer to workers.

Different Agents and background services should receive only permissions relevant to their work; a permission held by Super must not automatically give every Worker the same permission.

<a id="cy-14-02"></a>

**Disposition: Required.**

### 14.2 Operation Authorization

**CY-14-02.** Authorize read, write, send, delete, generate, and deploy actions separately, with risk-based confirmation; high-impact execution requires scoped explicit conversational confirmation.

Authorize each operation according to its risk. Reading versus sending email, reading versus deleting a file, and writing versus deploying a program are separate permissions. Software installation, high-risk system commands, sensitive configuration changes, and irreversible external operations follow the user's authorization rules.

For a high-impact or irreversible action, confirmation is an execution authorization, not acceptance of a result. The server defines the confirmation scope, task/run/action identity, expected version, expiry, and audit record. Ambiguous assent such as “okay,” “fine,” or an equivalent brief affirmative must not execute the action without contextual resolution.

<a id="cy-14-03"></a>

**Disposition: Required.**

### 14.3 User Control

**CY-14-03.** Let users inspect, change, revoke grants, stop tasks/services, audit actions, and confirm high-risk work.

Users should be able to view and adjust existing permission scope, revoke permissions, stop tasks and services, view important operation records, and confirm high-risk operations.

A single vague authorization must not give a long-running service unlimited operating permissions.

<a id="cy-14-04"></a>

**Disposition: Required.**

### 14.4 Isolation and Audit

**CY-14-04.** Isolate project, worker, service data and execution; protect sensitive context and audit important actions.

Different projects, Workers, and background services should have appropriate data and execution boundaries; sensitive context must not be exposed casually to unrelated Agents, and important operations need traceable execution records.

## 15. Reliability and Exception Handling

The detailed reliability requirements are [exception detection](#151-exception-detection), [fault recovery](#152-fault-recovery), [observable actual state](#153-observable-actual-state), and [background health management](#154-background-health-management). Records remain available while no client is connected.

Zoe must handle more than successful tasks during long-term operation.

<a id="cy-15-01"></a>

**Disposition: Required.**

### 15.1 Exception Detection

**CY-15-01.** Detect service exit, agent failure, unavailable APIs, network loss, timeout, denial, resource shortage, and conflicts.

The system needs to detect a background service exit, Worker failure, unavailable external API, network interruption, execution timeout, insufficient permissions or resources, and conflicting data or state.

<a id="cy-15-02"></a>

**Disposition: Required.**

### 15.2 Fault Recovery

**CY-15-02.** Retry safely; reconcile external effects when possible and require resolution for unknown irreversible outcomes.

Operations that can be retried safely should support retry or recovery; operations that may produce duplicate side effects must be reconciled before retry. For example, recovery after an email-send failure must avoid resending an email that may already have been sent.

<a id="cy-15-03"></a>

**Disposition: Required.**

### 15.3 Observable Actual State

**CY-15-03.** Distinguish request receipt, scheduling, execution, agent output, successful operation, and user-goal completion.

Zoe should distinguish task receipt, scheduling, execution in progress, Agent output, actual operation success, and achievement of the requested result.

These states must not be conflated.

When users ask about progress, Zoe should answer from actual records.

<a id="cy-15-04"></a>

**Disposition: Required.**

### 15.4 Background Health Management

**CY-15-04.** Monitor service health, recover within policy, notify users, and retain context when recovery fails.

Long-running services need to report runtime status, recover automatically or notify the user where necessary, and save the reason and relevant context when recovery fails.

## 16. Data Management Requirements

Persist important records beyond model context and process lifetime; the detailed requirements for migration,
recovery, provenance, security, and data control are in [Sections 16.2–16.6](#162-data-migration). Acceptance
follows durable commit. Data control includes permission-aware inspection, export, and deletion of
authoritative and derived copies, client caches, exports, replicas, service volumes, and backups under a
stated retention policy. Migration/recovery preserves sources, versions, effective times, configuration,
artifacts, and required volumes; secrets use deployment references rather than values in source or evidence.

<a id="cy-16-01"></a>

**Disposition: Required.**

### 16.1 Persistence

**CY-16-01.** Persist important records and report acceptance only after durable commit.

Important Zoe data must be saved long term and must not exist only in model context or process memory.

<a id="cy-16-02"></a>

**Disposition: Required.**

### 16.2 Data Migration

**CY-16-02.** Migrate projects, history, tasks, memory, and services without binding records to one model provider.

The system should support migration of projects, history, tasks, memories, and service configuration; records must not be lost when the AI model changes.

<a id="cy-16-03"></a>

**Disposition: Required.**

### 16.3 Data Backup and Recovery

**CY-16-03.** Back up and restore important database, files, and configuration with rehearsed evidence.

The system needs to save necessary backups and be able to recover important data.

<a id="cy-16-04"></a>

**Disposition: Required.**

### 16.4 Data Sources and Versions

**CY-16-04.** Retain decision/task/memory source, version, and effective-time history.

Important decisions, task records, and memories should retain appropriate timestamps, sources, and version information.

<a id="cy-16-05"></a>

**Disposition: Required.**

### 16.5 Data Security

**CY-16-05.** Protect content, files, conversations, secrets, external grants, and configuration.

The system needs to protect:

- User content
- Project files
- Conversation history
- Keys and credentials
- External-service authorizations
- System configuration

<a id="cy-16-06"></a>

**Disposition: Required.**

### 16.6 Data Control

**CY-16-06.** Support permission-aware inspection, export, and deletion across derived stores and defined backup policy.

Users or authorized administrators should be able to view, export, and delete data as appropriate to its type and permission requirements, including applicable derived stores and backup policy.

## 17. Resource and Cost Management

Ordinary programs handle timers, file checks, explicit rules, conversion, state changes, and health. Idle scheduling and event processing make no model calls. Each admitted model action needs task/run authority, a route, and a budget; record usage and prevent further dispatch when limits are exhausted. Unknown consumption stays reserved pending reconciliation. Numeric limits represented in a DTO do not establish actual provider/tool/artifact enforcement.

Zoe should pay attention to long-term operating costs, especially token use.

<a id="cy-17-01"></a>

**Disposition: Required.**

### 17.1 Invoke AI When Needed

**CY-17-01.** Use programs for time, file, rule, state-transition, and health checks instead of model polling.

Prefer ordinary programs for work they can perform.

Examples include:

- Scheduled triggers
- Checking whether a file exists
- Explicit rule evaluation
- State updates
- Simple data conversion
- Service health checks

Invoke an Agent only when intelligent understanding or reasoning is needed.

<a id="cy-17-02"></a>

**Disposition: Required.**

### 17.2 Context Efficiency

**CY-17-02.** Build minimal relevant context without sacrificing traceability or correctness.

Agent calls should receive only the context they need.

Avoid repeatedly sending large amounts of the same history.

<a id="cy-17-03"></a>

**Disposition: Required.**

### 17.3 Call Management

**CY-17-03.** Record and bound model usage; stop further dispatch when budgets or loop limits are reached.

The system should record and manage resource consumption caused by Agent calls.

Where necessary, it should be possible to set call limits to prevent abnormal tasks from consuming resources indefinitely.

<a id="cy-17-04"></a>

**Disposition: Required.**

### 17.4 Efficiency During Continuous Operation

**CY-17-04.** Keep routine monitoring, scheduling, and event processing efficient without high-frequency model calls.

Background services should not depend on frequent large-language-model calls to stay running.

Long-term monitoring, scheduling, and event handling need mechanisms that can run continuously with stability and low cost.

## 18. Openness and Scope for Expansion

The initial release is private, single-owner, single-instance, and multi-project. Preserve user/workspace and permission-principal boundaries for future evolution. This does not add a team collaboration interface to the current release.

<a id="cy-18-01"></a>

**Disposition: Required.**

**CY-18-01.** Retain extensible ownership and access boundaries for multiple users, projects, agents, devices, and services.

Zoe is not defined as a product that can serve only one person.

Although an individual can deploy and use it independently, the product structure should not assume that there will forever be only one user, one project, or one Agent.

The system should leave room for:

- One user with multiple projects
- Collaboration among multiple Agents
- Access from multiple devices
- Multiple running services
- Different permission principals
- Team or organizational use

Future multi-user support will need corresponding identity, data isolation, permission management, and collaboration capabilities.

**This means the product is not limited to a single-user positioning; it does not mean every deployment must enable multi-user collaboration.**

## 19. Complete Usage Scenarios

These five scenarios combine requirements across components; they are acceptance journeys. Repeated constraints link to their authoritative requirements below. Required evidence and gaps belong in [the acceptance matrix](acceptance/requirements.md).

The scenarios below show how the requirements work together in Zoe; they are not development steps.

<a id="cy-19-a"></a>

**Disposition: Required.**

### Scenario A: Software Development

**CY-19-A.** Scenario A: voice/text software task, native agent execution, client disconnect, cross-device progress, continuation and safe stop.

A user says through Zoe on a Mac:

“Develop a new login system for me.”

Zoe understands the request and creates a project task ([CY-08-01](#cy-08-01)).

Super plans the work when needed and assigns specific development work to a Codex Worker ([CY-06-01](#cy-06-01), [CY-05-01](#cy-05-01)).

Codex performs the development.

The task system continuously records the actual state ([CY-15-03](#cy-15-03)).

Closing the Zoe client should not interrupt background execution ([CY-04-01](#cy-04-01)).

The user picks up an iPhone and asks about development progress.

Zoe answers from the actual task records ([CY-08-03](#cy-08-03)).

The user then asks to add tests.

The system continues the existing work instead of creating a completely unrelated project ([CY-08-05](#cy-08-05)).

The first release needs to verify persistent task recovery, real stopping, and artifact viewing. The following manual-continuation steps are optional enhancements: the user returns to the Mac, enters the same background execution environment, and opens the official Codex CLI.

If the official CLI supports it, the user finds the original development session and continues work.

Zoe can then recognize the updated work state.

Throughout the process, the project, task, session, and code workspace must remain correctly associated.

<a id="cy-19-b"></a>

**Disposition: Required.**

### Scenario B: Creating a Long-Term Automation Capability

**CY-19-B.** Scenario B: authorized long-running mail capability with deterministic, worker, and Super routing plus notices.

A user says:

“Monitor important email for me from now on.”

Zoe understands this as a long-term service request ([CY-09-01](#cy-09-01)).

Super can coordinate a plan as needed ([CY-06-01](#cy-06-01)).

A Worker implements or configures the email-monitoring capability ([CY-07-01](#cy-07-01), [CY-06-02](#cy-06-02)).

The background service starts after the user authorizes it ([CY-07-02](#cy-07-02), [CY-14-02](#cy-14-02)).

Thereafter, the service applies the routing defined in [Sections 6.3](#63-direct-invocation-of-workers) and [7.3](#73-independent-operation-of-new-capabilities): an ordinary program continuously checks email and applies explicit rules, semantic cases go to a Worker, complex decisions may escalate to Super, and important results are reported through [Section 9.4](#94-proactive-notifications).

**After this capability has been deployed, Super does not need to repeat the entire email-checking process each time.**

<a id="cy-19-c"></a>

**Disposition: Required.**

### Scenario C: Long-Term Project Memory

**CY-19-C.** Scenario C: recover dated project decisions and reasons across devices without inventing missing evidence.

A user discusses a project with Zoe.

At first, they decide to use a particular technical approach.

After development, they change that decision.

Several weeks later, the user asks:

“Why did we move away from that approach?”

Zoe should find the relevant discussion and formal decision using long-term memory and historical records ([CY-10-01](#cy-10-01), [CY-10-04](#cy-10-04)).

Its answer should include the current approach, change process, timing, and rationale ([CY-16-04](#cy-16-04)).

If a rationale was not recorded, Zoe should say that it cannot confirm it rather than make one up ([CY-10-06](#cy-10-06)).

<a id="cy-19-d"></a>

**Disposition: Required.**

### Scenario D: Parallel Work by Multiple Workers

**CY-19-D.** Scenario D: parallel specialist work, conflict coordination, and truthful aggregate outcomes.

A user says:

“Check this project's security and organize its API documentation at the same time.”

Zoe can create the corresponding tasks.

It assigns them to different Workers ([CY-06-02](#cy-06-02)).

The two Workers do not have to wait for Super to direct each individual step in sequence ([CY-06-03](#cy-06-03)).

The system preserves their execution status and results ([CY-06-04](#cy-06-04)).

Super coordinates if there is a resource conflict, a major dependency, or a complex decision ([CY-06-01](#cy-06-01)).

The user receives one consolidated result.

<a id="cy-19-e"></a>

**Disposition: Required.**

### Scenario E: Failure and Recovery

**CY-19-E.** Scenario E: unattended network/API/process failure detection, safe recovery, unknown-effect handling, and records.

A long-running background service fails because of a network interruption.

Zoe should detect the exception ([CY-15-01](#cy-15-01)).

Once the service recovers, it should continue or retry according to [CY-15-02](#cy-15-02).

If automatic recovery would not be safe, it should notify the user to take action ([CY-15-04](#cy-15-04)).

Even if the client was closed during the failure, the related event and failure record should be retained ([CY-04-01](#cy-04-01), [CY-16-01](#cy-16-01)).

## 20. Overall Quality Requirements

All fifteen dimensions below apply to final system acceptance under CY-20-01. Numeric thresholds absent from the requirements remain open targets; a design or static check cannot establish measured quality.

<a id="cy-20-01"></a>

**Disposition: Required.**

Human native-session handoff remains optional under chapter 5; required interoperability preserves native work products and supported execution/recovery.

**CY-20-01.** Meet and measure all fifteen system-quality dimensions; unspecified thresholds remain open targets.

| Dimension | Requirement |
|---|---|
| Accuracy | Do not guess execution results or treat unconfirmed inferences as facts |
| Continuity | Projects, tasks, and long-running services remain after a conversation ends |
| Persistence | Important data and memory are not lost when a process ends |
| Reliability | Detect execution exceptions and recover under safe conditions |
| Extensibility | Add models, Agents, services, and system capabilities |
| Independence | Do not bind core capabilities tightly to one model or service |
| Interoperability | Support two-way continuation with native tools such as official Codex as far as possible |
| Cross-device consistency | Multiple clients access the same authoritative task and memory state |
| Cross-platform support | macOS, iPhone, and iPad clients access a unified backend; server and execution hosts use Linux |
| Controllability | Users can understand, authorize, pause, and stop related work |
| Security | Run with appropriate permissions and protect data and external services |
| Observability | Runtime state, exceptions, results, and their sources are traceable |
| Cost efficiency | Reduce unnecessary model calls and repeated context |
| Ease of use | Make natural interaction primary; do not require users to understand internal technology |
| Long-term evolution | Integrate new capabilities without repeatedly rebuilding the system from scratch |

## 21. Key Boundaries of Zoe

Zoe manages persistent projects, tasks, services, and memory through conversation ([Chapter 1](#1-product-positioning), [Chapter 8](#8-task-management-requirements), [Chapter 10](#10-long-term-memory-requirements)). Super coordinates complex work; Workers and ordinary programs retain independent roles ([Chapter 6](#6-agent-system-requirements), [Chapter 9](#9-long-running-background-services)). Zoe extends execution through authorized capabilities on existing operating systems and specialist tools ([Chapter 7](#7-autonomous-capability-expansion), [Section 1.3](#13-product-boundaries)).

<a id="cy-21-01"></a>

**Disposition: Required.**

**CY-21-01.** Keep Zoe outside the boundaries of chat-only, Super-only, orchestration-only, editor, OS, memory-only, and personal-only products.

Several distinctions need to remain clear to avoid future misunderstandings; their authoritative requirements are linked below.

**First, Zoe is not an ordinary chat assistant.** Conversation is its interaction method; it manages projects, tasks, memories, and execution capabilities that persist over time ([Chapter 1](#1-product-positioning)).

**Second, Zoe is not a centralized Agent that requires Super to direct everything.** Super handles complex coordination, Workers execute independently, and ordinary background services keep running ([Chapter 6](#6-agent-system-requirements), [Chapter 9](#9-long-running-background-services)).

**Third, Zoe is more than an Agent orchestration tool.** It can create, deploy, and manage ordinary programs under authorization ([Chapter 7](#7-autonomous-capability-expansion)).

**Fourth, Zoe is not a new code editor.** It uses native development tools such as Codex and preserves required CLI execution, stopping, viewing, task recovery, and code-artifact access; human takeover and bidirectional handoff remain optional ([Chapter 5](#5-agent-and-runtime-integration-requirements)).

**Fifth, Zoe is not an operating system in the traditional sense.** It builds on existing operating systems rather than replacing macOS or Linux ([Section 1.3](#13-product-boundaries)).

**Sixth, Zoe is more than a memory system.** Long-term memory is foundational, but Zoe also acts on memory, real-world state, and user instructions ([Chapter 10](#10-long-term-memory-requirements)).

**Seventh, Zoe is not limited to a personal product.** It can serve individuals while retaining future boundaries for teams and other scenarios ([Chapter 18](#18-openness-and-scope-for-expansion)).

## 22. Final Complete Product Definition

The complete product is summarized by the authoritative requirements in [Chapter 3](#3-client-requirements),
[Chapter 5](#5-agent-and-runtime-integration-requirements), [Chapter 6](#6-agent-system-requirements),
[Chapter 7](#7-autonomous-capability-expansion), [Chapter 8](#8-task-management-requirements), [Chapter
9](#9-long-running-background-services), [Chapter 10](#10-long-term-memory-requirements), [Chapter
11](#11-context-management), [Chapter 12](#12-unified-state-and-cross-device-continuity), [Chapter
13](#13-modular-ai-and-services), and [Chapters 14–18](#14-permission-and-security-requirements). The user
sets a goal, Super coordinates when needed, Workers execute specialist work, and ordinary programs perform
deterministic continuous work; results enter the unified task/state system and important knowledge enters
controlled memory.

<a id="cy-22-01"></a>

**Disposition: Required.**

**CY-22-01.** Deliver the complete product behavior across interaction, decisions, execution, interoperability, services, memory, state, security, modularity, and evolution.

**Zoe is an open AI system that can run long term, accumulate knowledge, coordinate intelligent execution units, and gain new capabilities under authorization ([CY-01-01](#cy-01-01), [CY-07-01](#cy-07-01)).**

Its complete capabilities are defined by the linked stable requirements:

**Interaction:** Communicate with users through natural speech and text and support continuity across devices ([CY-03-01](#cy-03-01), [CY-03-03](#cy-03-03)).

**Understanding and decision-making:** Super handles complex requirements, planning, and coordination; Workers handle specific specialist tasks and local intelligent decisions ([CY-06-01](#cy-06-01), [CY-06-02](#cy-06-02)).

**Execution:** Invoke tools such as Codex to perform development and other real work, and manage actual task status ([CY-05-01](#cy-05-01), [CY-08-02](#cy-08-02)).

**Native interoperability:** Preserve usable projects/work products and supported runtime session associations. Human continuation in the same official Codex CLI session, reverse native-session discovery, and automatic synchronization of external changes are optional enhancements under chapter 5; they do not gate required-scope acceptance.

**Continuous operation:** Ordinary background programs handle scheduled work, monitoring, event response, and long-running services without requiring AI to repeatedly perform deterministic operations ([CY-07-03](#cy-07-03), [CY-09-01](#cy-09-01), [CY-17-01](#cy-17-01)).

**Autonomous expansion:** Under user authorization, create, deploy, register, and manage new software services so the system gradually gains new functions that remain available ([CY-07-01](#cy-07-01), [CY-07-02](#cy-07-02)).

**Long-term memory:** Store history, project context, formal decisions, task state, and experience, then retrieve and use them reliably and traceably ([CY-10-01](#cy-10-01), [CY-10-04](#cy-10-04)).

**State management:** All clients, Agents, tasks, and background services share one consistent and trustworthy state system ([CY-12-01](#cy-12-01)).

**Security controls:** Different execution principals have clear permissions, and important operations are controlled and traceable ([CY-14-01](#cy-14-01), [CY-14-04](#cy-14-04)).

**Modularity:** AI models, Agents, voice services, and third-party applications can be added, replaced, and upgraded independently ([CY-13-01](#cy-13-01), [CY-13-03](#cy-13-03), [CY-13-04](#cy-13-04)).

**Long-term evolution:** Expand to new scenarios while preserving the core architecture and continuity of existing data ([CY-18-01](#cy-18-01)).

### Zoe's Core Operating Logic

**The user sets a goal; Super performs complex planning and coordination when needed; Workers perform specialist execution; ordinary programs handle work that can run reliably over time.**

All execution results enter a unified task and state system, and important information enters reliable long-term memory.

Users can view, intervene in, or continue work at any time from different devices, and can also continue tasks directly in native tools such as Codex where supported.

As it is used over time, Zoe should gain more than a larger conversation history: it should accumulate knowledge, preserve work products, manage more continuously running services, and gain more reusable practical capabilities.

**Ultimate goal: to make Zoe an AI system that understands and executes instructions into a system that can run long term, accumulate knowledge continuously, expand its capabilities autonomously under control, and coordinate intelligent and non-intelligent programs to do real work.**

---

*This complete product requirements definition was organized from requirements expressed earlier. It does not assume a specific technology framework or describe unverified Codex interoperability as implemented. It does not divide the product into development phases or an MVP; all sections together describe the capabilities the complete Zoe product should have.*
