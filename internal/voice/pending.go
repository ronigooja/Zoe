package voice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

type pendingQuestion struct {
	id, question string
	version      int64
}

type pendingQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// pendingHeld applies only to one recent, owned task in this live generation.
func (s *Service) pendingHeld(ctx context.Context, db pendingQueryer, workspace, owner, session string, generation int64) (pendingQuestion, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id,t.version,t.interaction_question FROM tasks t JOIN voice_task_links l ON l.task_id=t.id AND l.workspace_id=t.workspace_id JOIN voice_sessions v ON v.id=l.session_id AND v.workspace_id=l.workspace_id WHERE t.workspace_id=? AND t.owner_subject_id=? AND v.owner_id=? AND l.session_id=? AND l.generation=? AND v.generation=l.generation AND v.state='active' AND t.status IN ('waiting','awaiting_user') AND t.interaction_state IN ('awaiting_message','explicit_confirmation') AND t.interaction_question<>'' AND t.created_at>=? AND t.created_at>=v.created_at ORDER BY t.created_at DESC LIMIT 2`, workspace, owner, owner, session, generation, s.now().Add(-s.recall.ConfirmationWindow).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return pendingQuestion{}, err
	}
	defer rows.Close()
	var pending pendingQuestion
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&pending.id, &pending.version, &pending.question); err != nil {
			return pendingQuestion{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return pendingQuestion{}, err
	}
	if count != 1 {
		return pendingQuestion{}, nil
	}
	return pending, nil
}

func (s *Service) heldReplyTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p handoffPayload, text string) error {
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?)`, uuid.NewString(), cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, recallPrefix+text, "task_control", at, at, at)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_turns SET handoff_handled=1 WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision)
	return err
}

func (s *Service) pendingControlTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p handoffPayload, category string, server *control.Server) (contracts.CommandResult, []contracts.Event, bool, error) {
	result := contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}
	if category == "cancel" {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT t.id FROM tasks t JOIN voice_task_links l ON l.task_id=t.id AND l.workspace_id=t.workspace_id WHERE t.workspace_id=? AND t.owner_subject_id=? AND l.session_id=? AND l.generation=? AND t.status NOT IN ('complete','failed','canceled') ORDER BY t.created_at DESC,t.id DESC LIMIT 1`, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SessionID, p.Generation).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return result, nil, true, s.heldReplyTx(ctx, tx, cmd, p, "Nothing is running.")
		}
		if err != nil {
			return result, nil, true, err
		}
		r, events, err := server.StopTaskTx(ctx, tx, cmd, id)
		if err == nil {
			err = s.heldReplyTx(ctx, tx, cmd, p, "OK, stopped")
		}
		return r, events, true, err
	}
	var bound pendingQuestion
	err := tx.QueryRowContext(ctx, `SELECT pending_task_id,pending_task_version,pending_question FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&bound.id, &bound.version, &bound.question)
	if err != nil {
		return result, nil, true, err
	}
	if bound.id == "" {
		return result, nil, false, nil
	}
	if category != "confirm" && category != "decline" && category != "unclassified" {
		return result, nil, false, nil
	}
	pending, err := s.pendingHeld(ctx, tx, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SessionID, p.Generation)
	if err != nil {
		return result, nil, true, err
	}
	if pending != bound {
		return result, nil, true, s.heldReplyTx(ctx, tx, cmd, p, "That question is no longer pending. Nothing was changed.")
	}
	if category == "unclassified" {
		// One re-ask per held question, not one per ambiguous utterance.
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_turns WHERE pending_task_id=? AND pending_task_version=? AND handoff_handled=1 AND triage_category='unclassified'`, bound.id, bound.version).Scan(&count); err != nil {
			return result, nil, true, err
		}
		if count == 0 {
			return result, nil, true, s.heldReplyTx(ctx, tx, cmd, p, "Please answer the pending question again. Nothing has been started.")
		}
		_, err = tx.ExecContext(ctx, `UPDATE voice_turns SET handoff_handled=1 WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision)
		return result, nil, true, err
	}
	var r contracts.CommandResult
	var events []contracts.Event
	if category == "decline" {
		r, events, err = server.StopTaskTx(ctx, tx, cmd, bound.id)
		if err == nil {
			err = s.heldReplyTx(ctx, tx, cmd, p, "OK, cancelled")
		}
	} else {
		// The model supplies intent, never the command text or authorization scope.
		r, events, err = server.AppendTaskTx(ctx, tx, cmd, bound.id, "yes, proceed")
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE voice_turns SET resumed_task_id=?,handoff_handled=1 WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, bound.id, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision)
	}
	return r, events, true, err
}
