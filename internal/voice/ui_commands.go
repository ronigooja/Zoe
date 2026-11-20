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

// redisplayTaskTx presents an existing completed result without changing task work.
func (s *Service) redisplayTaskTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p handoffPayload, task string, screenDisplay bool) (contracts.CommandResult, []contracts.Event, error) {
	result := contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}
	var authoritative string
	err := sql.ErrNoRows
	if screenDisplay {
		err = tx.QueryRowContext(ctx, `SELECT e.content FROM events v JOIN tasks t ON t.id=v.aggregate_id AND t.workspace_id=v.workspace_id JOIN task_entries e ON e.id=json_extract(v.payload,'$.payload.entry_id') AND e.task_id=t.id AND e.workspace_id=t.workspace_id JOIN runs r ON r.id=json_extract(v.payload,'$.payload.run_id') AND r.task_id=t.id AND r.workspace_id=t.workspace_id WHERE v.event_type='task.assistant_message' AND v.aggregate_type='task' AND t.workspace_id=? AND t.owner_subject_id=? AND t.id=? AND json_extract(v.payload,'$.payload.task_id')=t.id AND e.kind='assistant_message' AND trim(e.content)<>'' ORDER BY v.cursor DESC LIMIT 1`, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, task).Scan(&authoritative)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var reply string
		if err := tx.QueryRowContext(ctx, `SELECT report_text FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&reply); err != nil {
			return result, nil, err
		}
		if reply = strings.TrimSpace(reply); reply == "" {
			reply = stewardErrorSpeech
		}
		err = s.heldReplyTx(ctx, tx, cmd, p, reply)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE voice_fast_replies SET intent_kind='task_report' WHERE session_id=? AND generation=? AND turn_id=?`, p.SessionID, p.Generation, p.TurnID)
		}
		return result, nil, err
	}
	if err != nil {
		return result, nil, err
	}
	if err = s.recordTaskUITx(ctx, tx, cmd.Principal, task, p.SessionID, p.Generation, p.TurnID, p.Revision, cmd.AcceptedAt); err != nil {
		return result, nil, err
	}
	var request, turn string
	if err = tx.QueryRowContext(ctx, `SELECT r.id,v.id FROM ui_requests r JOIN voice_turns v ON v.id=r.voice_turn_id WHERE v.workspace_id=? AND v.session_id=? AND v.generation=? AND v.turn_id=? AND v.revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&request, &turn); err != nil {
		return result, nil, err
	}
	if err := pinUIResultTx(ctx, tx, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, task, request, "", 0); err != nil {
		return result, nil, err
	}
	if err = s.insertUIJobTx(ctx, tx, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, "task_entry", task+"#"+turn, request, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return result, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET ui_display=1 WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, task); err != nil {
		return result, nil, err
	}
	return result, nil, s.markHandledTx(ctx, tx, cmd, p)
}

func (s *Service) recordTaskUITx(ctx context.Context, tx *sql.Tx, p contracts.Principal, task, session string, generation int64, turn string, revision int64, at time.Time) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,task_id,request,created_at,updated_at) SELECT ?,workspace_id,?,'task',id,?,request,?,? FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=? AND trim(coalesce(request,''))<>''`, id, p.SubjectID, task, stamp, stamp, p.WorkspaceID, session, generation, turn, revision)
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
	err := tx.QueryRowContext(ctx, `SELECT coalesce(surface_id,''),status,task_question,coalesce(input_note,'') FROM ui_requests WHERE id=? AND workspace_id=? AND owner_subject_id=?`, id, p.WorkspaceID, p.SubjectID).Scan(&surface, &status, &awaiting, &note)
	if errors.Is(err, sql.ErrNoRows) {
		err = apiError(http.StatusNotFound, "not_found", "UI request not found", nil)
	}
	return surface, status, awaiting, note, err
}
func (s *Service) commandUIScreen(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var p struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.Width < 320 || p.Width > 8192 || p.Height < 240 || p.Height > 8192 {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "UI screen dimensions are invalid", nil)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO ui_screens(workspace_id,owner_subject_id,width,height,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(workspace_id,owner_subject_id) DO UPDATE SET width=excluded.width,height=excluded.height,updated_at=excluded.updated_at`, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.Width, p.Height, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano))
	return contracts.CommandResult{ResourceType: "ui_screen", ResourceID: cmd.Principal.SubjectID}, nil, err
}

func (s *Service) commandUIRendered(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var p struct {
		RequestID      string `json:"request_id"`
		SurfaceID      string `json:"surface_id"`
		SurfaceVersion int64  `json:"surface_version"`
		Success        *bool  `json:"success"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.RequestID == "" || p.SurfaceID == "" || p.SurfaceVersion < 1 || p.Success == nil {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "Render identity and outcome are required", nil)
	}
	if _, _, _, _, err := uiOwned(ctx, tx, cmd.Principal, p.RequestID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var current bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ui_requests r JOIN ui_surfaces s ON s.workspace_id=r.workspace_id AND s.owner_subject_id=r.owner_subject_id AND s.surface_id=r.surface_id AND s.request_id=r.id AND s.version=r.surface_version WHERE r.id=? AND r.surface_id=? AND r.surface_version=? AND r.status IN ('generated','rendered'))`, p.RequestID, p.SurfaceID, p.SurfaceVersion).Scan(&current); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !current {
		return contracts.CommandResult{}, nil, apiError(409, "ui_render_not_current", "Render acknowledgement is stale", nil)
	}
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	if !*p.Success {
		_, err := tx.ExecContext(ctx, `UPDATE ui_requests SET status='failed',task_question=0,updated_at=? WHERE id=? AND status='generated'`, at, p.RequestID)
		if err == nil {
			err = removeAcknowledgedUISurface(ctx, tx, p.RequestID)
		}
		return contracts.CommandResult{ResourceType: "ui_request", ResourceID: p.RequestID}, nil, err
	}
	_, err := tx.ExecContext(ctx, `UPDATE ui_requests SET status='rendered',updated_at=? WHERE id=? AND status='generated'`, at, p.RequestID)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) SELECT id,workspace_id,owner_subject_id,feedback,?,?,? FROM ui_requests WHERE id=? AND task_question=0 AND trim(coalesce(feedback,''))<>''`, at, at, at, p.RequestID)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_input_notices(kind,source_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) SELECT 'ui_input_request',q.id,r.workspace_id,r.owner_subject_id,q.question,?,?,? FROM ui_requests r JOIN pending_runtime_actions q ON q.id=r.question_id WHERE r.id=? AND r.task_question=1 AND r.chat_input=0`, at, at, at, p.RequestID)
	}
	if err == nil {
		err = removeAcknowledgedUISurface(ctx, tx, p.RequestID)
	}
	return contracts.CommandResult{ResourceType: "ui_request", ResourceID: p.RequestID}, nil, err
}
func (s *Service) commandUIAction(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if err := uiScope(cmd); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var p struct {
		RequestID         string                     `json:"request_id"`
		SurfaceID         string                     `json:"surface_id"`
		SurfaceVersion    int64                      `json:"surface_version"`
		Name              string                     `json:"name"`
		SourceComponentID string                     `json:"source_component_id"`
		Context           map[string]json.RawMessage `json:"context"`
	}
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.RequestID == "" || p.SurfaceID == "" || p.SurfaceVersion < 1 || strings.TrimSpace(p.Name) == "" || p.SourceComponentID == "" || p.Context == nil {
		return contracts.CommandResult{}, nil, apiError(400, "invalid_request", "UI action fields are required", nil)
	}
	surface, status, awaiting, _, err := uiOwned(ctx, tx, cmd.Principal, p.RequestID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var voiceInput bool
	if err = tx.QueryRowContext(ctx, `SELECT chat_input FROM ui_requests WHERE id=?`, p.RequestID).Scan(&voiceInput); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if surface != p.SurfaceID || (status != "generated" && status != "rendered") || (!awaiting && !voiceInput) {
		return contracts.CommandResult{}, nil, apiError(409, "ui_request_not_awaiting_input", "UI request is not awaiting input", nil)
	}
	var task, question string
	err = tx.QueryRowContext(ctx, `SELECT coalesce(r.task_id,''),coalesce(r.question_id,'') FROM ui_requests r JOIN ui_surfaces s ON s.workspace_id=r.workspace_id AND s.owner_subject_id=r.owner_subject_id AND s.surface_id=r.surface_id AND s.request_id=r.id AND s.version=r.surface_version WHERE r.id=? AND r.workspace_id=? AND r.owner_subject_id=? AND r.surface_id=? AND r.surface_version=? AND ((r.task_question=1 AND r.question_id IS NOT NULL AND r.question_id<>'') OR (r.chat_input=1 AND r.question_id IS NULL AND r.task_id IS NULL))`, p.RequestID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SurfaceID, p.SurfaceVersion).Scan(&task, &question)
	if err == sql.ErrNoRows {
		return contracts.CommandResult{}, nil, apiError(409, "ui_input_stale", "UI input binding is unavailable", nil)
	}
	if err != nil {
		return contracts.CommandResult{}, nil, err
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
	if voiceInput {
		if err := insertTextInput(ctx, tx, cmd, text); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE ui_requests SET status='input_received',chat_input=0,updated_at=? WHERE id=?`, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), p.RequestID)
		if err == nil {
			err = pruneUISurfacesTx(ctx, tx, cmd.AcceptedAt)
		}
		return contracts.CommandResult{ResourceType: "voice_text_input", ResourceID: cmd.CommandID}, nil, err
	}
	if s.uiEventServer == nil {
		return contracts.CommandResult{}, nil, errors.New("event_server_missing")
	}
	result, events, err := s.uiEventServer.AppendTaskInputTx(ctx, tx, cmd, task, control.TaskInput{Content: text, QuestionID: &question})
	if err != nil {
		return result, nil, err
	}
	at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_input_notices(kind,source_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES('ui_input_received',?,?,?,?,?,?,?)`, cmd.CommandID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, text, at, at, at); err != nil {
		return result, nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE ui_requests SET status='input_received',task_question=0,updated_at=? WHERE id=?`, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), p.RequestID)
	if err == nil {
		err = pruneUISurfacesTx(ctx, tx, cmd.AcceptedAt)
	}
	return result, events, err
}

func (s *Service) queueUIFeedbackTx(ctx context.Context, tx *sql.Tx, id, workspace, owner, text, at string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, workspace, owner, text, at, at, at)
	return err
}
