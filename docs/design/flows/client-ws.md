# Client WebSocket transport

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. One authenticated WebSocket between a client and its Zoe instance carries frames for independent channels. The transport delivers frames and detects a dead connection; it makes no delivery, ordering-across-reconnect, replay or persistence promise. Each channel defines its own semantics. [Client MCP](client-mcp.md) is the first channel.

This transport does not replace SSE `GET /v1/events`, `POST /v1/commands` or the voice paths, whose contracts are unchanged. Moving any of them here needs its own approved contract change.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Client `GET /v1/ws` (instance bearer, WebSocket upgrade) | Zoe validates the bearer as for other client routes, then upgrades. At most one connection per device, the device of the validated principal: a newer connection from the same device replaces the older, which is closed. An instance can have several devices connected. |
| Frame: JSON object with required string `channel`, other fields owned by that channel | Receiver routes by `channel` to its handler. A frame with an unknown or unsupported `channel` is dropped and answered with a `transport.error` frame `{code:"unknown_channel", channel}`. |
| Channel need: a channel asks the client to hold the connection | The client opens the connection while at least one channel needs it and closes it when none does. |
| Ping/pong | Both sides send them. |

## Required behavior

The connection authenticates once at upgrade. Frames carry no credentials. Channels never share state through the transport, and one channel's failure does not close the connection.

Both sides close a connection that has been silent for the [heartbeat limit](configuration.md#zoe-limits). A frame above the [frame limit](configuration.md#zoe-limits) closes the connection with WebSocket close code 1009; senders check the limit first and let the owning channel fail the operation instead of sending.

Connection loss, replacement, server restart and client exit are reported to every channel as a closed connection. What this means for in-flight work is the channel's rule, not the transport's.

## Persistence and idempotency

The transport persists nothing and assigns no frame identity. Channels that need identity, deduplication or durability define them.

## Failure and recovery

While any channel needs the connection, the client reconnects with exponential backoff and tells each channel when the connection is open again. Reconnect restores no frames. A rejected upgrade (`401`) stops reconnection until credentials change; other failures retry.

## Implementation locations

The existing endpoint, connection lifecycle and frame routing are in [client WebSocket control](../../../internal/control/client_ws.go); client MCP channel handling is in [client MCP control](../../../internal/control/client_mcp.go), with upgrade authentication in [control auth](../../../internal/control/auth.go). These are current transport/channel locations, not evidence that every approved channel is implemented. The [Model monitor](model-monitor.md) defines its own channel contract.

## Verification obligations

Verify bearer validation at upgrade, per-device single-connection replacement, channel routing and the unknown-channel response, heartbeat close, oversize close, reconnect backoff, stopping on `401`, and that closing is reported to all channels. Verify the transport holds no state across reconnect. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
