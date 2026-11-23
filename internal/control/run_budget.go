package control

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func (s *Server) handleRunBudgetReserve(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:claim") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "runner lacks budget accounting scope", nil))
		return
	}
	var req contracts.RunBudgetReserveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.AttemptID == "" || req.Fence == 0 || req.ReservationID == "" || !validBudgetUsage(req.Usage) || !hasBudgetUsage(req.Usage) {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "budget reservation identity and positive nonnegative usage are required", nil))
		return
	}
	body, _ := json.Marshal(req)
	hash := sha256.Sum256(body)
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	var owner, attempt, status, desired string
	var fence uint64
	var budgetJSON []byte
	err = tx.QueryRowContext(r.Context(), `SELECT runner_id,attempt_id,fence,status,desired_control,budget_snapshot_json FROM runs WHERE id=?`, r.PathValue("run_id")).Scan(&owner, &attempt, &fence, &status, &desired, &budgetJSON)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (owner != p.RunnerID || attempt != req.AttemptID || fence != req.Fence || !HasRunnerScope(p, "runner:claim")) {
		writeError(w, apiError(http.StatusNotFound, "not_found", "run budget is not available to this runner", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var limits contracts.RunBudgetSnapshot
	if err := json.Unmarshal(budgetJSON, &limits); err != nil {
		writeError(w, apiError(http.StatusInternalServerError, "internal", "persisted run budget is invalid", nil))
		return
	}
	var existingHash, reservedJSON, actualJSON []byte
	var existingStatus string
	err = tx.QueryRowContext(r.Context(), `SELECT request_hash,reserved_json,actual_json,status FROM run_budget_reservations WHERE run_id=? AND reservation_id=?`, r.PathValue("run_id"), req.ReservationID).Scan(&existingHash, &reservedJSON, &actualJSON, &existingStatus)
	if err == nil {
		if !sameHash(existingHash, hash[:]) {
			writeError(w, apiError(http.StatusConflict, "idempotency_conflict", "reservation id was reused with a different budget request", nil))
			return
		}
		var reserved, actual contracts.RunBudgetUsage
		if json.Unmarshal(reservedJSON, &reserved) != nil || json.Unmarshal(actualJSON, &actual) != nil {
			writeError(w, apiError(http.StatusInternalServerError, "internal", "persisted run budget ledger is invalid", nil))
			return
		}
		authorizedNow := false
		authorizationReason := ""
		if status == "dispatching" || status == "active" {
			authorization, authErr := s.TaskDispatchAuthorizationTx(r.Context(), tx, r.PathValue("run_id"), time.Now().UTC())
			if authErr != nil {
				writeError(w, authErr)
				return
			}
			authorizedNow = authorization.Allowed
			authorizationReason = authorization.ReasonCode
			if !authorizedNow {
				desired = "stop"
				if authorizationReason != "" {
					if err := appendBudgetEventTx(r, tx, r.PathValue("run_id"), req.AttemptID, req.Fence, "run.dispatch_authorization_rejected", map[string]any{"reason": authorizationReason, "reservation_id": req.ReservationID}); err != nil {
						writeError(w, err)
						return
					}
				}
			}
		}
		consumed, e := budgetConsumedTx(r, tx, r.PathValue("run_id"))
		if e != nil {
			writeError(w, e)
			return
		}
		receipt := contracts.RunBudgetReceipt{ReservationID: req.ReservationID, Status: existingStatus, Allowed: existingStatus != "rejected" && authorizedNow && desired == "run", StopRequested: desired == "stop", Reserved: reserved, Actual: actual, Consumed: consumed}
		if err := tx.Commit(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	consumed, err := budgetConsumedTx(r, tx, r.PathValue("run_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	authorized := false
	authorizationReason := ""
	if status == "dispatching" || status == "active" {
		authorization, authErr := s.TaskDispatchAuthorizationTx(r.Context(), tx, r.PathValue("run_id"), time.Now().UTC())
		if authErr != nil {
			writeError(w, authErr)
			return
		}
		authorized = authorization.Allowed
		authorizationReason = authorization.ReasonCode
		if !authorized {
			desired = "stop"
		}
	}
	allowed := (status == "dispatching" || status == "active") && desired == "run" && authorized && budgetWithinLimits(addBudgetUsage(consumed, req.Usage), limits)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := "reserved"
	if !allowed {
		state = "rejected"
	}
	reservedRaw, _ := json.Marshal(req.Usage)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO run_budget_reservations(run_id,reservation_id,attempt_id,fence,request_hash,reserved_json,status,actual_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'{}',?,?)`, r.PathValue("run_id"), req.ReservationID, req.AttemptID, req.Fence, hash[:], reservedRaw, state, now, now); err != nil {
		writeError(w, err)
		return
	}
	if !allowed && desired != "stop" {
		if err := requestBudgetStopTx(r, tx, r.PathValue("run_id"), req.AttemptID, req.Fence, "reservation_exceeded", req.ReservationID); err != nil {
			writeError(w, err)
			return
		}
		desired = "stop"
	} else if !allowed && authorizationReason != "" {
		if err := appendBudgetEventTx(r, tx, r.PathValue("run_id"), req.AttemptID, req.Fence, "run.dispatch_authorization_rejected", map[string]any{"reason": authorizationReason, "reservation_id": req.ReservationID}); err != nil {
			writeError(w, err)
			return
		}
	}
	if err = appendBudgetEventTx(r, tx, r.PathValue("run_id"), req.AttemptID, req.Fence, "run.budget_reserved", map[string]any{"reservation_id": req.ReservationID, "status": state, "usage": req.Usage}); err != nil {
		writeError(w, err)
		return
	}
	if allowed {
		consumed = addBudgetUsage(consumed, req.Usage)
	}
	receipt := contracts.RunBudgetReceipt{ReservationID: req.ReservationID, Status: state, Allowed: allowed, StopRequested: desired == "stop", Reserved: req.Usage, Consumed: consumed}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleRunBudgetSettle(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:claim") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "runner lacks budget accounting scope", nil))
		return
	}
	var req contracts.RunBudgetSettleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.AttemptID == "" || req.Fence == 0 || req.SettlementID == "" || (req.Outcome != "settled" && req.Outcome != "unknown") || !validBudgetUsage(req.Actual) || req.Outcome == "unknown" && hasBudgetUsage(req.Actual) {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "budget settlement identity, outcome, or actual usage is invalid", nil))
		return
	}
	if req.Outcome == "settled" && strings.TrimSpace(req.EvidenceRef) == "" {
		writeError(w, apiError(http.StatusBadRequest, "usage_evidence_required", "confirmed usage settlement requires an upstream or observation evidence reference", nil))
		return
	}
	body, _ := json.Marshal(req)
	hash := sha256.Sum256(body)
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	runID, resID := r.PathValue("run_id"), r.PathValue("reservation_id")
	var owner, attempt, status, desired, existingBudgetStatus string
	var fence uint64
	var budgetJSON, requestHash, reservedJSON, actualJSON, settleHash []byte
	var oldSettlementID string
	err = tx.QueryRowContext(r.Context(), `SELECT r.runner_id,r.attempt_id,r.fence,r.status,r.desired_control,r.budget_snapshot_json,b.request_hash,b.reserved_json,b.actual_json,b.status,b.settlement_id,b.settlement_hash FROM runs r JOIN run_budget_reservations b ON b.run_id=r.id WHERE r.id=? AND b.reservation_id=?`, runID, resID).Scan(&owner, &attempt, &fence, &status, &desired, &budgetJSON, &requestHash, &reservedJSON, &actualJSON, &existingBudgetStatus, &oldSettlementID, &settleHash)
	_ = requestHash
	if errors.Is(err, sql.ErrNoRows) || err == nil && (owner != p.RunnerID || attempt != req.AttemptID || fence != req.Fence || !HasRunnerScope(p, "runner:claim")) {
		writeError(w, apiError(http.StatusNotFound, "not_found", "run budget reservation is not available to this runner", nil))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if oldSettlementID == req.SettlementID {
		if !sameHash(settleHash, hash[:]) {
			writeError(w, apiError(http.StatusConflict, "idempotency_conflict", "settlement id was reused with a different budget result", nil))
			return
		}
		var limits contracts.RunBudgetSnapshot
		_ = json.Unmarshal(budgetJSON, &limits)
		consumed, e := budgetConsumedTx(r, tx, runID)
		if e != nil {
			writeError(w, e)
			return
		}
		var reserved, actual contracts.RunBudgetUsage
		_ = json.Unmarshal(reservedJSON, &reserved)
		_ = json.Unmarshal(actualJSON, &actual)
		receipt := contracts.RunBudgetReceipt{ReservationID: resID, Status: existingBudgetStatus, Allowed: existingBudgetStatus != "rejected", StopRequested: desired == "stop", Reserved: reserved, Actual: actual, Consumed: consumed}
		if e = tx.Commit(); e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	if existingBudgetStatus != "reserved" && existingBudgetStatus != "unknown" {
		writeError(w, apiError(http.StatusConflict, "state_conflict", "reservation is already settled or rejected", nil))
		return
	}
	if oldSettlementID != "" && req.Outcome == "unknown" {
		writeError(w, apiError(http.StatusConflict, "state_conflict", "an unknown reservation can only be reconciled by confirmed actual usage", nil))
		return
	}
	var reserved contracts.RunBudgetUsage
	if err = json.Unmarshal(reservedJSON, &reserved); err != nil {
		writeError(w, err)
		return
	}
	newStatus := req.Outcome
	actualRaw := []byte(`{}`)
	if req.Outcome == "settled" {
		actualRaw, _ = json.Marshal(req.Actual)
		newStatus = "settled"
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE run_budget_reservations SET status=?,actual_json=?,settlement_id=?,settlement_hash=?,updated_at=? WHERE run_id=? AND reservation_id=?`, newStatus, actualRaw, req.SettlementID, hash[:], time.Now().UTC().Format(time.RFC3339Nano), runID, resID); err != nil {
		writeError(w, err)
		return
	}
	consumed, err := budgetConsumedTx(r, tx, runID)
	if err != nil {
		writeError(w, err)
		return
	}
	var limits contracts.RunBudgetSnapshot
	if err = json.Unmarshal(budgetJSON, &limits); err != nil {
		writeError(w, err)
		return
	}
	stopRequested := desired == "stop"
	if !budgetWithinLimits(consumed, limits) && !stopRequested {
		if err = requestBudgetStopTx(r, tx, runID, attempt, fence, "actual_usage_exceeded", resID); err != nil {
			writeError(w, err)
			return
		}
		stopRequested = true
	}
	if err = appendBudgetEventTx(r, tx, runID, attempt, fence, "run.budget_settled", map[string]any{"reservation_id": resID, "outcome": req.Outcome, "actual": req.Actual}); err != nil {
		writeError(w, err)
		return
	}
	receipt := contracts.RunBudgetReceipt{ReservationID: resID, Status: newStatus, Allowed: true, StopRequested: stopRequested, Reserved: reserved, Actual: req.Actual, Consumed: consumed}
	if err = tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func budgetConsumedTx(r *http.Request, tx *sql.Tx, runID string) (contracts.RunBudgetUsage, error) {
	rows, err := tx.QueryContext(r.Context(), `SELECT reserved_json,actual_json,status FROM run_budget_reservations WHERE run_id=?`, runID)
	if err != nil {
		return contracts.RunBudgetUsage{}, err
	}
	defer rows.Close()
	var total contracts.RunBudgetUsage
	for rows.Next() {
		var reserveRaw, actualRaw []byte
		var status string
		if err = rows.Scan(&reserveRaw, &actualRaw, &status); err != nil {
			return total, err
		}
		if status == "rejected" {
			continue
		}
		var use contracts.RunBudgetUsage
		raw := reserveRaw
		if status == "settled" {
			raw = actualRaw
		}
		if err = json.Unmarshal(raw, &use); err != nil {
			return total, err
		}
		total = addBudgetUsage(total, use)
	}
	return total, rows.Err()
}
func addBudgetUsage(a, b contracts.RunBudgetUsage) contracts.RunBudgetUsage {
	return contracts.RunBudgetUsage{ModelCalls: saturatingBudgetAdd(a.ModelCalls, b.ModelCalls), InputUnits: saturatingBudgetAdd(a.InputUnits, b.InputUnits), OutputUnits: saturatingBudgetAdd(a.OutputUnits, b.OutputUnits), ToolSteps: saturatingBudgetAdd(a.ToolSteps, b.ToolSteps), ArtifactBytes: saturatingBudgetAdd(a.ArtifactBytes, b.ArtifactBytes)}
}

func saturatingBudgetAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

func validBudgetUsage(u contracts.RunBudgetUsage) bool {
	return u.ModelCalls >= 0 && u.InputUnits >= 0 && u.OutputUnits >= 0 && u.ToolSteps >= 0 && u.ArtifactBytes >= 0
}
func hasBudgetUsage(u contracts.RunBudgetUsage) bool {
	return u.ModelCalls > 0 || u.InputUnits > 0 || u.OutputUnits > 0 || u.ToolSteps > 0 || u.ArtifactBytes > 0
}
func budgetWithinLimits(u contracts.RunBudgetUsage, b contracts.RunBudgetSnapshot) bool {
	return u.ModelCalls >= 0 && u.InputUnits >= 0 && u.OutputUnits >= 0 && u.ToolSteps >= 0 && u.ArtifactBytes >= 0 && u.ModelCalls <= b.MaxModelCalls && u.InputUnits <= b.MaxInputUnits && u.OutputUnits <= b.MaxOutputUnits && u.ToolSteps <= b.MaxToolSteps && u.ArtifactBytes <= b.MaxArtifactBytes
}

func requestBudgetStopTx(r *http.Request, tx *sql.Tx, runID, attempt string, fence uint64, reason, reservation string) error {
	_, err := tx.ExecContext(r.Context(), `UPDATE runs SET desired_control='stop',control_version=control_version+1 WHERE id=? AND fence=? AND desired_control='run'`, runID, fence)
	if err != nil {
		return err
	}
	return appendBudgetEventTx(r, tx, runID, attempt, fence, "run.budget_stop_requested", map[string]any{"reason": reason, "reservation_id": reservation})
}

func appendBudgetEventTx(r *http.Request, tx *sql.Tx, runID, attempt string, fence uint64, kind string, payload any) error {
	return appendEventTx(r.Context(), tx, contracts.Event{Type: kind, OccurredAt: time.Now().UTC(), AggregateType: "run", AggregateID: runID, AggregateVersion: int64(fence), Payload: raw(map[string]any{"run_id": runID, "attempt_id": attempt, "fence": fence, "budget": payload})})
}
