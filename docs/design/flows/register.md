# Register

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. The register makes one routing call. Request derivation does not expand the user's authority. No additional impact-confirmation or delegation prohibition is introduced.

## Inputs and outputs

Input is the owner's compact context, contiguous recent raw dialogue, and current original utterance. The exact output schema is:

```typescript
type RegisterOutput = {
  route: "chat" | "memory_write" | "recall" | "task" | "task_ui" | "ui" | "chat_input";
  request: string | null;
};
```

## Required behavior

`route` is one string from the seven values above. For `chat`, `request` is null. For every other route, `request` is a nonempty, complete, self-contained statement of the user's entire request, resolving conversational references without adding intent. It is required even when the original utterance was already complete. Literal `none`, omitted context, and fallback to the original utterance are forbidden. There is no one-sentence or verbatim-quotation restriction that could truncate a complete request.

| Route | Semantic responsibility and consumer |
|---|---|
| `chat` | Conversation requiring no execution, screen action or memory operation; end delegated handling without Steward, task admission, memory search, or duplicate Zoe speech. Upstream voice answers conversation. |
| `memory_write` | Save or update personal information; send the full request to the memory worker. |
| `recall` | Retrieve personal memory; send the full request to scoped search. |
| `task` | Perform, modify, answer, continue, inspect or cancel work; send request and candidate facts to Steward. A question about readiness is task only. |
| `task_ui` | Present content that has to be obtained or created first, or an already finished task's result; call Steward with display_requested=true, then UI_Controller. |
| `ui` | Present content that already exists in the user's own words, needing nothing obtained or created; send the full request to UI_Controller without task work. |
| `chat_input` | Show an input box for conversation text, without a task or pending question. |

Display routes (`task_ui`, `ui`, `chat_input`) require the request to state that content is to be shown on the screen; for `chat_input`, specify an input box for typing conversation text.

## Persistence and idempotency

The server retains the register request as the downstream execution input. The original utterance is retained only as register input, provenance, dialogue and asynchronous memory-ingestion source; it is not a second downstream execution input. The server persists request and route, with original provenance separately linked. Classification and selected action/IDs/reply are durable; replay reuses accepted classification.

## Failure and recovery

Invalid/partial output creates no fallback task or memory operation. Classification failure admits nothing; never bypass register/Steward on timeout.

## Implementation locations

[Register](../../../internal/voice/register.go), [handoff preflight](../../../internal/voice/handoff_preflight.go).

## Verification obligations

Strict parsing checks shape and nullability; semantic completeness and fidelity also need model-quality acceptance, not merely parser tests. Verify exact schema/route/nullability, complete contextual requests without `none` or original-input fallback, and semantic fidelity on real contextual turns.

Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
