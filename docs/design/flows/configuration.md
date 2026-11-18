# Model configuration and deployment

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Operator model menu: selected provider/model and effort plus secret references | Probe the proposed configuration, then atomically save/reload the corresponding environment/routes, OpenCode provider/agent configuration and session selection. All Worker layers resolve the same model+effort. |
| Register, Compact, Steward, UI_Controller, memory and Worker consumers | Preserve separate model configurations; changes target the chosen consumer rather than silently changing another or falling back across consumers. |
| Release/deployment manifest, network configuration, key references and persistent data | Deployment retains existing network reachability, keys and data; gateway image-only update does not implicitly reset configuration or state. |

## Required behavior

Never log credentials. Reset and backup are explicit operations; never silently regenerate keys or destroy data to make startup pass. Verify effective runtime/session settings rather than menu success alone.

## Persistence and idempotency

Keep previous known configuration and applied version. Record effective per-consumer configuration identity. Retain immutable release identity, retained configuration/data inventory and explicit backup/rollback references.

## Failure and recovery

Probe/save/reload failure rolls back the whole change, not a partially selected route. A working consumer does not prove another is configured; report truthful failure. Health/startup success is not end-to-end success.

## Implementation locations

[Operator configuration](../../../cmd/zoe/configure.go), [menu/operations](../../../cmd/zoe/operations.go), [runner](../../../internal/runner/engine.go), [deployment](../../../deploy/linux/), [voice deployment](../../../deploy/voice/README.md).

## Verification obligations

Verify probe/save/reload failures with atomic rollback, effective model+effort equality across Worker route/provider/agent/session, consumer separation, retained network/key/data and explicit reset/backup/restore evidence; real end-to-end task/result/voice/UI success separately from health. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
