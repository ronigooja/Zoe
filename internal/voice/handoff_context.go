package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// priorTaskContext applies to continuation admission; it is never goal text.
type priorTaskContext struct {
	TaskID    string   `json:"task_id"`
	Goal      string   `json:"goal"`
	Status    string   `json:"status"`
	Result    *string  `json:"result"`
	Additions []string `json:"additions"`
}

func pointerText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func liveTaskStatus(status string) bool {
	return status == "queued" || status == "active" || status == "waiting" || status == "awaiting_user"
}

func readPriorTaskContext(ctx context.Context, tx *sql.Tx, p contracts.Principal, id string) (*priorTaskContext, int64, error) {
	prior := &priorTaskContext{TaskID: id, Additions: make([]string, 0)}
	var result sql.NullString
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT goal,status,result,version FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND id=?`, p.WorkspaceID, p.SubjectID, id).Scan(&prior.Goal, &prior.Status, &result, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, apiError(409, "selection_stale", "selected task is unavailable", nil)
	}
	if err != nil {
		return nil, 0, err
	}
	if result.Valid {
		prior.Result = &result.String
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.content FROM task_entries e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.workspace_id=? AND e.task_id=? AND e.kind='user_message' AND julianday(e.created_at)>julianday(t.created_at) ORDER BY julianday(e.created_at),e.id`, p.WorkspaceID, id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var addition string
		if err := rows.Scan(&addition); err != nil {
			return nil, 0, err
		}
		prior.Additions = append(prior.Additions, addition)
	}
	return prior, version, rows.Err()
}

func validatePendingQuestionTx(ctx context.Context, tx *sql.Tx, p contracts.Principal, taskID, questionID string, now time.Time) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_runtime_actions a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE a.workspace_id=? AND t.owner_subject_id=? AND a.task_id=? AND a.id=? AND t.status IN ('waiting','awaiting_user') AND t.interaction_state='awaiting_message' AND a.run_id=t.interaction_run_id AND a.action_digest=t.interaction_action_digest AND a.decision='' AND a.authorization_used=0 AND julianday(a.expires_at)>julianday(?) AND NOT EXISTS(SELECT 1 FROM task_question_answers b WHERE b.question_id=a.id))`, p.WorkspaceID, p.SubjectID, taskID, questionID, now.UTC().Format(time.RFC3339Nano)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return apiError(409, "selection_stale", "selected pending question is unavailable", nil)
	}
	return nil
}

func appendedEntryID(events []contracts.Event) string {
	for _, event := range events {
		if event.Type != "task.entry_appended" {
			continue
		}
		var payload struct {
			Entry contracts.TaskEntry `json:"entry"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil {
			return payload.Entry.ID
		}
	}
	return ""
}

func (s *Service) staleDecisionTx(context.Context, *sql.Tx, string, handoffPayload) error {
	return apiError(409, "selection_stale", "task facts changed; retry handoff to refresh selection", nil)
}
