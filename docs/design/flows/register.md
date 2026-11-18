# Register routing contract

## Purpose and boundary

Part of the [approved design](../data-flows.md), with no source-conformance or delivery claim. Use [evidence and acceptance](../../development.md#evidence-and-acceptance) for verification reporting.

The register makes one routing call for each delegated utterance. It performs no task management or memory search and reads no memory keys/subjects. [Steward](steward.md) selects task handling; [memory](memory.md) owns context coverage and memory operations. Request derivation never expands user authority or adds an impact-confirmation gate.

## Inputs and outputs

Input: owner/workspace compact context, contiguous recent raw original dialogue, and the current original utterance. Output is exactly:

```typescript
type RegisterOutput = {
  routes: Array<"chat" | "memory_write" | "recall" | "task" | "ui">;
  request: string | null;
};
```

`routes` is nonempty and distinct. `chat`, `memory_write`, and `recall` are each exclusive; `task` and `ui` may occur alone or together. For `chat`, `request` is null. For every other route, it is a nonempty, complete, self-contained statement of the user's entire request, resolving conversational references without adding intent. It is required even when the original utterance was already complete.

## Required behavior

| Route | Responsibility and consumer |
|---|---|
| `chat` | Conversation requiring no execution, screen action, or memory operation. End delegated handling without Steward, task admission, memory search, or duplicate Zoe speech; upstream voice answers. |
| `memory_write` | Save/update personal information; send the full request to the memory worker. |
| `recall` | Retrieve personal memory; send the full request to scoped search. |
| `task` | Perform, modify, answer, continue, inspect, cancel, or redisplay work; send request and candidate facts to Steward. |
| `ui` | Present content or collect screen input; send the full request to UI_Controller. With `task`, also set `display_requested` for Steward/task delivery. |

Literal `none`, omitted context, and original-utterance fallback are forbidden. No one-sentence or verbatim-quotation restriction may truncate the complete request. There are no legacy `progress`, `cancel`, `confirm`, `decline`, `unclear`, or `memory_read` register routes.

The retained request is the downstream execution input. The original utterance remains separately linked register input, provenance, dialogue, and asynchronous memory-ingestion source, not a second execution input. See [execution](execution.md) for source commitment and handoff.

Opening voice instructions list available capabilities—execute tasks, search the web, use files/devices, report progress/results, cancel/change tasks, and retrieve personal records/earlier conversations. They do not require delegation or a mandatory first user-rule item. Worker delegation is an [execution policy](execution.md#required-behavior), not a voice-model instruction.

## Persistence and idempotency

Persist routes/request with source identity. Classification and selected Steward action/IDs/reply are durable; replay reuses accepted classification rather than rerouting accepted work. Keep original dialogue available to [asynchronous ingestion](memory.md).

## Failure and recovery

Strict parsing checks exact shape, route constraints, and nullability. Invalid/partial output or unavailable routing admits no fallback task or memory operation. Timeout never bypasses register/Steward. Retain the concise `Processing error.` failure path, not fallback authorization. Disabled routing admits no task; enablement and independent consumers belong to [configuration](configuration.md).

## Implementation locations

[Register](../../../internal/voice/register.go), [handoff preflight](../../../internal/voice/handoff_preflight.go), [voice handlers](../../../internal/voice/handlers.go), [compaction](../../../internal/voice/register_compact.go).

## Verification obligations

Verify exact schema/routes/nullability and complete contextual requests without sentinel/original-input fallback. Parser fixtures do not establish semantic completeness or fidelity: use real contextual turns and capability-only opening instructions. Each delegated utterance adds one register call; a task route adds Steward. [Configuration](configuration.md) preserves existing budgets; approved added latency is not measured latency and cannot guarantee upstream speech has not begun first.
