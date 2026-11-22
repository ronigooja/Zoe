package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/steward"
)

var ErrModelConsumerStopped = errors.New("model consumer stopped")

func (s *Service) interactiveModel(ctx context.Context, consumer string, call func() error) error {
	store := s.retry
	if store.DB == nil {
		store.DB = s.db
	}
	due, err := store.Due(ctx, consumer, "", s.now())
	if err != nil {
		logging.Warnf("model_retry_state_unreadable consumer=%s", consumer)
		due = true
	}
	if !due {
		return ErrModelConsumerStopped
	}
	err = call()
	if err != nil && backoff.Classify(err) == backoff.Auth {
		if recordErr := store.Record(ctx, consumer, "", backoff.Auth, s.now(), backoff.Interactive, nil); recordErr != nil {
			logging.Warnf("model_retry_record_failed consumer=%s", consumer)
		} else {
			s.PublishRetryState()
		}
	} else if err == nil {
		_ = store.Clear(ctx, consumer, "")
	}
	return err
}

func (s *Service) handleReadSession(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var result sessionResponse
	err := s.db.QueryRowContext(r.Context(), `SELECT id,generation,state FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND id=?`, p.WorkspaceID, p.SubjectID, r.PathValue("session_id")).Scan(&result.SessionID, &result.Generation, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	}
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type commitTurnPayload struct {
	SessionID       string `json:"session_id"`
	Generation      int64  `json:"generation"`
	TurnID          string `json:"turn_id"`
	Revision        int64  `json:"revision"`
	ProviderMessage string `json:"provider_message_id,omitempty"`
	Content         string `json:"content"`
	UserConfirmed   bool   `json:"user_confirmed"`
}

type handoffPayload struct {
	SessionID   string `json:"session_id"`
	Generation  int64  `json:"generation"`
	TurnID      string `json:"turn_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
}

// commandHandoff applies persisted register routing and Steward choice inside
// the command transaction. The caller supplies only the stable turn identity.
func (s *Service) commandHandoff(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest, server *control.Server) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice handoff scope is required", nil)
	}
	var p handoffPayload
	if err := decodePayload(req.Payload, &p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if p.SessionID == "" || p.Generation < 1 || p.TurnID == "" || p.Revision < 1 || len(p.ContentHash) != sha256.Size*2 {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "session, generation, turn revision, and content hash are required", nil)
	}
	// Classification preparation runs before ExecuteCommand acquires its sole
	// database connection. Read the durable routing decision inside this tx.
	category := ""
	var owner, device, state string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,device_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, p.SessionID).Scan(&owner, &device, &state, &generation); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_session_not_found", "voice session was not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if owner != cmd.Principal.SubjectID || device != cmd.Principal.DeviceID {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "invalid_source", "voice turn belongs to another subject or device", nil)
	}
	if state != "active" || generation != p.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_stale", "voice handoff belongs to an inactive media generation", nil)
	}
	var taskID, content, storedCategory, stewardAction, stewardTaskID, questionID, turnID string
	var resolved sql.NullString
	var selectedVersion int64
	var storedHash []byte
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(l.task_id,i.task_id,nullif(v.resumed_task_id,''),''),v.content,v.content_hash,coalesce(v.exits,''),v.steward_action,v.steward_task_id FROM voice_turns v LEFT JOIN voice_task_links l ON l.session_id=v.session_id AND l.generation=v.generation AND l.turn_id=v.turn_id LEFT JOIN voice_task_inputs i ON i.session_id=v.session_id AND i.generation=v.generation AND i.turn_id=v.turn_id WHERE v.workspace_id=? AND v.session_id=? AND v.generation=? AND v.turn_id=? AND v.revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&taskID, &content, &storedHash, &storedCategory, &stewardAction, &stewardTaskID); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision was not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if storedCategory != "" {
		category = storedCategory
	}
	exits := RouteResult{Route: storedCategory}
	if exits.Has("task") {
		category = "task"
	}
	providedHash, err := hex.DecodeString(p.ContentHash)
	if err != nil || !bytes.Equal(storedHash, providedHash) {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "turn_content_conflict", "handoff content hash does not match the committed turn", nil)
	}
	if taskID != "" {
		return contracts.CommandResult{ResourceType: "task", ResourceID: taskID}, nil, nil
	}
	var handled int
	if err := tx.QueryRowContext(ctx, `SELECT handoff_handled FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&handled); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if handled != 0 {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT request,steward_question_id,steward_task_version,id FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&resolved, &questionID, &selectedVersion, &turnID); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if category == "ui" {
		// The UI exit has no consumer until 2b; do not mark it handled.
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID, ResourceVersion: p.Revision}, nil, nil
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
	content = resolved.String
	if category == "recall" || category == "memory_write" {
		id := uuid.NewString()
		at := cmd.AcceptedAt.UTC().Format(time.RFC3339Nano)
		holding := ""
		intent := "recall_holding"
		if category == "memory_write" {
			intent = memoryWriteHolding
			// Preserve the exact committed revision without extending the job
			// schema: a write holding receipt uses that voice_turns row's ID.
			if err := tx.QueryRowContext(ctx, `SELECT id FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, p.SessionID, p.Generation, p.TurnID, p.Revision).Scan(&id); err != nil {
				return contracts.CommandResult{}, nil, err
			}
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
	goal := content
	if err := s.validateStewardSnapshotTx(ctx, tx, cmd.Principal, p); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if stewardAction == "error" {
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, s.heldReplyTx(ctx, tx, cmd, p, stewardErrorSpeech)
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
			return contracts.CommandResult{}, nil, s.staleDecisionTx(ctx, tx, cmd.Principal.WorkspaceID, p, turnID, true)
		}
	}
	{
		if stewardAction == string(steward.ActionReport) || stewardAction == string(steward.ActionClarify) {
			if stewardAction == string(steward.ActionReport) && prior == nil {
				return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "report requires a selected task", nil)
			}
			if stewardAction == string(steward.ActionReport) {
				// This turn speaks the Steward reply only. Preserve any original
				// result pending contextual selection; a report summary is not its ACK.
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
				_, err = tx.ExecContext(ctx, `UPDATE voice_fast_replies SET intent_kind='task_report' WHERE session_id=? AND generation=? AND turn_id=?`, p.SessionID, p.Generation, p.TurnID)
			}
			return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: p.TurnID}, nil, err
		}
		if stewardAction == string(steward.ActionCancel) {
			if prior == nil {
				return contracts.CommandResult{}, nil, apiError(409, "invalid_selection", "cancel requires a selected task", nil)
			}
			var id string
			err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE workspace_id=? AND owner_subject_id=? AND id=?`, cmd.Principal.WorkspaceID, owner, stewardTaskID).Scan(&id)
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
				logging.Infof("selection_stale branch=task_not_live turn=%s", turnID)
				return contracts.CommandResult{}, nil, apiError(409, "selection_stale", "selected task is no longer live", nil)
			}
			input := control.TaskInput{Content: content}
			if stewardAction == string(steward.ActionAnswer) {
				input.QuestionID = &questionID
			}
			r, events, err := server.AppendTaskInputTx(ctx, tx, cmd, stewardTaskID, input)
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
	admission, err := server.AdmitTaskTx(ctx, tx, control.AdmissionContext{Principal: cmd.Principal, Source: "voice", SourceRef: ref, IdempotencyKey: cmd.CommandID, ProfileID: "zoe-steward", Title: content, Goal: goal, InitialMessage: goal, AcceptedAt: cmd.AcceptedAt})
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

// stewardErrorSpeech is a plain statement; the Steward error detail stays in the logs.
const stewardErrorSpeech = "Processing error."

func (s *Service) commandCommitTurn(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice turn commit scope is required", nil)
	}
	var payload commitTurnPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if !payload.UserConfirmed || payload.SessionID == "" || payload.Generation < 1 || strings.TrimSpace(payload.TurnID) == "" || payload.Revision < 1 || strings.TrimSpace(payload.Content) == "" || len(payload.Content) > 20_000 {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "an explicitly user-confirmed bounded turn with stable identity and revision is required", nil)
	}
	var state string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT state,generation FROM voice_sessions WHERE id=? AND workspace_id=? AND owner_id=?`, payload.SessionID, cmd.Principal.WorkspaceID, cmd.Principal.SubjectID).Scan(&state, &generation); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if state != "active" || generation != payload.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_stale", "turn belongs to an inactive media generation", nil)
	}
	content := strings.TrimSpace(payload.Content)
	hash := sha256.Sum256([]byte(content))
	var existingID string
	var existingHash []byte
	err := tx.QueryRowContext(ctx, `SELECT id,content_hash FROM voice_turns WHERE session_id=? AND generation=? AND turn_id=? AND revision=?`, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision).Scan(&existingID, &existingHash)
	if err == nil {
		if !bytes.Equal(hash[:], existingHash) {
			return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "turn_revision_conflict", "turn revision was already committed with different text", nil)
		}
		return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: existingID, ResourceVersion: payload.Revision}, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, err
	}
	id := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,provider_message_id,content,content_hash,confirmed_at,command_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.Revision, payload.ProviderMessage, content, hash[:], cmd.AcceptedAt.UTC().Format(time.RFC3339Nano), cmd.CommandID)
	if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	turnEvent, _ := json.Marshal(map[string]any{"turn_id": payload.TurnID, "revision": payload.Revision, "voice_session_id": payload.SessionID, "generation": payload.Generation, "content_hash": hex.EncodeToString(hash[:]), "status": "confirmed_candidate"})
	event := contracts.Event{Type: "voice.turn.committed", AggregateType: "voice_turn", AggregateID: id, AggregateVersion: payload.Revision, Payload: turnEvent}
	return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: id, ResourceVersion: payload.Revision}, []contracts.Event{event}, nil
}

func (s *Service) classifyCommittedTurn(id string) {
	if _, loaded := s.classifying.LoadOrStore(id, struct{}{}); loaded {
		logging.Infof("classify_in_flight turn=%s", id)
		return
	}
	defer s.classifying.Delete(id)

	ctx, cancel := context.WithTimeout(context.Background(), s.classificationTimeout())
	defer cancel()
	var content, workspace, owner string
	var acceptedRoutes sql.NullString
	var acceptedRequest *string
	var acceptedSnapshot string
	if err := s.db.QueryRowContext(ctx, `SELECT v.content,v.workspace_id,s.owner_id,v.exits,v.request,v.steward_snapshot FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id WHERE v.id=? AND v.handoff_handled=0`, id).Scan(&content, &workspace, &owner, &acceptedRoutes, &acceptedRequest, &acceptedSnapshot); err != nil {
		return
	}
	// A completed selection is reused until preflight explicitly invalidates it.
	if acceptedSnapshot != "" || (acceptedRoutes.Valid && !(RouteResult{Route: acceptedRoutes.String}).Has("task")) {
		return
	}
	route := RouteResult{Route: "task"}
	var err error
	if acceptedRoutes.Valid {
		route = RouteResult{Route: acceptedRoutes.String, Request: acceptedRequest}
	} else if s.register == nil {
		err = errors.New("context register is disabled")
	} else {
		err = s.interactiveModel(ctx, "register", func() error {
			var callErr error
			route, callErr = s.register.Route(ctx, RegisterOwner{workspace, owner}, content, s.now())
			return callErr
		})
	}
	if err == nil && !acceptedRoutes.Valid {
		// The register boundary is accepted once; subsequent selection refreshes
		// must never invoke it again, including after Steward failure.
		var accepted sql.Result
		accepted, err = s.db.ExecContext(ctx, `UPDATE voice_turns SET exits=?,request=? WHERE id=? AND exits IS NULL`, route.Exits(), route.Request, id)
		if err == nil {
			var changed int64
			changed, err = accepted.RowsAffected()
			if err == nil && changed == 0 {
				logging.Warnf("voice_routing_discarded id=%s reason=already_classified", id)
				return
			}
		}
		if err == nil {
			acceptedRoutes = sql.NullString{String: route.Exits(), Valid: true}
			acceptedRequest = route.Request
		}
	}
	var choice steward.Choice
	var taskID string
	var selectedVersion int64
	var snapshot string
	if err == nil && route.Has("task") {
		snapshot, err = stewardSnapshot(ctx, s.db, workspace, owner)
		var candidates []taskCandidate
		if err == nil {
			candidates, err = s.taskCandidates(ctx, s.db, workspace, owner, *route.Request, s.now(), 0)
		}
		if err == nil {
			facts := make([]steward.Candidate, 0, len(candidates))
			for _, c := range candidates {
				facts = append(facts, steward.Candidate{TaskID: c.TaskID, Goal: c.Goal, Status: c.Status, PendingQuestions: c.PendingQuestions, SummaryResult: c.SummaryResult, Additions: c.Additions})
			}
			if s.steward == nil {
				err = errors.New("steward is disabled")
			} else {
				err = s.interactiveModel(ctx, "steward", func() error {
					var callErr error
					choice, callErr = s.steward.Choose(logging.WithUserHash(ctx, logging.UserHash(owner)), steward.Input{Request: *route.Request, DisplayRequested: route.Has("ui"), Tasks: facts})
					return callErr
				})
			}
			if err == nil && choice.TaskID != nil {
				matched := false
				for _, candidate := range candidates {
					if candidate.TaskID == *choice.TaskID {
						matched = true
						taskID, selectedVersion = candidate.TaskID, candidate.Version
						break
					}
				}
				if !matched {
					err = steward.ErrNoMatch
				}
			}
		}
		if err == nil {
			current, readErr := stewardSnapshot(ctx, s.db, workspace, owner)
			if readErr != nil {
				err = readErr
			} else if current != snapshot {
				choice = steward.Choice{}
				taskID = ""
				snapshot = ""
			}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		// Internal failure sentinel, never a model route: fail closed on replay.
		if !acceptedRoutes.Valid {
			route.Route = "error"
		}
		choice = steward.Choice{}
		taskID = ""
		if acceptedRoutes.Valid {
			choice.Action = steward.Action("error")
			snapshot, _ = stewardSnapshot(ctx, s.db, workspace, owner)
		} else {
			snapshot = ""
		}
	}
	writeCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	action := string(choice.Action)
	result, writeErr := s.db.ExecContext(writeCtx, `UPDATE voice_turns SET exits=?,steward_action=?,steward_task_id=?,steward_question_id=?,steward_task_version=?,report_text=?,request=?,steward_snapshot=? WHERE id=? AND handoff_handled=0 AND coalesce(steward_snapshot,'')=? AND exits IS ? AND request IS ?`, route.Exits(), action, taskID, pointerText(choice.QuestionID), selectedVersion, pointerText(choice.Reply), route.Request, snapshot, id, "", acceptedRoutes, acceptedRequest)
	if writeErr == nil {
		var affected int64
		affected, writeErr = result.RowsAffected()
		if writeErr == nil && affected == 0 {
			logging.Warnf("voice_routing_discarded id=%s reason=already_classified", id)
			return
		}
	}
	logging.Infof("voice_routing id=%s failed=%t storage_error=%t", id, err != nil, writeErr != nil)
	if err != nil || writeErr != nil {
		logging.Errorf("voice_routing_failure id=%s error_kind=routing_or_storage", id)
	}
}

func (s *Service) handleTurns(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var owner string
	if err := s.db.QueryRowContext(r.Context(), `SELECT owner_id FROM voice_sessions WHERE workspace_id=? AND id=?`, p.WorkspaceID, r.PathValue("session_id")).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	} else if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if owner != p.SubjectID {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice session belongs to another subject", nil))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT turn_id,generation,revision,provider_message_id,content,confirmed_at FROM voice_turns WHERE session_id=? ORDER BY confirmed_at,turn_id,revision`, r.PathValue("session_id"))
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	defer rows.Close()
	type turnView struct {
		TurnID            string `json:"turn_id"`
		Generation        int64  `json:"generation"`
		Revision          int64  `json:"revision"`
		ProviderMessageID string `json:"provider_message_id,omitempty"`
		Content           string `json:"content"`
		ConfirmedAt       string `json:"confirmed_at"`
	}
	items := []turnView{}
	for rows.Next() {
		var item turnView
		if err := rows.Scan(&item.TurnID, &item.Generation, &item.Revision, &item.ProviderMessageID, &item.Content, &item.ConfirmedAt); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": items})
}

