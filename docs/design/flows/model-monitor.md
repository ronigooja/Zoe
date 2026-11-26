# Model activity monitor

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. A Mac client Settings entry opens an independent live-only Zoe model monitor window. The monitor observes bounded model activity events; it does not provide voice, which remains gateway-owned. It uses the authenticated `/v1/ws` connection shared with [Client MCP](client-mcp.md), under the existing owner authorization of the WebSocket upgrade. It sends no credentials in frames. The transport contract is [Client WebSocket](client-ws.md).

The monitor is observational, not a complete trace. It does not promise true token streaming for models that do not stream, raw provider requests for Worker/OpenCode, or complete event coverage. It does not change the durable `GET /v1/events` contract.

## Wire contract

The monitor uses channel `model_monitor`. Client frames are:

```json
{"channel":"model_monitor","type":"subscribe","subscription_id":"..."}
{"channel":"model_monitor","type":"unsubscribe","subscription_id":"..."}
```

Server frames are:

```json
{"channel":"model_monitor","type":"subscribed|unsubscribed|event|gap|error","subscription_id":"...","sequence":1,"event":{"component":"...","call_id":"...","time":"RFC3339","type":"request|response|error|tool_call|tool_result","payload":{},"truncated":false},"error":{"code":"..."}}
```

Fields not applicable to a frame type are omitted. `event.payload` is optional JSON and may be truncated; `event.truncated` indicates truncation when present. `error` carries a code. An accepted subscription is acknowledged with `subscribed` before its events. Event `sequence` starts at 1 for each subscription and increases within it. The client visibly highlights a `gap` and any sequence discontinuity; it must not imply missing events can be recovered.

## Required behavior

Opening the monitor subscribes; closing its window unsubscribes. Reconnecting marks a visible gap and resubscribes, with no replay of disconnected events. The feed and any buffering are bounded and live-only: no persistence, history, or replay. Authorization is limited to the authenticated owner's existing WebSocket upgrade scope; no additional scope or credential is introduced. Channel failure does not change voice or the durable event stream.

## Persistence and recovery

Persist no monitor events or subscription history. On connection loss, discard transient buffered data, mark the gap, and resubscribe after reconnect while the window remains open.

## Implementation locations

The existing WebSocket endpoint and routing are implemented in [client WebSocket control](../../../internal/control/client_ws.go); authentication is enforced by [control auth](../../../internal/control/auth.go). This identifies current transport code only, not a monitor implementation.

## Verification obligations

Verify Settings opens an independent monitor window, subscription acknowledgement precedes events, sequence and gap presentation, unsubscribe on close, reconnect gap and resubscription, bounded live-only behavior, owner-only upgrade authorization, and unchanged voice and durable `/v1/events` behavior. Verify payload truncation is indicated and the UI does not claim token streaming, raw Worker/OpenCode provider requests, or complete coverage. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
