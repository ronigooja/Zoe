package voice

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/internal/control"
)

func (s *Service) markHandledTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p delegationIdentity) error {
	_, err := tx.ExecContext(ctx, `UPDATE voice_turns SET handoff_handled=1 WHERE session_id=? AND generation=? AND turn_id=? AND revision=?`, p.SessionID, p.Generation, p.TurnID, p.Revision)
	return err
}

func (s *Service) heldReplyTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p delegationIdentity, text string) error {
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',?,?,?)`, uuid.NewString(), p.SessionID, p.Generation, p.TurnID, text, "task_control", at, at, at)
	if err != nil {
		return err
	}
	return s.markHandledTx(ctx, tx, cmd, p)
}

// stopStateReplyTx applies after the normal stop path and reads its persisted outcome.
func (s *Service) stopStateReplyTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p delegationIdentity, id string) error {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=? AND owner_subject_id=?`, id, cmd.Principal.SubjectID).Scan(&status); err != nil {
		return err
	}
	text := "已经请求停止，正在停下。"
	switch status {
	case "canceled":
		text = "任务已经停止。"
	case "complete":
		text = "任务已经完成。"
	case "failed":
		text = "任务已经失败。"
	}
	return s.heldReplyTx(ctx, tx, cmd, p, text)
}