func (s *Service) handleReceipts(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	var owner string
	if err := s.db.QueryRowContext(r.Context(), `SELECT owner_id FROM voice_sessions WHERE workspace_id=? AND id=?`, p.WorkspaceID, r.PathValue("session_id")).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		control.WriteError(w, apiError(http.StatusNotFound, "not_found", "voice session not found", nil))
		return
	} else if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	if owner != p.SubjectID {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice session belongs to another subject", nil))
		return
	}
	type receiptView struct {
		EventID     string          `json:"event_id"`
		Cursor      string          `json:"cursor"`
		Type        string          `json:"type"`
		TaskID      string          `json:"task_id"`
		TaskVersion int64           `json:"task_version"`
		OccurredAt  string          `json:"occurred_at"`
		Payload     json.RawMessage `json:"payload"`
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT e.event_id,e.cursor,e.event_type,e.aggregate_id,e.aggregate_version,e.occurred_at,e.payload FROM voice_task_links l JOIN events e ON e.workspace_id=l.workspace_id AND e.aggregate_type='task' AND e.aggregate_id=l.task_id WHERE l.workspace_id=? AND l.session_id=? AND l.generation=(SELECT generation FROM voice_sessions WHERE id=l.session_id) ORDER BY e.cursor`, p.WorkspaceID, r.PathValue("session_id"))
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	defer rows.Close()
	items := []receiptView{}
	for rows.Next() {
		var item receiptView
		if err := rows.Scan(&item.EventID, &item.Cursor, &item.Type, &item.TaskID, &item.TaskVersion, &item.OccurredAt, &item.Payload); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": items})
}

type linkTaskPayload struct {
	SessionID    string `json:"session_id"`
	Generation   int64  `json:"generation"`
	TurnID       string `json:"turn_id"`
	TurnRevision int64  `json:"turn_revision"`
	TaskID       string `json:"task_id"`
}

type voiceTaskSourceReference struct {
	Kind        string `json:"kind"`
	SessionID   string `json:"voice_session_id"`
	Generation  int64  `json:"generation"`
	TurnID      string `json:"turn_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash,omitempty"`
}

// admitVoiceTask binds a confirmed stable turn to the task inside the same
// transaction as task.create. Revision is provenance; it is deliberately not
// part of the uniqueness key, so a later transcript revision cannot dispatch
// a second task.
func (s *Service) admitVoiceTask(ctx context.Context, tx *sql.Tx, principal contracts.Principal, taskID string, request *control.TaskSourceRequest, raw json.RawMessage, acceptedAt time.Time) ([]contracts.Event, error) {
	var ref voiceTaskSourceReference
	if err := decodePayload(raw, &ref); err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid_source", "voice source reference is invalid", nil)
	}
	if ref.Kind != "voice_turn" || ref.SessionID == "" || ref.Generation < 1 || ref.TurnID == "" || ref.Revision < 1 {
		return nil, apiError(http.StatusBadRequest, "invalid_source", "voice source reference requires a confirmed stable turn identity", nil)
	}
	var owner, deviceID, state string
	var generation int64
	err := tx.QueryRowContext(ctx, `SELECT owner_id,device_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, principal.WorkspaceID, ref.SessionID).Scan(&owner, &deviceID, &state, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "voice_turn_not_found", "voice session was not found", nil)
	}
	if err != nil {
		return nil, err
	}
	if owner != principal.SubjectID || deviceID != principal.DeviceID {
		return nil, apiError(http.StatusForbidden, "invalid_source", "voice turn belongs to another subject or device", nil)
	}
	if state != "active" || generation != ref.Generation {
		return nil, apiError(http.StatusConflict, "voice_generation_stale", "voice turn is not from the active media generation", nil)
	}
	var content string
	var resolved sql.NullString
	var storedExits sql.NullString
	var contentHash []byte
	err = tx.QueryRowContext(ctx, `SELECT content,content_hash,exits,request FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, principal.WorkspaceID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision).Scan(&content, &contentHash, &storedExits, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision was not found", nil)
	}
	if err != nil {
		return nil, err
	}
	var existingTask string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM voice_task_links WHERE session_id=? AND generation=? AND turn_id=?`, ref.SessionID, ref.Generation, ref.TurnID).Scan(&existingTask)
	if err == nil {
		return nil, apiError(http.StatusConflict, "voice_turn_already_dispatched", "this stable voice turn is already bound to a task", map[string]string{"task_id": existingTask})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !resolved.Valid || strings.TrimSpace(resolved.String) == "" || strings.EqualFold(strings.TrimSpace(resolved.String), "none") {
		return nil, apiError(http.StatusConflict, "request_unavailable", "a complete register request is required", nil)
	}
	content = resolved.String
	title := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	if runes := []rune(title); len(runes) > 200 {
		title = string(runes[:200])
	}
	request.Title = title
	request.Goal = content
	request.InitialMessage = content
	if (RouteResult{Route: storedExits.String}).Has("ui") {
		if err := s.recordTaskUITx(ctx, tx, principal, taskID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision, acceptedAt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET ui_display=1 WHERE workspace_id=? AND id=?`, principal.WorkspaceID, taskID); err != nil {
			return nil, err
		}
	}
	eventPayload, _ := json.Marshal(map[string]any{"task_id": taskID, "voice_session_id": ref.SessionID, "generation": ref.Generation, "turn_id": ref.TurnID, "turn_revision": ref.Revision, "content_hash": hex.EncodeToString(contentHash), "status": "task_created"})
	event := contracts.Event{Type: "voice.task.created", AggregateType: "voice_session", AggregateID: ref.SessionID, AggregateVersion: ref.Generation, Payload: eventPayload, OccurredAt: acceptedAt}
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES(?,?,?,?,?,?,?,?,?)`, principal.WorkspaceID, ref.SessionID, ref.Generation, ref.TurnID, ref.Revision, taskID, principal.DeviceID, taskID, acceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, apiError(http.StatusConflict, "voice_turn_already_dispatched", "this stable voice turn is already bound to a task", nil)
		}
		return nil, err
	}
	return []contracts.Event{event}, nil
}

