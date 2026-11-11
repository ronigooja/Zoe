package voice

import (
	"context"
	"database/sql"
	"time"
)

// callRosterTask mirrors steward.RosterTask without importing the steward package here.
type callRosterTask struct {
	ID, Title, Status, Question, Result string
}

type rosterQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// callRoster returns the tasks linked to this voice session/generation for the owner,
// newest first, at most 10. Read-only; it never writes.
func (s *Service) callRoster(ctx context.Context, q rosterQueryer, workspace, owner, session string, generation int64, now time.Time) ([]callRosterTask, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT t.id,coalesce(t.title,''),t.status,
CASE WHEN t.status IN ('waiting','awaiting_user') AND t.interaction_state IN ('awaiting_message','explicit_confirmation') THEN coalesce(t.interaction_question,'') ELSE '' END,
CASE WHEN t.status='complete' THEN coalesce(t.result,'') ELSE '' END
FROM voice_task_links l JOIN tasks t ON l.task_id=t.id AND l.workspace_id=t.workspace_id
WHERE t.workspace_id=? AND t.owner_subject_id=? AND l.session_id=? AND l.generation=?
AND (t.status IN ('queued','active','waiting','awaiting_user') OR
 (t.status='complete' AND julianday(coalesce(t.finished_at,t.updated_at)) BETWEEN julianday(?) AND julianday(?)))
ORDER BY coalesce(t.started_at,t.created_at) DESC LIMIT 10`,
		workspace, owner, session, generation, now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []callRosterTask
	for rows.Next() {
		var task callRosterTask
		if err := rows.Scan(&task.ID, &task.Title, &task.Status, &task.Question, &task.Result); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}
