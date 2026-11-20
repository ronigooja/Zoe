# Steward

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Steward returns a decision, never echoes or rewrites the request. The server retains and dispatches that full request.

## Inputs and outputs

The server retrieves owner/workspace-scoped task facts, preserving full goals, pending questions and retained additions, but sending only the stored result summary. Titles, quoted original utterances and per-call aliases are not selectors. The exact schemas are:

```typescript
type StewardInput = {
  request: string;
  display_requested: boolean;
  tasks: Array<{
    task_id: string;
    goal: string;
    status: string;
    pending_questions: Array<{ question_id: string; text: string }>;
    summary_result: string | null;
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

Every key is present; irrelevant fields are null. IDs are real stable server-owned identities from the supplied facts. `status` reflects authoritative task state; `summary_result` is copied directly from the task column, with no fallback to the full result, preserving null and empty values. Additions remain in conversational order. See [result summaries](results.md#task-result-summaries) for generation and storage.

## Required behavior

| Action | Semantic choice | Required non-null output beyond action | Server effect using retained request |
|---|---|---|---|
| `new` | Independent work not dependent on an existing task. | None | Admit a new task whose goal is the full request. |
| `append` | Additional direction for existing live work. | task_id | Persist the full request as an ordered addition and deliver to the original live session, without replacement work. |
| `answer` | Response to a currently pending question of existing work. | task_id, question_id | Bind the full request as the answer to that exact pending question. |
| `continue` | New work dependent on prior work rather than an addition to its live execution. | task_id | Admit a new task with the full request as goal and structured prior-task context. |
| `report` | Request for factual state, progress, result or handling of identified work. | task_id, reply | Deliver the fact-grounded reply; do not execute or mutate the task. |
| `cancel` | Withdrawal or stopping of identified work. | task_id | Apply cancellation to that task, preserving actual termination semantics. |
| `redisplay` | Presentation of an existing authoritative result with no new work. | task_id | Send that result and request to UI_Controller; invoke no Worker. |
| `clarify` | Intent or task/question reference is insufficiently determined. | reply | Ask the resolving question without admitting work. |

Steward chooses semantically using these definitions and supplied context, not examples, keyword rules or newest-task heuristics. An empty candidate set does not imply `new`: unresolved references or intent require `clarify`. Ambiguity requires clarification instead of arbitrary selection. A report must identify a real supplied task; absence of a resolvable subject uses clarification, not an invented report task. Replies for report/clarify are nonempty and follow the request's language.

## Persistence and idempotency

Pending-question IDs survive retries and are scoped to the selected task. Keep source identity, decision, selected stable IDs and resulting task/entry/question linkage. Before applying a decision, the server validates ownership, selected IDs, eligibility and current pending state transactionally.

## Failure and recovery

If facts changed, refresh and re-evaluate the selection; never silently convert it into new work. Invalid model output or an unavailable model fails without admitting work. Existing processing-error behavior may report failure; it is not fallback authorization.

## Implementation locations

[Candidate retrieval](../../../internal/voice/taskcandidates.go), [Steward](../../../internal/steward/choose.go), [task commands](../../../internal/control/core_handlers.go).

## Verification obligations

Verify every action's required/null fields, stable duplicate-title task IDs and pending-question IDs, semantic ambiguity/empty candidates, stale-selection re-evaluation, and preserved full request at all execution branches. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
