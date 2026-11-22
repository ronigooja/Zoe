package control

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// TaskInput applies to task additions and exact pending-question answers.
// A nil QuestionID is additional direction and never resolves an interaction.
type TaskInput struct {
	Content    string  `json:"content"`
	QuestionID *string `json:"question_id"`
}

func (s *Server) AppendTaskInputTx(ctx context.Context, tx *sql.Tx, c CommandContext, taskID string, input TaskInput) (contracts.CommandResult, []contracts.Event, error) {
	if taskID == "" || strings.TrimSpace(input.Content) == "" || len(input.Content) > s.cfg.Limits.Int("task_text_max_bytes") {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "task_id and bounded content are required", nil)
	}
	version, status, err := taskVersionStatus(ctx, tx, c.Principal, taskID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if status != "queued" && status != "active" && status != "waiting" && status != "awaiting_user" {
		return contracts.CommandResult{}, nil, apiError(409, "state_conflict", "task is not live", nil)
	}
	var runID string
	if input.QuestionID != nil {
		err = tx.QueryRowContext(ctx, `SELECT a.run_id FROM pending_runtime_actions a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id JOIN runs r ON r.id=a.run_id AND r.task_id=t.id AND r.workspace_id=t.workspace_id WHERE a.id=? AND a.task_id=? AND a.workspace_id=? AND t.owner_subject_id=? AND t.status IN ('waiting','awaiting_user') AND t.interaction_state='awaiting_message' AND t.interaction_run_id=a.run_id AND t.interaction_action_digest=a.action_digest AND a.decision='' AND a.authorization_used=0 AND julianday(a.expires_at)>julianday(?) AND r.fence=a.fence AND r.status IN ('dispatching','active','uncertain','stopped') AND NOT EXISTS(SELECT 1 FROM task_question_answers b WHERE b.question_id=a.id)`, *input.QuestionID, taskID, c.Principal.WorkspaceID, c.Principal.SubjectID, c.AcceptedAt.UTC().Format(time.RFC3339Nano)).Scan(&runID)
		if err == sql.ErrNoRows {
			return contracts.CommandResult{}, nil, apiError(409, "question_stale", "pending question is unavailable", nil)
		}
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	id, err := randomID()
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	at := c.AcceptedAt.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,?,'user_message',?,?,?)`, id, taskID, c.Principal.WorkspaceID, input.Content, c.Principal.DeviceID, at); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if input.QuestionID != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_question_answers(entry_id,workspace_id,task_id,question_id) VALUES(?,?,?,?)`, id, c.Principal.WorkspaceID, taskID, *input.QuestionID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE pending_runtime_actions SET decision='answer',message=? WHERE id=? AND task_id=? AND workspace_id=?`, input.Content, *input.QuestionID, taskID, c.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		var live bool
		if err = tx.QueryRowContext(ctx, `SELECT status IN ('dispatching','active','uncertain') FROM runs WHERE id=?`, runID).Scan(&live); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if !live {
			status = "queued"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET interaction_state='none',interaction_question='' WHERE id=? AND workspace_id=?`, taskID, c.Principal.WorkspaceID); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	version++
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET version=?,status=?,updated_at=? WHERE id=? AND workspace_id=?`, version, status, at, taskID, c.Principal.WorkspaceID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	entry := contracts.TaskEntry{ID: id, Kind: "user_message", Content: input.Content, AuthorDeviceID: c.Principal.DeviceID, CreatedAt: c.AcceptedAt}
	return contracts.CommandResult{ResourceType: "task", ResourceID: taskID, ResourceVersion: version}, []contracts.Event{event("task.entry_appended", "task", taskID, version, map[string]any{"task_id": taskID, "entry": entry, "question_id": input.QuestionID, "status": status})}, nil
}
