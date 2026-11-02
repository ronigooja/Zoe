package control

import (
	"context"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// ExpireRuntimeActions expires undecided runtime interactions in one transaction.
// It deliberately leaves the task awaiting_user: the interaction is durable and
// a later user entry may start a fresh run without fabricating an answer.
func (s *Server) ExpireRuntimeActions(ctx context.Context, now time.Time) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.workspace_id,a.task_id,a.run_id,a.kind,a.question FROM pending_runtime_actions a JOIN runs r ON r.id=a.run_id WHERE a.decision='' AND a.expires_at<=? AND r.status IN ('dispatching','active','uncertain')`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	type expired struct{ id, workspace, task, run, kind, question string }
	var actions []expired
	for rows.Next() {
		var a expired
		if err := rows.Scan(&a.id, &a.workspace, &a.task, &a.run, &a.kind, &a.question); err != nil {
			rows.Close()
			return err
		}
		actions = append(actions, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, a := range actions {
		decision := "expired"
		if a.kind == "permission_requested" {
			decision = "reject"
		}
		result, err := tx.ExecContext(ctx, `UPDATE pending_runtime_actions SET decision=? WHERE id=? AND decision=''`, decision, a.id)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			if err != nil {
				return err
			}
			continue
		}
		state := "awaiting_message"
		expiredKind := "question"
		if a.kind == "permission_requested" {
			state = "explicit_confirmation"
			expiredKind = "permission"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE runs SET desired_control='stop',control_version=control_version+CASE WHEN desired_control='stop' THEN 0 ELSE 1 END WHERE id=? AND status IN ('dispatching','active','uncertain')`, a.run); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status='awaiting_user',interaction_state=?,interaction_expired_kind=?,interaction_run_id=?,interaction_question=?,version=version+1,updated_at=? WHERE id=? AND workspace_id=? AND status='active' AND interaction_run_id=?`, state, expiredKind, a.run, a.question, now.UTC().Format(time.RFC3339Nano), a.task, a.workspace, a.run); err != nil {
			return err
		}
		if err = appendEventTx(ctx, tx, contracts.Event{Type: "run.runtime_action_expired", OccurredAt: now.UTC(), WorkspaceID: a.workspace, AggregateType: "run", AggregateID: a.run, Payload: raw(map[string]any{"run_id": a.run, "task_id": a.task, "kind": a.kind, "decision": decision, "question": a.question})}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) StartRuntimeWaitingWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	s.RegisterShutdownHook(cancel)
	go func() {
		ticker := time.NewTicker(s.cfg.WaitSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.ExpireRuntimeActions(ctx, s.cfg.Now().UTC()); err != nil && ctx.Err() == nil { /* next tick retries */
				}
			}
		}
	}()
}
