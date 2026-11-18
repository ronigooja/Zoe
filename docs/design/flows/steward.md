# Steward decision contract

## Purpose and boundary

Approved target, 2026-11-16; not implementation or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. Steward decides task handling from the [register request](register.md) and server facts, never executes work or rewrites the request. [Execution](execution.md) owns effects; [results](results.md) and [screen](screen.md) own delivery.

## Inputs and outputs

The server retrieves owner/workspace-scoped task facts, preserving full goals, pending questions, results, and retained additions. Titles, quoted original utterances, and per-call aliases are not selectors. Exact schemas:

```typescript
type StewardInput = {
  request: string;
  display_requested: boolean;
  tasks: Array<{
    task_id: string;
    goal: string;
    status: string;
    pending_questions: Array<{ question_id: string; text: string }>;
    result: string | null;
    additions: string[];
  }>;
};
type StewardOutput = {
  action: "new" | "append" | "answer" | "continue" | "report" |
          "cancel" | "redisplay" | "clarify";
  task_id: string | null;
  question_id: string | null;
  reply: string | null;
};
```

Every key is present; irrelevant fields are null. IDs are real stable server-owned identities from supplied facts. `status` is authoritative; absent `result` is null; additions retain conversational order. Pending-question IDs survive retries and are scoped to the selected task. Steward returns only a decision; the server retains and dispatches the full request.

## Required behavior

Non-null fields below are in addition to `action`; all others are null.

| Action | Semantic choice | Non-null fields | Effect using retained full request |
|---|---|---|---|
| `new` | Independent work not dependent on existing work. | None | Admit task with full request as goal. |
| `append` | Additional direction for existing live work. | `task_id` | Persist ordered addition; deliver to original live session, no replacement work. |
| `answer` | Response to a current pending question. | `task_id`, `question_id` | Bind full request to that exact pending question. |
| `continue` | New work dependent on prior work, not an addition to live execution. | `task_id` | Admit new task with request as goal and structured prior-task context. |
| `report` | Factual state/progress/result/handling of identified work. | `task_id`, `reply` | Deliver fact-grounded reply; do not execute or mutate task. |
| `cancel` | Withdrawal/stopping of identified work. | `task_id` | Apply cancellation, preserving actual termination semantics. |
| `redisplay` | Existing authoritative result presentation without new work. | `task_id` | Send result and request to UI_Controller; invoke no Worker. |
| `clarify` | Insufficiently determined intent or task/question reference. | `reply` | Ask resolving question; admit no work. |

Choose semantically from these definitions and supplied context, not examples, keywords, or newest-task heuristics. Empty candidates do not imply `new`: unresolved intent/references require `clarify`. Ambiguity must not select arbitrarily. A report must identify a real supplied task; no resolvable subject means clarification, not an invented report task. Report/clarify replies are nonempty and follow the request's language, without another answer model. Do not filter long requests out of candidate facts.

## Persistence and idempotency

Retain source identity, request, decision, selected stable IDs/reply, and resulting task/entry/question linkage. Transactionally validate ownership, IDs, eligibility, and current pending state before applying any decision. Stable question and entry identities support [execution checkpoints](execution.md#persistence-and-idempotency).

## Failure and recovery

If facts changed, refresh and re-evaluate selection; never silently convert a stale/rejected/ambiguous decision into new work. Invalid output or unavailable model fails without admission. Existing `Processing error.` behavior may report failure; it is not fallback authorization.

## Implementation locations

[Candidate retrieval](../../../internal/voice/taskcandidates.go), [Steward](../../../internal/steward/choose.go), [task commands](../../../internal/control/core_handlers.go), [handoff](../../../internal/voice/handoff_preflight.go).

## Verification obligations

Exercise every action's required/null fields, duplicate-title tasks with distinct stable IDs, pending-question scoping, semantic ambiguity, empty candidates, and stale-selection refresh. Verify preservation of the full request at every branch, no Worker for redisplay, factual nonmutating reports, clarification language, and actual runtime continuity separately from schema fixtures. Follow [evidence and acceptance](../../development.md#evidence-and-acceptance).
