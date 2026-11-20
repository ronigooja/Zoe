package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/uictl"
)

// uiSnapshot repairs only still-retained documents at the workspace cursor.
type uiSnapshot struct {
	Cursor   int64               `json:"cursor"`
	Surfaces []uiSnapshotSurface `json:"surfaces"`
}
type uiSnapshotSurface struct {
	RequestID      string            `json:"request_id"`
	SurfaceID      string            `json:"surface_id"`
	SurfaceVersion int64             `json:"surface_version"`
	Messages       []json.RawMessage `json:"messages"`
	Container      string            `json:"container"`
	Size           *uictl.Size       `json:"size,omitempty"`
	RenderStatus   string            `json:"render_status"`
	AwaitingInput  bool              `json:"awaiting_input"`
	VoiceInput     bool              `json:"voice_input"`
	TaskID         string            `json:"task_id,omitempty"`
	QuestionID     string            `json:"question_id,omitempty"`
}

func (s *Service) handleUISnapshot(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(401, "unauthenticated", "device authentication required", nil))
		return
	}
	// Like owner-scoped voice reads, this requires authenticated identity, not
	// command scope. No owner/workspace selector is accepted from the client.
	result, err := s.readUISnapshot(r.Context(), p)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) readUISnapshot(ctx context.Context, p contracts.Principal) (uiSnapshot, error) {
	result := uiSnapshot{Surfaces: []uiSnapshotSurface{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err = pruneUISurfacesTx(ctx, tx, s.now()); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT current_cursor FROM event_counters WHERE workspace_id=?),0)`, p.WorkspaceID).Scan(&result.Cursor); err != nil {
		return result, err
	}
	// The first read establishes the SQLite snapshot shared by the document read.
	result.Surfaces, err = readUISnapshotSurfaces(ctx, tx, p)
	if err != nil {
		return result, err
	}
	err = tx.Commit()
	return result, err
}

func readUISnapshotSurfaces(ctx context.Context, tx *sql.Tx, p contracts.Principal) ([]uiSnapshotSurface, error) {
	result := []uiSnapshotSurface{}
	rows, err := tx.QueryContext(ctx, `SELECT s.surface_id,s.version,s.state_json,s.container,s.width,s.height,r.id,r.status,r.awaiting_input,r.voice_input,coalesce(r.task_id,''),coalesce(r.question_id,'') FROM ui_surfaces s LEFT JOIN ui_requests r ON r.id=s.request_id AND r.workspace_id=s.workspace_id AND r.owner_subject_id=s.owner_subject_id AND r.surface_id=s.surface_id AND r.surface_version=s.version WHERE s.workspace_id=? AND s.owner_subject_id=? ORDER BY s.surface_id`, p.WorkspaceID, p.SubjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sf uiSnapshotSurface
		var raw []byte
		var width, height sql.NullInt64
		var request, status sql.NullString
		var awaiting, voiceInput sql.NullBool
		var task, question string
		if err = rows.Scan(&sf.SurfaceID, &sf.SurfaceVersion, &raw, &sf.Container, &width, &height, &request, &status, &awaiting, &voiceInput, &task, &question); err != nil {
			return nil, err
		}
		// Never return a document with missing/stale/cross-owner presentation mapping.
		if !request.Valid || !status.Valid {
			return nil, errors.New("ui_snapshot_mapping_invalid")
		}
		sf.RequestID, sf.RenderStatus = request.String, status.String
		if err = json.Unmarshal(raw, &sf.Messages); err != nil || sf.Messages == nil {
			return nil, errors.New("ui_snapshot_document_invalid")
		}
		if width.Valid && height.Valid {
			sf.Size = &uictl.Size{Width: int(width.Int64), Height: int(height.Int64)}
		}
		sf.TaskID = task
		sf.AwaitingInput = awaiting.Bool && (status.String == "generated" || status.String == "rendered")
		sf.VoiceInput = voiceInput.Bool && (status.String == "generated" || status.String == "rendered")
		if sf.AwaitingInput {
			if task == "" || question == "" {
				return nil, errors.New("ui_snapshot_input_mapping_invalid")
			}
			sf.QuestionID = question
		}
		result = append(result, sf)
	}
	return result, rows.Err()
}
