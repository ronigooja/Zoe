package control

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// RegisterTaskDispatchGuard installs the dispatch policy for a trusted task
// source. Source handlers are immutable after server registration is sealed.
func (s *Server) RegisterTaskDispatchGuard(source string, guard TaskDispatchGuard) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return errors.New("server registrations are sealed")
	}
	source = strings.TrimSpace(source)
	if source == "" || source == "user" || guard == nil {
		return errors.New("task source and dispatch guard are required; user source is built in")
	}
	if _, exists := s.taskDispatchGuards[source]; exists {
		return errors.New("task dispatch guard is already registered")
	}
	s.taskDispatchGuards[source] = guard
	return nil
}

func (s *Server) taskDispatchReasonTx(ctx context.Context, tx *sql.Tx, source, workspaceID, taskID string, now time.Time) (string, error) {
	if source == "user" || source == "voice" || source == "system" || source == "child" {
		return "", nil
	}
	s.mu.RLock()
	guard := s.taskDispatchGuards[source]
	s.mu.RUnlock()
	if guard == nil {
		return "dispatch_policy_unavailable", nil
	}
	return guard(ctx, tx, workspaceID, taskID, now.UTC())
}

// TaskDispatchAuthorizationTx rechecks a run's task source under the caller's
// SQLite transaction. It is also used by the model-call reservation path so
// authorization and the last pre-dispatch budget reservation share a commit.
func (s *Server) TaskDispatchAuthorizationTx(ctx context.Context, tx *sql.Tx, runID string, now time.Time) (contracts.DispatchAuthorization, error) {
	var workspaceID, taskID, source, status, desired string
	err := tx.QueryRowContext(ctx, `SELECT r.workspace_id,r.task_id,t.source,r.status,r.desired_control FROM runs r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id WHERE r.id=?`, runID).Scan(&workspaceID, &taskID, &source, &status, &desired)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.DispatchAuthorization{Allowed: false, ReasonCode: "run_unavailable"}, nil
	}
	if err != nil {
		return contracts.DispatchAuthorization{}, err
	}
	if status == "uncertain" {
		// There is no read-only runtime lookup contract yet. Preserve this effect as
		// uncertain so the runner cannot mistake a failed recheck for proof
		// that no external session was created.
		return contracts.DispatchAuthorization{Allowed: false, ReasonCode: "reconciliation_unavailable"}, nil
	}
	if status != "dispatching" && status != "active" {
		return contracts.DispatchAuthorization{Allowed: false, ReasonCode: "run_not_dispatchable"}, nil
	}
	if desired != "run" {
		return contracts.DispatchAuthorization{Allowed: false, ReasonCode: "run_stopping"}, nil
	}
	reason, err := s.taskDispatchReasonTx(ctx, tx, source, workspaceID, taskID, now)
	if err != nil {
		return contracts.DispatchAuthorization{}, err
	}
	if reason == "" {
		return contracts.DispatchAuthorization{Allowed: true}, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET desired_control='stop',control_version=control_version+CASE WHEN desired_control='stop' THEN 0 ELSE 1 END WHERE id=? AND status IN ('dispatching','active')`, runID); err != nil {
		return contracts.DispatchAuthorization{}, err
	}
	return contracts.DispatchAuthorization{Allowed: false, ReasonCode: reason}, nil
}

func (s *Server) handleDispatchAuthorization(w http.ResponseWriter, r *http.Request) {
	runner, ok := RunnerPrincipalFromContext(r.Context())
	if !ok {
		WriteError(w, NewAPIError(http.StatusUnauthorized, "unauthenticated", "runner authentication required", nil))
		return
	}
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		WriteError(w, NewAPIError(http.StatusBadRequest, "invalid_request", "run_id is required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		WriteError(w, err)
		return
	}
	defer tx.Rollback()
	var workspaceID, assignedRunner string
	err = tx.QueryRowContext(r.Context(), `SELECT workspace_id,runner_id FROM runs WHERE id=? AND status IN ('dispatching','active','uncertain')`, runID).Scan(&workspaceID, &assignedRunner)
	if errors.Is(err, sql.ErrNoRows) || assignedRunner != runner.RunnerID || !HasRunnerScope(runner, "runner:claim") {
		WriteError(w, NewAPIError(http.StatusNotFound, "run_unavailable", "dispatch authorization is unavailable for this runner", nil))
		return
	}
	if err != nil {
		WriteError(w, err)
		return
	}
	result, err := s.TaskDispatchAuthorizationTx(r.Context(), tx, runID, time.Now().UTC())
	if err != nil {
		WriteError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
