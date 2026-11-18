# Register

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. The register makes one routing call, without task management or memory search. Request derivation does not expand the user's authority. No additional impact-confirmation or delegation prohibition is introduced.

## Inputs and outputs

Input is the owner's compact context, contiguous recent raw dialogue, and current original utterance. The exact output schema is:

```typescript
type RegisterOutput = {
  routes: Array<"chat" | "memory_write" | "recall" | "task" | "ui">;
  request: string | null;
};
```

## Required behavior

`routes` is nonempty and distinct. `chat`, `memory_write`, and `recall` are each exclusive; `task` and `ui` may occur alone or together. For `chat`, `request` is null. For every other route, `request` is a nonempty, complete, self-contained statement of the user's entire request, resolving conversational references without adding intent. It is required even when the original utterance was already complete. Literal `none`, omitted context, and fallback to the original utterance are forbidden. There is no one-sentence or verbatim-quotation restriction that could truncate a complete request.

| Route | Semantic responsibility and consumer |
|---|---|
| `chat` | Conversation requiring no execution, screen action or memory operation; end delegated handling without Steward, task admission, memory search, or duplicate Zoe speech. Upstream voice answers conversation. |
| `memory_write` | Save or update personal information; send the full request to the memory worker. |
| `recall` | Retrieve personal memory; send the full request to scoped search. |
| `task` | Perform, modify, answer, continue, inspect, cancel or redisplay work; send the request and candidate facts to Steward. |
| `ui` | Present content or collect input on the screen; send the full request to UI_Controller. With `task`, also set display_requested for Steward/task delivery. |

## Persistence and idempotency

The server retains the register request as the downstream execution input. The original utterance is retained only as register input, provenance, dialogue and asynchronous memory-ingestion source; it is not a second downstream execution input. The server persists request and routes, with original provenance separately linked. Classification and selected action/IDs/reply are durable; replay reuses accepted classification.

## Failure and recovery

Invalid/partial output creates no fallback task or memory operation. Classification failure admits nothing; never bypass register/Steward on timeout.

## Implementation locations

[Register](../../../internal/voice/register.go), [handoff preflight](../../../internal/voice/handoff_preflight.go).

## Verification obligations

Strict parsing checks shape and nullability; semantic completeness and fidelity also need model-quality acceptance, not merely parser tests. Verify exact schema/routes/nullability, complete contextual requests without `none` or original-input fallback, and semantic fidelity on real contextual turns.

Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