func (s *Service) commandLinkTask(ctx context.Context, tx *sql.Tx, cmd control.CommandContext, req contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
	if !control.HasScope(cmd.Principal, "client:command") && !control.HasScope(cmd.Principal, "voice:turn:commit") {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice task linking requires command scope", nil)
	}
	var payload linkTaskPayload
	if err := decodePayload(req.Payload, &payload); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if payload.SessionID == "" || payload.Generation < 1 || payload.TurnID == "" || payload.TurnRevision < 1 || payload.TaskID == "" {
		return contracts.CommandResult{}, nil, apiError(http.StatusBadRequest, "invalid_request", "session, generation, turn revision, and task are required", nil)
	}
	var owner, sessionState string
	var currentGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,state,generation FROM voice_sessions WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, payload.SessionID).Scan(&owner, &sessionState, &currentGeneration); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if owner != cmd.Principal.SubjectID || currentGeneration != payload.Generation {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "voice turn belongs to another owner or media generation", nil)
	}
	if sessionState == "quarantined" || sessionState == "allocating" || sessionState == "ending" {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_generation_unresolved", "task cannot be linked while media generation is unresolved", nil)
	}
	var turnCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.TurnRevision).Scan(&turnCount); err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if turnCount != 1 {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "voice_turn_not_found", "confirmed voice turn revision not found", nil)
	}
	var taskSource, createdBy string
	var taskVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT source,created_by_device_id,version FROM tasks WHERE workspace_id=? AND id=?`, cmd.Principal.WorkspaceID, payload.TaskID).Scan(&taskSource, &createdBy, &taskVersion); errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandResult{}, nil, apiError(http.StatusNotFound, "not_found", "task not found", nil)
	} else if err != nil {
		return contracts.CommandResult{}, nil, err
	}
	if taskSource != "voice" || createdBy != cmd.Principal.DeviceID {
		return contracts.CommandResult{}, nil, apiError(http.StatusForbidden, "forbidden", "only a voice-sourced task created by this device can be linked", nil)
	}
	if req.ExpectedVersion == nil || *req.ExpectedVersion != taskVersion {
		return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "version_conflict", "task version changed before voice linkage", map[string]any{"current_version": taskVersion})
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES(?,?,?,?,?,?,?,?,?)`, cmd.Principal.WorkspaceID, payload.SessionID, payload.Generation, payload.TurnID, payload.TurnRevision, payload.TaskID, cmd.Principal.DeviceID, cmd.CommandID, cmd.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return contracts.CommandResult{}, nil, apiError(http.StatusConflict, "voice_task_link_conflict", "task or turn is already linked to a different voice record", nil)
		}
		return contracts.CommandResult{}, nil, err
	}
	payloadJSON, _ := json.Marshal(map[string]any{"task_id": payload.TaskID, "voice_session_id": payload.SessionID, "generation": payload.Generation, "turn_id": payload.TurnID, "turn_revision": payload.TurnRevision})
	event := contracts.Event{Type: "voice.task.linked", AggregateType: "voice_session", AggregateID: payload.SessionID, AggregateVersion: payload.Generation, Payload: payloadJSON}
	return contracts.CommandResult{ResourceType: "voice_task_link", ResourceID: payload.TaskID, ResourceVersion: taskVersion}, []contracts.Event{event}, nil
}

