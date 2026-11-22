package voice

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/steward"
)

const stewardErrorSpeech = "Processing error."

func voiceTaskTitle(request string) string {
	title, _, _ := strings.Cut(strings.TrimSpace(request), "\n")
	if len(title) > 300 {
		title = title[:300]
		for !utf8.ValidString(title) {
			title = title[:len(title)-1]
		}
	}
	if runes := []rune(title); len(runes) > 200 {
		title = string(runes[:200])
	}
	return title
}

type delegationIdentity struct {
	SessionID, TurnID, ContentHash string
	Generation, Revision           int64
}

type oversizedDelegation struct{ requestBytes int }

func (e *oversizedDelegation) Error() string { return "delegation request exceeds task text limit" }

// applyDelegationOutcomeTx applies only a persisted route for the bound turn.
func (s *Service) applyDelegationOutcomeTx(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, p delegationIdentity, turnID string, server *control.Server) (contracts.CommandResult, []contracts.Event, error) {
	var storedCategory, stewardAction, stewardTaskID, questionID string
	var resolved sql.NullString
	var selectedVersion int64
	var storedHash []byte
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(exits,''),request,content_hash,steward_action,steward_task_id,steward_question_id,steward_task_version FROM voice_turns WHERE id=?`, turnID).Scan(&storedCategory, &resolved, &storedHash, &stewardAction, &stewardTaskID, &questionID, &selectedVersion); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	p.ContentHash = hex.EncodeToString(storedHash)
	exits := RouteResult{Route: storedCategory}
	category := storedCategory
	if exits.Has("task") {
		category = "task"
	}
	if storedCategory == "" {
		return contracts.CommandResult{}, nil, errDelegationNotReady
	}
	if stewardAction == "error" {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, s.heldReplyTx(ctx, tx, cmd, p, stewardErrorSpeech)
	}
	if category == "ui" || category == "chat_input" {
		if !resolved.Valid || strings.TrimSpace(resolved.String) == "" {
			return contracts.CommandResult{}, nil, apiError(409, "request_unavailable", "a complete register request is required", nil)
		}
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, s.createRegisterUIJobTx(ctx, tx, turnID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, resolved.String, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano))
	}
	if category != "task" && category != "memory_write" && category != "recall" && category != "chat" {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, s.heldReplyTx(ctx, tx, cmd, p, stewardErrorSpeech)
	}
	if category == "chat" {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, s.markHandledTx(ctx, tx, cmd, p)
	}
	if !resolved.Valid || strings.TrimSpace(resolved.String) == "" || strings.EqualFold(strings.TrimSpace(resolved.String), "none") {
		return contracts.CommandResult{}, nil, apiError(409, "request_unavailable", "a complete register request is required", nil)
	}
	// From here on only the retained request is an execution input.
	content := resolved.String
	if category == "recall" || category == "memory_write" {
		id := uuid.NewString()
		at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
		holding := ""
		intent := "recall_holding"
		if category == "memory_write" {
			intent = memoryWriteHolding
			// Preserve the exact committed revision without extending the job
			// schema: a write holding receipt uses that voice_turns row's ID.
			id = turnID
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'delivered',?,?,?)`, id, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, holding, intent, at, at, at)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_recall_jobs(id,workspace_id,owner_id,session_id,generation,turn_id,question,holding_id) VALUES(?,?,?,?,?,?,?,(SELECT id FROM voice_fast_replies WHERE session_id=? AND generation=? AND turn_id=?))`, uuid.NewString(), cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, p.SessionID, p.Generation, p.TurnID, content, p.SessionID, p.Generation, p.TurnID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if err = s.markHandledTx(ctx, tx, cmd, p); err != nil {
			return contracts.CommandResult{}, nil, err
		}
		event, _ := json.Marshal(map[string]any{"route": "fast", "category": category, "turn_id": p.TurnID, "revision": p.Revision})
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, []contracts.Event{{Type: "voice.turn.handoff", AggregateType: "voice_session", AggregateID: p.SessionID, AggregateVersion: p.Generation, Payload: event}}, nil
	}
	if err := s.validateStewardSnapshotTx(ctx, tx, cmd.Principal, p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	var prior *priorTaskContext
	if stewardTaskID != "" {
		var version int64
		var err error
		prior, version, err = readPriorTaskContext(ctx, tx, cmd.Principal, stewardTaskID, turnID)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if version != selectedVersion {
			return contracts.CommandResult{}, nil, staleSelection("version_changed", turnID)
		}
	}
	switch steward.Action(stewardAction) {
	case steward.ActionNew, steward.ActionContinue, steward.ActionAppend, steward.ActionAnswer:
		if len(content) > s.limits.Int("task_text_max_bytes") {
			return contracts.CommandResult{}, nil, &oversizedDelegation{requestBytes: len(content)}
		}
	}
	{
		if stewardAction == string(steward.ActionReport) || stewardAction == string(steward.ActionClarify) {
			if stewardAction == string(steward.ActionReport) && prior == nil {
				return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "report requires a selected task", nil)
			}
			if stewardAction == string(steward.ActionReport) {
				// Defer the original result until this report reply commits delivered;
				// the summary itself never acknowledges the original result.
				if _, err := tx.ExecContext(ctx, `UPDATE voice_result_deliveries AS d SET last_error_code='awaiting_context' WHERE workspace_id=? AND subject_id=? AND task_id=? AND status='pending' AND `+resultNotInOpening+` AND NOT EXISTS(SELECT 1 FROM voice_result_chunks c WHERE c.entry_id=d.entry_id AND c.state!='pending')`, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, stewardTaskID); err != nil {
					return contracts.CommandResult{}, nil, err
				}
			}
			var reply string
			if err := tx.QueryRowContext(ctx, `SELECT report_text FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&reply); err != nil {
				return contracts.CommandResult{}, nil, err
			}
			reply = strings.TrimSpace(reply)
			if reply == "" {
				reply = stewardErrorSpeech
			}
			err := s.heldReplyTx(ctx, tx, cmd, p, reply)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE voice_fast_replies SET intent_kind='task_report',report_turn_id=CASE WHEN ?='report' THEN ? ELSE NULL END WHERE session_id=? AND generation=? AND turn_id=?`, stewardAction, turnID, p.SessionID, p.Generation, p.TurnID)
			}
			return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, err
		}
		if stewardAction == string(steward.ActionCancel) {
			if prior == nil {
				return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "cancel requires a selected task", nil)
			}
			var id string
			err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND id=?`, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID, stewardTaskID).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, s.heldReplyTx(ctx, tx, cmd, p, stewardErrorSpeech)
			}
			if err != nil {
				return contracts.CommandResult{}, nil, err
			}
			r, events, err := server.StopTaskTx(ctx, tx, cmd, id)
			var conflict *control.APIError
			if err != nil && (!errors.As(err, &conflict) || conflict.Code != "state_conflict") {
				return r, events, err
			}
			if err != nil {
				r = contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}
			}
			return r, events, s.stopStateReplyTx(ctx, tx, cmd, p, id)
		}
		if stewardAction == string(steward.ActionRedisplay) {
			if prior == nil || prior.Result == nil {
				return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "redisplay requires an authoritative result", nil)
			}
			return s.redisplayTaskTx(ctx, tx, cmd, p, stewardTaskID, true)
		}
		if stewardAction == string(steward.ActionAppend) || stewardAction == string(steward.ActionAnswer) {
			if prior == nil || !liveTaskStatus(prior.Status) {
				return contracts.CommandResult{}, nil, staleSelection("task_not_live", turnID)
			}
			input := control.TaskInput{Content: content}
			if stewardAction == string(steward.ActionAnswer) {
				input.QuestionID = &questionID
			}
			r, events, err := server.AppendTaskInputTx(ctx, tx, cmd, stewardTaskID, input)
			var conflict *control.APIError
			if errors.As(err, &conflict) && conflict.Code == "question_stale" {
				return r, nil, staleSelection("question_unavailable", turnID)
			}
			if err == nil {
				entryID := appendedEntryID(events)
				if entryID == "" {
					return contracts.CommandResult{}, nil, errors.New("append did not return entry identity")
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO voice_task_inputs(session_id,generation,turn_id,workspace_id,turn_revision,task_id,entry_id,question_id,content_hash) VALUES(?,?,?,?,?,?,?,?,?)`, p.SessionID, p.Generation, p.TurnID, cmd.Principal.WorkspaceID, p.Revision, stewardTaskID, entryID, input.QuestionID, storedHash)
			}
			if err == nil && exits.Has("ui") {
				_, err = tx.ExecContext(ctx, `UPDATE tasks SET ui_display=1 WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, stewardTaskID)
				if err != nil {
					return contracts.CommandResult{}, nil, err
				}
				err = s.recordTaskUITx(ctx, tx, cmd.Principal, stewardTaskID, p.SessionID, p.Generation, p.TurnID, p.Revision, cmd.AcceptedAt)
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE voice_turns SET resumed_task_id=?,handoff_handled=1 WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, stewardTaskID, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision)
			}
			return r, events, err
		}
	}
	if stewardAction != string(steward.ActionNew) && stewardAction != string(steward.ActionContinue) {
		return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "no valid task admission decision", nil)
	}
	if stewardAction == string(steward.ActionContinue) && prior == nil {
		return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "continuation requires a selected prior task", nil)
	}
	ref, _ := json.Marshal(voiceTaskSourceReference{Kind: "voice_turn", SessionID: p.SessionID, Generation: p.Generation, TurnID: p.TurnID, Revision: p.Revision, ContentHash: p.ContentHash})
	admission, err := server.AdmitTaskTx(ctx, tx, control.AdmissionContext{Principal: cmd.Principal, Source: "voice", SourceRef: ref, IdempotencyKey: cmd.CommandID, ProfileID: "zoe-steward", Title: voiceTaskTitle(content), Goal: content, InitialMessage: content, AcceptedAt: cmd.AcceptedAt, TrustedSource: true})
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if stewardAction == string(steward.ActionContinue) {
		additions, err := json.Marshal(prior.Additions)
		if err != nil {
			return contracts.CommandResult{}, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_prior_contexts(task_id,workspace_id,prior_task_id,goal,status,result,additions_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, admission.Task.ID, cmd.Principal.WorkspaceID, stewardTaskID, prior.Goal, prior.Status, prior.Result, additions, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return contracts.CommandResult{}, nil, err
		}
	}
	events := admission.Events
	status := "queued"
	handoffEvent, _ := json.Marshal(map[string]any{"task_id": admission.Task.ID, "turn_id": p.TurnID, "revision": p.Revision, "content_hash": p.ContentHash, "profile_id": "zoe-steward", "status": status, "route": "task", "category": category})
	events = append(events, contracts.Event{Type: "voice.turn.handoff", AggregateType: "voice_session", AggregateID: p.SessionID, AggregateVersion: p.Generation, Payload: handoffEvent})
	return contracts.CommandResult{ResourceType: "task", ResourceID: admission.Task.ID, ResourceVersion: admission.Task.Version}, events, nil
}
