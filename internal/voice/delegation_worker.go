package voice

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

const delegationPollInterval = 250 * time.Millisecond

var (
	errDelegationNotReady = errors.New("delegation classification is not durable")
	errStaleSelection     = errors.New("steward selection is stale")
)

func delegationKey(session string, generation int64, turn string, revision int64) string {
	identity, _ := json.Marshal([]any{session, generation, turn, revision})
	return fmt.Sprintf("voice-delegation:%x", sha256.Sum256(identity))
}

func (s *Service) delegationWorker(ctx context.Context) {
	ticker := time.NewTicker(delegationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processDelegations(ctx)
		}
	}
}

func (s *Service) processDelegations(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM voice_turns WHERE handoff_handled=0 AND transcript_seq IS NOT NULL ORDER BY confirmed_at,id LIMIT 25`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.processDelegation(ctx, id)
	}
}

func (s *Service) processDelegation(ctx context.Context, id string) {
	if !s.claimClassification(id) {
		return
	}
	defer s.classifying.Delete(id)
	const maxRefreshes = 3
	for refreshes := 0; ctx.Err() == nil; refreshes++ {
		s.classifyDelegation(ctx, id)
		err := s.applyDelegation(ctx, id)
		if err == nil || errors.Is(err, errDelegationNotReady) || ctx.Err() != nil {
			return
		}
		var api *control.APIError
		switch {
		case errors.Is(err, errStaleSelection) && refreshes < maxRefreshes:
			// The applying transaction rolled back. Keep Register's accepted
			// request; only invalidate the selection before calling Steward again.
			_, err = s.db.ExecContext(ctx, `UPDATE voice_turns SET steward_snapshot='',steward_action='',steward_task_id='',steward_question_id='',steward_task_version=0,report_text='' WHERE id=? AND handoff_handled=0`, id)
			if err == nil {
				continue
			}
		case errors.Is(err, errStaleSelection), errors.As(err, &api) && api.Status >= 400 && api.Status < 500:
			// Fail closed after a bounded refresh loop or invalid decision. No
			// part of the rejected decision survives the applying transaction.
			logging.Warnf("voice_delegation_held id=%s stale=%t", id, errors.Is(err, errStaleSelection))
			if _, err = s.db.ExecContext(ctx, `UPDATE voice_turns SET steward_action='error',steward_task_id='',steward_question_id='',steward_task_version=0,report_text='',steward_snapshot='' WHERE id=? AND handoff_handled=0`, id); err == nil {
				err = s.applyDelegation(ctx, id)
			}
		}
		if err != nil {
			logging.Warnf("voice_delegation_apply_pending id=%s", id)
		}
		return
	}
}

func (s *Service) claimClassification(id string) bool {
	if _, loaded := s.classifying.LoadOrStore(id, struct{}{}); loaded {
		logging.Infof("classify_in_flight turn=%s", id)
		return false
	}
	return true
}

func (s *Service) applyDelegation(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var p delegationIdentity
	cmd := control.CommandContext{AcceptedAt: s.now().UTC()}
	// Read the actor from the original bound session in the applying transaction.
	// Do not look up a current call, and don't require the original to remain active.
	err = tx.QueryRowContext(ctx, `SELECT v.workspace_id,s.owner_id,s.device_id,v.session_id,v.generation,v.turn_id,v.revision
 FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id AND s.workspace_id=v.workspace_id AND s.generation=v.generation
 WHERE v.id=? AND v.handoff_handled=0 AND v.transcript_seq IS NOT NULL`, id).Scan(&cmd.Principal.WorkspaceID, &cmd.Principal.SubjectID, &cmd.Principal.DeviceID, &p.SessionID, &p.Generation, &p.TurnID, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cmd.CommandID = delegationKey(p.SessionID, p.Generation, p.TurnID, p.Revision)
	_, events, err := s.applyDelegationOutcomeTx(ctx, tx, cmd, p, id, s.uiEventServer)
	var oversized *oversizedDelegation
	if err != nil && !errors.As(err, &oversized) {
		return err
	}
	if len(events) != 0 {
		if err = s.uiEventServer.AppendEventsTx(ctx, tx, cmd.Principal, events); err != nil {
			return err
		}
	}
	if err = s.markHandledTx(ctx, tx, cmd, p); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if oversized != nil {
		// Size rejection happens before task writes. Log only after the turn is durably handled.
		logging.Errorf("voice_delegation_task_text_too_large id=%s request_bytes=%d limit_bytes=%d", id, oversized.requestBytes, control.MaxTaskTextBytes)
	}
	return nil
}