func (s *Service) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !control.HasScope(p, "voice:read") && !control.HasScope(p, "voice:pool:reconcile") && !control.HasScope(p, "client:admin") {
		control.WriteError(w, apiError(http.StatusForbidden, "forbidden", "voice:read scope is required", nil))
		return
	}
	var blocked int
	var reason, updatedAt string
	if err := s.db.QueryRowContext(r.Context(), `SELECT blocked,reason_code,updated_at FROM voice_pool_state WHERE id=1`).Scan(&blocked, &reason, &updatedAt); err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	// Only session creators may trigger automatic stale-create recovery here;
	// read-only status access does not authorize persistent recovery writes.
	if blocked != 0 && reason == "gateway_outcome_unknown" && control.HasScope(p, "voice:open") {
		s.recoverPoolStatus(r.Context())
		if err := s.db.QueryRowContext(r.Context(), `SELECT blocked,reason_code,updated_at FROM voice_pool_state WHERE id=1`).Scan(&blocked, &reason, &updatedAt); err != nil {
			control.WriteError(w, safeSessionError(err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blocked": blocked != 0, "reason_code": reason, "updated_at": updatedAt,
		"account_lease_identity":       s.gatewayReady(),
		"account_scoped_termination":   false,
		"trusted_termination_evidence": s.gatewayReady(),
	})
}

// handleVoiceConfig proxies only Space's documented non-secret voice choices,
// WebRTC DataChannel settings, and ICE server configuration. Gateway auth and
// account data are never returned to the client.
func (s *Service) handleVoiceConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := control.PrincipalFromContext(r.Context()); !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if s.dialOrigin == "" {
		control.WriteError(w, apiError(http.StatusServiceUnavailable, "voice_unavailable", "gateway dial origin is unavailable", nil))
		return
	}
	config, err := s.readVoiceConfig(withGatewayRequestID(r.Context(), r.Header.Get("X-Zoe-Client-Request-ID")))
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		spacePublicConfig
		GatewayDialOrigin string `json:"gateway_dial_origin"`
	}{config, s.dialOrigin})
}

func validDialOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.Opaque != "" || strings.ContainsAny(raw, "?#") || strings.HasSuffix(u.Host, ":") {
		return ""
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	return u.String()
}

func (s *Service) readVoiceConfig(ctx context.Context) (spacePublicConfig, error) {
	response, gatewayRequestID, err := s.callGateway(ctx, http.MethodGet, "/v1/voice/config", nil)
	if err != nil {
		return spacePublicConfig{}, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID})
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return spacePublicConfig{}, apiError(http.StatusServiceUnavailable, "voice_unavailable", "Space voice configuration is unavailable", map[string]any{"request_id": gatewayRequestID, "status": response.StatusCode})
	}
	var config spacePublicConfig
	dec := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil || config.Version == "" || len(config.WebRTC) == 0 {
		return spacePublicConfig{}, apiError(http.StatusBadGateway, "voice_config_invalid", "Space voice configuration response is invalid", map[string]any{"request_id": gatewayRequestID})
	}
	return config, nil
}

type spacePublicConfig struct {
	Version    string          `json:"version"`
	Defaults   json.RawMessage `json:"defaults"`
	Voices     json.RawMessage `json:"voices"`
	VoiceModes json.RawMessage `json:"voice_modes"`
	Languages  json.RawMessage `json:"languages"`
	WebRTC     json.RawMessage `json:"webrtc"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError(http.StatusBadRequest, "invalid_request", "invalid JSON request", nil)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return apiError(http.StatusBadRequest, "invalid_request", "request must contain one JSON value", nil)
	}
	return nil
}

func decodePayload(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError(http.StatusBadRequest, "invalid_request", "voice command payload is invalid", nil)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
