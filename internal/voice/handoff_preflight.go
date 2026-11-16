package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

func (s *Service) classificationTimeout() time.Duration {
	if s.recall.ClassificationTimeout > 0 {
		return s.recall.ClassificationTimeout
	}
	return 35 * time.Second
}

// prepareHandoff waits without holding the single database connection. On budget
// expiry the command stays retryable rather than inventing task intent.
func (s *Service) prepareHandoff(ctx context.Context, p contracts.Principal, req contracts.CommandRequest) error {
	if !control.HasScope(p, "client:command") && !control.HasScope(p, "voice:turn:commit") {
		return apiError(http.StatusForbidden, "forbidden", "voice handoff scope is required", nil)
	}
	var payload handoffPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return err
	}
	if payload.SessionID == "" || payload.Generation < 1 || payload.TurnID == "" || payload.Revision < 1 || len(payload.ContentHash) != 64 {
		return apiError(http.StatusBadRequest, "invalid_request", "a stable committed turn is required", nil)
	}
	started := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, s.classificationTimeout())
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var id string
	refreshes := 0
	for {
		if waitCtx.Err() != nil {
			return apiError(503, "routing_timeout", "voice routing is not ready; retry this handoff", nil)
		}
		var category, existing string
		var hash []byte
		err := s.db.QueryRowContext(waitCtx, `SELECT v.id,coalesce(v.exits,''),coalesce(l.task_id,i.task_id,nullif(v.resumed_task_id,''),''),v.content_hash FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id LEFT JOIN voice_task_links l ON l.session_id=v.session_id AND l.generation=v.generation AND l.turn_id=v.turn_id LEFT JOIN voice_task_inputs i ON i.session_id=v.session_id AND i.generation=v.generation AND i.turn_id=v.turn_id WHERE v.workspace_id=? AND s.owner_id=? AND s.device_id=? AND v.session_id=? AND v.generation=? AND v.turn_id=? AND v.revision=?`, p.WorkspaceID, p.SubjectID, p.DeviceID, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision).Scan(&id, &category, &existing, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			supplied, decodeErr := hex.DecodeString(payload.ContentHash)
			if decodeErr != nil || !bytes.Equal(hash, supplied) {
				return apiError(409, "turn_content_conflict", "handoff hash does not match committed content", nil)
			}
			if existing != "" {
				return nil
			}
		}
		if err == nil && category == "" {
			s.classifyCommittedTurn(id)
			select {
			case <-waitCtx.Done():
				return apiError(503, "routing_timeout", "voice routing is not ready; retry this handoff", nil)
			case <-ticker.C:
				continue
			}
		}
		if err == nil && (category != "" || existing != "") {
			if existing == "" && (RouteResult{Routes: strings.Split(category, ",")}).Has("task") {
				var accepted string
				var handled int
				if err := s.db.QueryRowContext(waitCtx, `SELECT steward_snapshot,handoff_handled FROM voice_turns WHERE id=?`, id).Scan(&accepted, &handled); err != nil {
					return err
				}
				current, err := stewardSnapshot(waitCtx, s.db, p.WorkspaceID, p.SubjectID)
				if err != nil {
					return err
				}
				if handled == 0 && (accepted == "" || accepted != current) {
					if refreshes >= 3 {
						return apiError(409, "selection_stale", "task facts keep changing; retry handoff", nil)
					}
					refreshes++
					if _, err := s.db.ExecContext(waitCtx, `UPDATE voice_turns SET steward_snapshot='',steward_action='',steward_task_id='',steward_question_id='',steward_task_version=0,report_text='' WHERE id=? AND handoff_handled=0`, id); err != nil {
						return err
					}
					s.classifyCommittedTurn(id)
					continue
				}
			}
			logging.Infof("voice_classification_wait id=%s wait_ms=%d budget_hit=false", id, time.Since(started).Milliseconds())
			return nil
		}
		if err != nil && waitCtx.Err() == nil {
			return err
		}
		select {
		case <-waitCtx.Done():
			logging.Warnf("voice_routing_wait id=%s wait_ms=%d budget_hit=true reason=routing_timeout", id, time.Since(started).Milliseconds())
			return apiError(http.StatusServiceUnavailable, "routing_timeout", "voice routing is not ready; retry this handoff", nil)
		case <-ticker.C:
		}
	}
}
