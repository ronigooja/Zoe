# Model activity monitor

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. A Mac client Settings entry opens an independent live-only Zoe model monitor window. The monitor observes bounded model activity events; it does not provide voice, which remains gateway-owned. It uses the authenticated `/v1/ws` connection shared with [Client MCP](client-mcp.md), under the existing owner authorization of the WebSocket upgrade. Monitor and MCP have separate per-channel demand: opening or closing the monitor must not change MCP subscription/use or its connection demand. It sends no credentials in frames. The transport contract is [Client WebSocket](client-ws.md).

The monitor is observational, not a complete trace. It does not promise true token streaming for models that do not stream, raw provider envelopes, or complete event coverage. It does not change the durable `GET /v1/events` contract. Events must not expose protocol or authorization material or configuration credentials. Existing components do not include arbitrary tool arguments or tool output. Prompt and model text can still contain sensitive information; redaction is best-effort and is not a guarantee that sensitive content is removed.

## Inputs and outputs

The monitor uses channel `model_monitor`. Client frames are:

```json
{"channel":"model_monitor","type":"subscribe","subscription_id":"..."}
{"channel":"model_monitor","type":"unsubscribe","subscription_id":"..."}
```

Server frames are:

```json
{"channel":"model_monitor","type":"subscribed|unsubscribed|event|gap|error","subscription_id":"...","sequence":1,"event":{"component":"...","call_id":"...","time":"RFC3339","type":"request|response|error|tool_call|tool_result","payload":{},"truncated":false,"incomplete":false,"conversation_id":"...","conversation_start":true,"request_messages":[{"role":"user","text":"..."}]},"error":{"code":"..."}}
```

Fields not applicable to a frame type are omitted. `event.payload` is optional JSON. `truncated` means an observation exceeded a capture limit; `incomplete` means the source could not observe all applicable content. Both may apply. `error` carries a code. An accepted subscription is acknowledged with `subscribed` before its events. Event `sequence` starts at 1 for each subscription and increases within it. The client visibly highlights a `gap` and any sequence discontinuity; it must not imply missing events can be recovered.

Conversation metadata is optional across all components. An opaque `conversation_id` is scoped to the component; `conversation_start` appears on a request only when the producer knows it starts without continuing earlier model state. A retry, task change, elapsed time, or matching text does not establish a boundary. Unknown boundaries stay unknown. `request_messages`, when present, is a complete ordered, filtered representation of the observed request messages suitable for exact comparison; it must stay within the existing observation budgets. The monitor never changes model input or makes additional model calls to produce this metadata.

Subscriptions with `include_opencode_context: true` also receive `opencode_context` events. Older clients do not receive them. The host OpenCode plugin observes actual provider sends and replies for sessions bound to current Zoe runs. It publishes ordered instruction and message text, including tool-call and tool-result text only when part of the observed model input or output. It does not publish provider envelopes, tool schemas, credentials, transport headers, generation settings, usage, or binary attachments. Apply credential filtering and the existing Worker text limit. Unsupported or incomplete observations are marked rather than reconstructed from stored session history; a reference to previous context does not imply earlier context was resent.

## Required behavior

Opening the monitor subscribes; closing its window unsubscribes. Minimizing the window does not unsubscribe. Authorization is limited to the authenticated owner's existing WebSocket upgrade scope; no additional scope or credential is introduced. Channel failure does not change voice or the durable event stream.

## Presentation

Overview is the first tab; component tabs follow in a stable order, in two rows, scrolling horizontally when the window is narrow. Selecting a tab does not change its contents.

The OpenCode tab alone shows `opencode_context`, pairing `Request` and `Reply` by call ID. Its events do not appear in Overview or the Worker tab.

The live state (connecting, subscribing, live, idle) is shown in the native window title. The window body shows only states the user must act on or that explain missing data: reconnecting with a gap, ended with Retry, and rejected instance key. Gap markers, server-truncation markers, and the monitor-error and dropped-entry notices remain visible.

Each event card shows one metadata row: `Request` or `Reply` as words (replies left, requests right), the component name as a pill, the time, and a short call ID that is unique among the displayed call IDs; the full call ID is available on hover and to accessibility. Metadata text is selectable for standard copy; the metadata has no copy action. The payload has its own copy action, which copies the complete formatted JSON and shows a brief confirmation only after the copy succeeds.

Mark a known fresh request with a new-conversation separator; continuations stay in that conversation, and unknown starts do not get a separator. Within a known conversation, default to newly appended request messages only when a complete prior visible request is an exact ordered prefix of the current complete request and no gap intervenes. Otherwise show the observed request snapshot. The original observed payload remains available for expansion and copying. Hiding repeated context is not truncation. Show actual truncation and incomplete capture separately; a gap marker is neither and never starts a conversation.

Recall appears in Overview and its own tab. Its request and reply use the same call ID and show the actual [Recall](memory.md) query and the successful retrieval text prepared for voice, subject to the monitor's usual filtering and limits. A successful empty search has an empty reply. Fallback speech is not a retrieval result, and a monitor reply does not acknowledge playback.

Payload display is readable text, not a copy format. Decoded strings keep their characters: quotes and newlines appear as written and literal backslashes are not unescaped. A string whose entire content is a JSON object or array is shown as structure, decoding at most two levels and only texts up to 64 KiB. Other strings are shown as written. Display previews are bounded; the copy action always uses the complete payload.

## Persistence and idempotency

The feed is bounded and live-only: persist no monitor events or subscription history and replay none. The window may retain entries already displayed while that same instance and window remain open, with any gap visible. Closing the window, changing credentials, or changing instance clears private monitor UI data. Subscription identity is not an idempotency or replay mechanism.

## Failure and recovery

On WebSocket disconnect, discard queued transport frames, mark a visible gap, and resubscribe after reconnect while the same window remains open. Ordinary source or writer loss is nonterminal: report a visible gap and continue the subscription if delivery can resume. If the server retires a subscription because bounded delivery fails, it sends `gap` followed by `error` with code `subscription_ended`; that subscription does not automatically restart. The user may retry manually or close and reopen the monitor.

Diagnose monitor loss with bounded, coalesced metadata-only logs. Runner distinguishes queue overflow, upload timeout, network failure, and HTTP rejection; the server distinguishes ingress rejection, source queue overflow, and writer admission drops; the client distinguishes connection interruption, sequence discontinuity, and an explicit server gap. Log fixed reason codes, counts, applicable correlation IDs, and status or expected/received sequence where relevant. Never log observed text, tool content, credentials, or raw messages or transport bodies.

## Implementation locations

The existing WebSocket endpoint and routing are implemented in [client WebSocket control](../../../internal/control/client_ws.go); authentication is enforced by [control auth](../../../internal/control/auth.go). This identifies current transport code only, not a monitor implementation.

## Verification obligations

Verify Settings opens an independent monitor window, subscription acknowledgement precedes events, sequence and gap presentation, unsubscribe on close, reconnect gap and resubscription, bounded live-only behavior, owner-only upgrade authorization, and unchanged voice and durable `/v1/events` behavior. Verify fresh versus unknown boundaries, append-only display and fallback across gaps or changed snapshots, unchanged copy, independent truncation and incomplete markers, and metadata-only loss diagnostics. The UI must not claim token streaming, raw Worker provider requests, or complete coverage. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
