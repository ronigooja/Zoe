package voice

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

// recoverySnapshot is the complete checkpoint contract, not a UI-only repair.
// All fields are read from one SQLite snapshot, with no upstream HTTP calls.
type recoverySnapshot struct {
	Cursor   string             `json:"cursor"`
	Snapshot contracts.Snapshot `json:"snapshot"`
	UI       uiSnapshot         `json:"ui"`
	Voice    recoveryVoice      `json:"voice"`
}

type recoveryVoice struct {
	CurrentSession *recoverySession `json:"current_session"`
	Pool           recoveryPool     `json:"pool"`
}

type recoverySession struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

type recoveryPool struct {
	Blocked    bool   `json:"blocked"`
	ReasonCode string `json:"reason_code"`
}

func (s *Service) handleRecovery(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	result, err := s.readRecovery(r.Context(), p)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) readRecovery(ctx context.Context, p contracts.Principal) (recoverySnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return recoverySnapshot{}, err
	}
	defer tx.Rollback()
	if err = pruneUISurfacesTx(ctx, tx, s.now()); err != nil {
		return recoverySnapshot{}, err
	}
	result, err := s.readRecoveryTx(ctx, tx, p)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Service) readRecoveryTx(ctx context.Context, tx *sql.Tx, p contracts.Principal) (recoverySnapshot, error) {
	var result recoverySnapshot
	var err error
	result.Snapshot, err = s.uiEventServer.LoadSnapshotTx(ctx, tx, p)
	if err != nil {
		return result, err
	}
	result.Cursor = result.Snapshot.EventCursor
	result.UI.Cursor, err = strconv.ParseInt(result.Cursor, 10, 64)
	if err != nil {
		return result, err
	}
	result.UI.Surfaces, err = readUISnapshotSurfaces(ctx, tx, p)
	if err != nil {
		return result, err
	}
	// Include the owner's newest session, including terminal state. No session,
	// device, lease, or upstream identity belonging to another owner is exposed.
	var session recoverySession
	err = tx.QueryRowContext(ctx, `SELECT id,generation,state FROM voice_sessions WHERE workspace_id=? AND owner_id=? ORDER BY (state='active') DESC,created_at DESC,id DESC LIMIT 1`, p.WorkspaceID, p.SubjectID).Scan(&session.ID, &session.Generation, &session.State)
	if err == nil {
		result.Voice.CurrentSession = &session
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	err = tx.QueryRowContext(ctx, `SELECT blocked,reason_code FROM voice_pool_state WHERE id=1`).Scan(&result.Voice.Pool.Blocked, &result.Voice.Pool.ReasonCode)
	return result, err
}
