package voice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/steward"
)

func (s *Service) classificationTimeout() time.Duration {
	if s.recall.ClassificationTimeout > 0 {
		return s.recall.ClassificationTimeout
	}
	return 35 * time.Second
}

// classifyDelegation runs with the caller holding the turn's single-flight guard.
func (s *Service) classifyDelegation(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(parent, s.classificationTimeout())
	defer cancel()
	var content, workspace, owner string
	var acceptedRoutes sql.NullString
	var acceptedRequest *string
	var acceptedSnapshot, acceptedAction, session string
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT v.content,v.workspace_id,s.owner_id,v.exits,v.request,v.steward_snapshot,v.steward_action,v.session_id,v.transcript_seq FROM voice_turns v JOIN voice_sessions s ON s.id=v.session_id AND s.workspace_id=v.workspace_id AND s.generation=v.generation WHERE v.id=? AND v.handoff_handled=0`, id).Scan(&content, &workspace, &owner, &acceptedRoutes, &acceptedRequest, &acceptedSnapshot, &acceptedAction, &session, &seq); err != nil {
		return
	}
	// A completed selection is reused until transactional apply detects stale facts.
	if acceptedAction == "error" || acceptedSnapshot != "" || (acceptedRoutes.Valid && !(RouteResult{Route: acceptedRoutes.String}).Has("task")) {
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
			route, callErr = s.register.route(ctx, RegisterOwner{workspace, owner}, content, s.now(), currentTranscript{session, seq})
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
		if route.Request == nil {
			err = errors.New("task route has no request")
		} else if s.steward == nil {
			err = errors.New("steward is disabled")
		} else {
			var candidates []taskCandidate
			err = s.interactiveModel(ctx, "steward", func() error {
				var callErr error
				choice, callErr = s.steward.ChooseLatest(logging.WithUserHash(ctx, logging.UserHash(owner)), func(ctx context.Context) (steward.Input, error) {
					var readErr error
					snapshot, readErr = stewardSnapshot(ctx, s.db, workspace, owner)
					if readErr != nil {
						return steward.Input{}, readErr
					}
					candidates, readErr = s.taskCandidatesForSession(ctx, s.db, workspace, owner, session, *route.Request, s.now(), 0)
					if readErr != nil {
						return steward.Input{}, readErr
					}
					facts := make([]steward.Candidate, 0, len(candidates))
					for _, c := range candidates {
						facts = append(facts, steward.Candidate{TaskID: c.TaskID, Goal: c.Goal, Status: c.Status, PendingQuestions: c.PendingQuestions, SummaryResult: c.SummaryResult, Additions: c.Additions})
					}
					return steward.Input{Request: *route.Request, DisplayRequested: route.Has("ui"), Tasks: facts}, nil
				})
				return callErr
			})
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
	var oversized *steward.InputLimitError
	if errors.As(err, &oversized) {
		logging.Errorf("steward_input_too_large turn_id=%s session_id=%s input_bytes=%d limit_bytes=%d", id, session, oversized.Bytes, oversized.Limit)
	}
	if parent.Err() != nil {
		return
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
	writeCtx, stop := context.WithTimeout(parent, time.Second)
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
	if (err != nil && oversized == nil) || writeErr != nil {
		logging.Errorf("voice_routing_failure id=%s error_kind=routing_or_storage", id)
	}
}
