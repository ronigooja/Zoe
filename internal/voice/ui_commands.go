package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func randomID() (string, error) { id, err := uuid.NewRandom(); return id.String(), err }

func (s *Service) recordTaskUITx(ctx context.Context, tx *sql.Tx, p contracts.Principal, task, session string, generation int64, turn string, revision int64, at time.Time) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,task_id,restatement,utterance,created_at,updated_at) SELECT ?,workspace_id,?,'task',id,?,restatement,content,?,? FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, id, p.SubjectID, task, stamp, stamp, p.WorkspaceID, session, generation, turn, revision)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_turns SET ui_handled=1 WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, p.WorkspaceID, session, generation, turn, revision)
	return err
}

func uiScope(cmd control.CommandContext) error {
	if !control.HasScope(cmd.Principal, "client:command") {
		return apiError(http.StatusForbidden, "forbidden", "UI commands require command scope", nil)
	}
	return nil
}
func uiOwned(ctx context.Context, tx *sql.Tx, p contracts.Principal, id string) (string, string, bool, string, error) {
	var surface, status, note string
	var awaiting bool
	err := tx.QueryRowContext(ctx, `SELECT coalesce(surface_id,''),status,awaiting_input,coalesce(input_note,'') FROM ui_requests WHERE id=? AND workspace_id=? AND owner_subject_id=?`, id, p.WorkspaceID, p.SubjectID).Scan(&surface, &status, &awaiting, &note)
	if errors.Is(err, sql.ErrNoRows) {
		err = apiError(http.StatusNotFound, "not_found", "UI request not found", nil)
	}
	return surface, status, awaiting, note, err
}
func (s *Service) commandUIRendered(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var p struct {
		RequestID string `json:"request_id"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if _, _, _, _, err := uiOwned(ctx, tx, cmd.Principal, p.RequestID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	_, err := tx.ExecContext(ctx, `UPDATE ui_requests SET status='rendered',updated_at=? WHERE id=? AND status='generated'`, s.now().UTC().Format(time.RFC3339Nano), p.RequestID)
	return contracts.CommandResult{ResourceType: "ui_request", ResourceID: p.RequestID}, nil, err
}
func (s *Service) commandUIAction(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var p struct {
		RequestID         string                     `json:"request_id"`
		SurfaceID         string                     `json:"surface_id"`
		Name              string                     `json:"name"`
		SourceComponentID string                     `json:"source_component_id"`
		Context           map[string]json.RawMessage `json:"context"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.RequestID == "" || p.SurfaceID == "" || strings.TrimSpace(p.Name) == "" || p.SourceComponentID == "" || p.Context == nil {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "UI action fields are required", nil)
	}
	surface, status, awaiting, note, err := uiOwned(ctx, tx, cmd.Principal, p.RequestID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if surface != p.SurfaceID || (status != "generated" && status != "rendered") || !awaiting {
		return contracts.CommandResult{}, nil, apiError(409, "ui_request_not_awaiting_input", "UI request is not awaiting input", nil)
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err = e.Encode(p.Context); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if len(p.Context) == 1 {
		for _, v := range p.Context {
			var value string
			if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &value) == nil {
				text = value
			}
		}
	}
	stamp := s.now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE ui_requests SET status='input_received',updated_at=? WHERE id=?`, stamp, p.RequestID); err == nil {
		err = s.queueUIFeedbackTx(ctx, tx, p.RequestID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, note+"\n"+text, stamp)
	}
	return contracts.CommandResult{ResourceType: "ui_request", ResourceID: p.RequestID}, nil, err
}

func (s *Service) queueUIFeedbackTx(ctx context.Context, tx *sql.Tx, id, workspace, owner, text, at string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, workspace, owner, text, at, at, at)
	return err
}
