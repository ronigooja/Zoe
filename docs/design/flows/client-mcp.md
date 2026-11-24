# Client MCP relay

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. A Worker calls tools of stdio MCP servers that run on the user's Mac client. Frames travel on the `mcp` channel of the [client WebSocket](client-ws.md), which owns the endpoint, authentication, heartbeat, frame limit and reconnection. The relay is transparent: it does not interpret tool arguments or results. Existing SSE, `POST /v1/commands` and voice contracts are unchanged.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Client local registry entry: `mcp_id`, command, arguments, environment references, launch options | Client only. The registry is never sent to or edited by the server. |
| Client per-MCP switch and auto-off timer | Client starts or terminates that MCP's child process. The timer turns the switch off after the configured idle time. While any MCP switch is on, the channel needs the connection. |
| Client frame `{channel:"mcp", type:"state"}`: enabled `mcp_id` list and each MCP's `tools/list` result | Server records it as the live tool catalog for the instance. Each change replaces the whole catalog. |
| Worker `tools/call` on the fixed `zoe-client` MCP entry | Server sends `{channel:"mcp", type:"call", request_id, mcp_id, jsonrpc}`; the client forwards the JSON-RPC to that MCP's child process and returns `{channel:"mcp", type:"result", request_id, ok, result \| error}`. |
| Server `zoe-client` `tools/list` | Union of the live catalog, each tool named `<mcp_id>_<tool>`. A catalog change sends `tools/list_changed`. |

## Required behavior

OpenCode registers one fixed local MCP entry, `zoe-client`, provided by Zoe. Adding another client MCP needs no server configuration change.

The server never supplies a command, argument or path for the client to run; it can only name an `mcp_id` already in the client registry. An unknown or disabled `mcp_id` fails immediately with `mcp_unavailable`.

Each MCP has its own switch, default off, and its own auto-off timer, shown beside the switch. Turning a switch off or letting its timer expire terminates the child process and fails that MCP's pending requests with `mcp_disabled`. Other MCPs are unaffected. Any `tools/call` for that MCP resets its idle timer. Client settings own the timer, with a default of 30 minutes; it is not a server limit.

Launch options belong to the registry entry and are set only by the user in client settings, never by the model or server. Changing one restarts that child process: pending requests fail with `mcp_restarted` and refs or sessions the MCP issued before the restart are invalid. For `agent-desktop mcp`, the option `headed` defaults to off; the process starts without `--headed`, so actions are semantic and do not move the user's pointer or focus. With it on, `--headed` is added and click, type, clear and scroll try physical input first.

Calls are requests, not events. There is no replay, cursor or resume: when the transport reports a closed connection, every pending request fails with `client_disconnected`, and nothing is re-sent after reconnect. Requests to one MCP run serially in arrival order; different MCPs run independently. The client ignores a repeated `request_id`, returning the stored outcome if complete. The server fails a request after the [call timeout](configuration.md#zoe-limits) with `mcp_timeout` and sends a cancel frame; the client then stops waiting but cannot undo an effect already applied. A call or result that would exceed the transport frame limit is not sent; the call fails with `frame_too_large`.

## Persistence and idempotency

Relay state is in memory only. Neither side persists requests, results, catalogs or frames. A Worker result that depended on a failed relay call must state the interruption; failure is never reported as success. Whether to retry a failed call is the Worker's decision after observing the current state.

## Failure and recovery

When the transport reports the connection open again, the client re-sends its `state` frame; pending requests are not recovered. A tool catalog change during a Worker session is announced with `tools/list_changed`; if the Worker runtime does not refresh its tool list, a newly enabled MCP is visible only to later sessions. Calls are validated against the live catalog on every request.

macOS Accessibility and Screen Recording permission belongs to the client app that launches the child process. The client checks it before enabling a desktop MCP and guides the user if missing. A child process that cannot start or exits unexpectedly turns that MCP off and fails its pending requests with `mcp_exited`.

## Implementation locations

None yet.

## Verification obligations

Verify that the server cannot cause an unregistered command to run. Verify catalog build, name prefixing, `tools/list_changed`, and calls to unknown, disabled and restarted MCPs. Verify per-MCP switch and idle timer, child termination, and that other MCPs stay running. Verify connection loss, timeout and cancel fail pending requests without replay, duplicate `request_id` handling, serial order and oversized calls and results. Verify the `headed` option's restart and default, and a real call to `agent-desktop mcp` on a Mac with granted permissions. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
