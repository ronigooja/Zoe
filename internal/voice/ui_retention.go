package voice

import (
	"context"
	"database/sql"
	"time"
)

// pruneUISurfacesTx removes answered, expired and superseded input documents.
// Unacknowledged non-input documents remain recoverable, including while offline.
func pruneUISurfacesTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM ui_surfaces WHERE EXISTS (
SELECT 1 FROM ui_requests r WHERE r.id=ui_surfaces.request_id AND (
r.status='input_received' OR
(r.question_id IS NOT NULL AND NOT EXISTS (
SELECT 1 FROM pending_runtime_actions a JOIN tasks t ON t.id=a.task_id
WHERE a.id=r.question_id AND t.id=r.task_id AND t.owner_subject_id=r.owner_subject_id
AND t.interaction_state='awaiting_message' AND t.interaction_run_id=a.run_id AND t.interaction_action_digest=a.action_digest
AND a.decision='' AND a.authorization_used=0 AND julianday(a.expires_at)>julianday(?)
AND NOT EXISTS(SELECT 1 FROM task_question_answers b WHERE b.question_id=a.id)))))`, now.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Service) pruneUISurfaces(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = pruneUISurfacesTx(ctx, tx, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func removeAcknowledgedUISurface(ctx context.Context, tx *sql.Tx, request string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM ui_surfaces WHERE request_id=? AND EXISTS(SELECT 1 FROM ui_requests r WHERE r.id=ui_surfaces.request_id AND r.task_question=0 AND (r.chat_input=0 OR r.status='failed') AND r.question_id IS NULL)`, request)
	return err
}
