package voice

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func (s *Service) classificationTimeout() time.Duration {
	if s.recall.ClassificationTimeout > 0 {
		return s.recall.ClassificationTimeout
	}
	return 4 * time.Second
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
	for {
		var category, existing string
		err := s.db.QueryRowContext(waitCtx, `SELECT v.id,coalesce(v.triage_category,''),coalesce(l.task_id,nullif(v.resumed_task_id,''),'') FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id LEFT JOIN voice_task_links l ON l.session_id=v.session_id AND l.generation=v.generation AND l.turn_id=v.turn_id WHERE v.workspace_id=? AND s.owner_id=? AND s.device_id=? AND v.session_id=? AND v.generation=? AND v.turn_id=? AND v.revision=?`, p.WorkspaceID, p.SubjectID, p.DeviceID, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision).Scan(&id, &category, &existing)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil && (category != "" || existing != "") {
			log.Printf("voice_classification_wait id=%s wait_ms=%d budget_hit=false", id, time.Since(started).Milliseconds())
			return nil
		}
		if err != nil && waitCtx.Err() == nil {
			return err
		}
		select {
		case <-waitCtx.Done():
			log.Printf("voice_classification_wait id=%s wait_ms=%d budget_hit=true reason=triage_timeout", id, time.Since(started).Milliseconds())
			return apiError(http.StatusServiceUnavailable, "triage_timeout", "voice classification is not ready; retry this handoff", nil)
		case <-ticker.C:
		}
	}
}
