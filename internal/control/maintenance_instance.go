package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

// InstanceState is the durable single-instance dispatch authority. Epochs rise
// on every fence and instance replacement; consumers must not cache a prior
// enabled state across an epoch change.
type InstanceState struct {
	InstanceID                            string     `json:"instance_id"`
	Epoch                                 int64      `json:"epoch"`
	DispatchEnabled                       bool       `json:"dispatch_enabled"`
	DerivedRetrievalEnabled               bool       `json:"derived_retrieval_enabled"`
	RestoreDeletionReconciliationRequired bool       `json:"restore_deletion_reconciliation_required"`
	FenceReason                           string     `json:"fence_reason,omitempty"`
	FencedAt                              *time.Time `json:"fenced_at,omitempty"`
}

type MigrationReadiness struct {
	UnresolvedRuns       int `json:"unresolved_runs"`
	UnknownEffects       int `json:"unknown_effects"`
	ActiveDeploymentJobs int `json:"active_deployment_jobs"`
	LiveDeployments      int `json:"live_deployments"`
	OpenVoiceSessions    int `json:"open_voice_sessions"`
}

func (r MigrationReadiness) Ready() bool {
	return r.UnresolvedRuns == 0 && r.UnknownEffects == 0 && r.ActiveDeploymentJobs == 0 && r.LiveDeployments == 0 && r.OpenVoiceSessions == 0
}

// IsolationProof is accepted only after VerifyIsolationProof authenticates its
// payload with the deployment-managed proof key and validates systemd evidence.
// Its fields are private so callers cannot construct an accepted proof value.
type IsolationProof struct {
	oldInstanceID string
	oldEpoch      int64
	verifiedAt    time.Time
	digest        string
}

type isolationProofPayload struct {
	SchemaVersion        int             `json:"schema_version"`
	OldInstanceID        string          `json:"old_instance_id"`
	OldEpoch             int64           `json:"old_epoch"`
	SourceHost           string          `json:"source_host"`
	Units                []isolationUnit `json:"units"`
	ActiveRuns           int             `json:"active_runs"`
	UnknownEffects       int             `json:"unknown_effects"`
	ActiveDeploymentJobs int             `json:"active_deployment_jobs"`
	LiveDeployments      int             `json:"live_deployments"`
	OpenVoiceSessions    int             `json:"open_voice_sessions"`
	VerifiedAt           time.Time       `json:"verified_at"`
}

type isolationUnit struct {
	Name          string `json:"name"`
	ActiveState   string `json:"active_state"`
	UnitFileState string `json:"unit_file_state"`
}

type isolationProofEnvelope struct {
	Payload string `json:"payload"` // raw URL-safe base64 JSON payload
	HMAC    string `json:"hmac"`    // lowercase hex HMAC-SHA256
}

// RegisterInstanceMaintenanceMigration adds the durable fence record. Version
// 1000 is intentionally isolated in the `instance` migration stream because
// the storage migration ledger currently assigns a globally unique version.
func RegisterInstanceMaintenanceMigration(store *storage.Store) error {
	if err := store.RegisterMigration(storage.Migration{Namespace: "instance", Version: 1000, Name: "persistent_instance_dispatch_fence", Up: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE zoe_instance_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  instance_id TEXT NOT NULL UNIQUE,
  epoch INTEGER NOT NULL CHECK(epoch>0),
  dispatch_enabled INTEGER NOT NULL CHECK(dispatch_enabled IN (0,1)),
  derived_retrieval_enabled INTEGER NOT NULL CHECK(derived_retrieval_enabled IN (0,1)),
  fence_reason TEXT NOT NULL DEFAULT '',
  fenced_at TEXT,
  updated_at TEXT NOT NULL
);
CREATE TABLE zoe_instance_fence_proofs (
  old_instance_id TEXT NOT NULL,
  old_epoch INTEGER NOT NULL,
  proof_digest TEXT NOT NULL,
  verified_at TEXT NOT NULL,
  PRIMARY KEY(old_instance_id,old_epoch)
);
CREATE TABLE zoe_instance_migration_source (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  old_instance_id TEXT NOT NULL,
  old_epoch INTEGER NOT NULL
);`); err != nil {
			return err
		}
		id, err := newInstanceID()
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(ctx, `INSERT INTO zoe_instance_state(singleton,instance_id,epoch,dispatch_enabled,derived_retrieval_enabled,fence_reason,fenced_at,updated_at) VALUES(1,?,1,1,1,'','',?)`, id, now)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "instance", Version: 1001, Name: "restore_deletion_reconciliation_gate", Up: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE zoe_instance_state ADD COLUMN restore_deletion_reconciliation_required INTEGER NOT NULL DEFAULT 0 CHECK(restore_deletion_reconciliation_required IN (0,1))`); err != nil {
			return err
		}
		// A migration-source row is created only when PrepareRestoredInstance
		// prepares a restore target. Backfill those targets closed, including
		// already-prepared databases that predate this marker.
		_, err := tx.ExecContext(ctx, `UPDATE zoe_instance_state SET restore_deletion_reconciliation_required=1
WHERE EXISTS (SELECT 1 FROM zoe_instance_migration_source m WHERE m.singleton=1 AND trim(m.old_instance_id)!='' AND m.old_epoch>0)`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "instance", Version: 1002, Name: "deletion_lineage_and_reconciliation_checkpoints", Up: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE zoe_instance_state ADD COLUMN lineage_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE zoe_instance_state ADD COLUMN generation INTEGER NOT NULL DEFAULT 1 CHECK(generation>0)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE zoe_instance_state ADD COLUMN deletion_sequence INTEGER NOT NULL DEFAULT 0 CHECK(deletion_sequence>=0)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE zoe_instance_state SET lineage_id=instance_id WHERE lineage_id=''`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
CREATE TABLE zoe_deletion_reconciliation_checkpoints (
  target TEXT PRIMARY KEY,
  completed_sequence INTEGER NOT NULL CHECK(completed_sequence>=0),
  updated_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);
INSERT INTO zoe_deletion_reconciliation_checkpoints(target,completed_sequence,updated_at,last_error)
SELECT target,0,strftime('%Y-%m-%dT%H:%M:%fZ','now'),'adapter_missing' FROM (
  SELECT 'claim' AS target UNION ALL SELECT 'source_links' UNION ALL SELECT 'proposals'
  UNION ALL SELECT 'mem0' UNION ALL SELECT 'qdrant' UNION ALL SELECT 'cache'
  UNION ALL SELECT 'export_temp' UNION ALL SELECT 'backup_delete_manifest' UNION ALL SELECT 'source_ledger'
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "instance", Version: 1003, Name: "authenticated_restore_deletion_progress", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE zoe_restore_deletion_progress (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  snapshot_lineage_id TEXT NOT NULL,
  snapshot_generation INTEGER NOT NULL CHECK(snapshot_generation>0),
  snapshot_sequence INTEGER NOT NULL CHECK(snapshot_sequence>=0),
  snapshot_event_count INTEGER NOT NULL CHECK(snapshot_event_count>=0),
  snapshot_event_cursor_sum INTEGER NOT NULL CHECK(snapshot_event_cursor_sum>=0),
  journal_lineage_id TEXT NOT NULL,
  journal_generation INTEGER NOT NULL CHECK(journal_generation>0),
  journal_record_generation INTEGER NOT NULL CHECK(journal_record_generation>=0),
  journal_sequence INTEGER NOT NULL CHECK(journal_sequence>=0),
  journal_head_sha256 TEXT NOT NULL,
  journal_file_sha256 TEXT NOT NULL,
  applied_sequence INTEGER NOT NULL CHECK(applied_sequence>=0),
  activated_generation INTEGER NOT NULL DEFAULT 0 CHECK(activated_generation>=0)
)`)
		return err
	}}); err != nil {
		return err
	}
	return store.RegisterMigration(storage.Migration{Namespace: "instance", Version: 1004, Name: "private_export_temp_registry", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE zoe_export_temp_registry (
  export_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  owner_id TEXT NOT NULL,
  export_scope TEXT NOT NULL CHECK(export_scope IN ('personal','workspace')),
  created_deletion_sequence INTEGER NOT NULL CHECK(created_deletion_sequence>=0),
  status TEXT NOT NULL CHECK(status IN ('active','invalidated','cleaned')),
  invalidation_sequence INTEGER NOT NULL DEFAULT 0 CHECK(invalidation_sequence>=0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX zoe_export_temp_scope ON zoe_export_temp_registry(workspace_id,status,created_deletion_sequence);`)
		return err
	}})
}

func newInstanceID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func GetInstanceState(ctx context.Context, tx *sql.Tx) (InstanceState, error) {
	var state InstanceState
	var dispatch, derived, restoreReconciliationRequired int
	var reason, fenced string
	err := tx.QueryRowContext(ctx, `SELECT instance_id,epoch,dispatch_enabled,derived_retrieval_enabled,restore_deletion_reconciliation_required,fence_reason,coalesce(fenced_at,'') FROM zoe_instance_state WHERE singleton=1`).Scan(&state.InstanceID, &state.Epoch, &dispatch, &derived, &restoreReconciliationRequired, &reason, &fenced)
	if err != nil {
		return state, err
	}
	state.DispatchEnabled, state.DerivedRetrievalEnabled, state.RestoreDeletionReconciliationRequired, state.FenceReason = dispatch == 1, derived == 1, restoreReconciliationRequired == 1, reason
	if fenced != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, fenced)
		if parseErr != nil {
			return state, fmt.Errorf("instance fence timestamp is invalid: %w", parseErr)
		}
		state.FencedAt = &parsed
	}
	return state, nil
}

func DispatchEnabledTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var enabled int
	err := tx.QueryRowContext(ctx, `SELECT dispatch_enabled FROM zoe_instance_state WHERE singleton=1`).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("instance dispatch fence is not initialized")
	}
	return enabled == 1, err
}

func DerivedRetrievalEnabledTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var enabled int
	err := tx.QueryRowContext(ctx, `SELECT derived_retrieval_enabled FROM zoe_instance_state WHERE singleton=1`).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("instance retrieval fence is not initialized")
	}
	return enabled == 1, err
}

// FenceInstance persistently stops future dispatch. It is safe to call while
// the process is online; active or uncertain effects still require separate
// reconciliation before migration may continue.
func FenceInstance(ctx context.Context, store *storage.Store, reason string, expectedEpoch int64) (InstanceState, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 160 || expectedEpoch < 1 {
		return InstanceState{}, errors.New("fence reason and positive expected epoch are required")
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return InstanceState{}, err
	}
	defer tx.Rollback()
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch FROM zoe_instance_state WHERE singleton=1`).Scan(&current); err != nil {
		return InstanceState{}, err
	}
	if current != expectedEpoch {
		return InstanceState{}, errors.New("instance epoch changed; reload state before fencing")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE zoe_instance_state SET epoch=epoch+1,dispatch_enabled=0,derived_retrieval_enabled=0,fence_reason=?,fenced_at=?,updated_at=? WHERE singleton=1 AND epoch=?`, reason, now, now, expectedEpoch); err != nil {
		return InstanceState{}, err
	}
	state, err := GetInstanceState(ctx, tx)
	if err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return InstanceState{}, err
	}
	return state, nil
}

// PrepareRestoredInstance rotates identity and leaves the target dispatch and
// derived retrieval gates closed. It refuses to prepare a copy with unresolved
// authoritative run outcomes.
func PrepareRestoredInstance(ctx context.Context, store *storage.Store, newID string, expectedEpoch int64) (InstanceState, error) {
	if strings.TrimSpace(newID) == "" || len(newID) > 128 || expectedEpoch < 1 {
		return InstanceState{}, errors.New("new instance ID and positive expected epoch are required")
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return InstanceState{}, err
	}
	defer tx.Rollback()
	current, err := GetInstanceState(ctx, tx)
	if err != nil {
		return InstanceState{}, err
	}
	if current.Epoch != expectedEpoch || current.DispatchEnabled || current.RestoreDeletionReconciliationRequired {
		return InstanceState{}, errors.New("restored database must already be fenced at the expected epoch")
	}
	if err = requireQuiescentAuthority(ctx, tx); err != nil {
		return InstanceState{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM zoe_restore_deletion_progress WHERE singleton=1`); err != nil {
		return InstanceState{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO zoe_instance_migration_source(singleton,old_instance_id,old_epoch) VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET old_instance_id=excluded.old_instance_id,old_epoch=excluded.old_epoch`, current.InstanceID, current.Epoch); err != nil {
		return InstanceState{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE zoe_instance_state SET instance_id=?,epoch=epoch+1,dispatch_enabled=0,derived_retrieval_enabled=0,restore_deletion_reconciliation_required=1,fence_reason='restored target awaiting old-instance isolation and deletion reconciliation',fenced_at=?,updated_at=? WHERE singleton=1 AND epoch=?`, newID, now, now, expectedEpoch); err != nil {
		return InstanceState{}, err
	}
	state, err := GetInstanceState(ctx, tx)
	if err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return InstanceState{}, err
	}
	return state, nil
}

func requireQuiescentAuthority(ctx context.Context, tx *sql.Tx) error {
	ready, err := MigrationReadinessTx(ctx, tx)
	if err != nil {
		return err
	}
	if !ready.Ready() {
		return fmt.Errorf("migration blocked by unresolved work: runs=%d effects=%d deployment_jobs=%d deployments=%d voice_sessions=%d", ready.UnresolvedRuns, ready.UnknownEffects, ready.ActiveDeploymentJobs, ready.LiveDeployments, ready.OpenVoiceSessions)
	}
	return nil
}

func MigrationReadinessTx(ctx context.Context, tx *sql.Tx) (MigrationReadiness, error) {
	var result MigrationReadiness
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE status IN ('dispatching','active','uncertain')`).Scan(&result.UnresolvedRuns); err != nil {
		return result, err
	}
	if table, err := tableExistsTx(ctx, tx, "external_effects"); err != nil {
		return result, err
	} else if table {
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM external_effects WHERE lower(status) IN ('unknown','uncertain','pending','started','in_flight','dispatched','submitted')`).Scan(&result.UnknownEffects); err != nil {
			return result, err
		}
	}
	if table, err := tableExistsTx(ctx, tx, "capability_deployment_jobs"); err != nil {
		return result, err
	} else if table {
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_deployment_jobs WHERE status IN ('claimed','uncertain')`).Scan(&result.ActiveDeploymentJobs); err != nil {
			return result, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM capability_deployments WHERE observed_state IN ('unknown','pending','installing','running','uninstalling','uncertain')`).Scan(&result.LiveDeployments); err != nil {
			return result, err
		}
	}
	if table, err := tableExistsTx(ctx, tx, "voice_sessions"); err != nil {
		return result, err
	} else if table {
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_sessions WHERE state IN ('allocating','active','ending','quarantined')`).Scan(&result.OpenVoiceSessions); err != nil {
			return result, err
		}
	}
	return result, nil
}

func tableExistsTx(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&found)
	return found == 1, err
}

// VerifyIsolationProof authenticates proof bytes emitted by zoe_migrate after
// SSH/systemd verification. The HMAC is over canonical JSON payload bytes.
func VerifyIsolationProof(document, key []byte) (IsolationProof, error) {
	var verified IsolationProof
	if len(key) != 32 || len(document) > 1<<20 {
		return verified, errors.New("proof key or document size is invalid")
	}
	var envelope isolationProofEnvelope
	if err := json.Unmarshal(document, &envelope); err != nil {
		return verified, fmt.Errorf("decode isolation proof envelope: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return verified, errors.New("isolation proof payload encoding is invalid")
	}
	if !hmacSHA256Valid(key, raw, envelope.HMAC) {
		return verified, errors.New("isolation proof authentication failed")
	}
	var proof isolationProofPayload
	if err = json.Unmarshal(raw, &proof); err != nil {
		return verified, errors.New("isolation proof payload is invalid")
	}
	if proof.SchemaVersion != 1 || proof.OldInstanceID == "" || proof.OldEpoch < 1 || strings.TrimSpace(proof.SourceHost) == "" || len(proof.Units) == 0 || proof.ActiveRuns != 0 || proof.UnknownEffects != 0 || proof.ActiveDeploymentJobs != 0 || proof.LiveDeployments != 0 || proof.OpenVoiceSessions != 0 || proof.VerifiedAt.IsZero() || time.Since(proof.VerifiedAt) > 24*time.Hour || proof.VerifiedAt.After(time.Now().Add(5*time.Minute)) {
		return verified, errors.New("isolation proof lacks current, quiescent source evidence")
	}
	for _, unit := range proof.Units {
		if strings.TrimSpace(unit.Name) == "" || unit.ActiveState != "inactive" || unit.UnitFileState != "masked" {
			return verified, errors.New("isolation proof requires every named systemd unit inactive and masked")
		}
	}
	verified.oldInstanceID, verified.oldEpoch, verified.verifiedAt = proof.OldInstanceID, proof.OldEpoch, proof.VerifiedAt.UTC()
	verified.digest = fmt.Sprintf("%x", sha256Sum(raw))
	return verified, nil
}

// EnableInstance opens the target dispatch gate only with an authenticated,
// fresh proof matching the source identity recorded in the restored database.
func EnableInstance(ctx context.Context, store *storage.Store, expectedEpoch int64, proof IsolationProof) (InstanceState, error) {
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return InstanceState{}, err
	}
	defer tx.Rollback()
	current, err := GetInstanceState(ctx, tx)
	if err != nil {
		return InstanceState{}, err
	}
	if current.RestoreDeletionReconciliationRequired {
		return InstanceState{}, storage.ErrRestoreDeletionReconciliationRequired
	}
	if current.Epoch != expectedEpoch || current.DispatchEnabled || proof.oldInstanceID == "" || proof.oldEpoch < 1 || proof.digest == "" || time.Since(proof.verifiedAt) > 24*time.Hour {
		return InstanceState{}, errors.New("target fence or old-instance isolation proof does not match")
	}
	if err = requireQuiescentAuthority(ctx, tx); err != nil {
		return InstanceState{}, err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM zoe_instance_fence_proofs WHERE old_instance_id=? AND old_epoch=? AND proof_digest=?`, proof.oldInstanceID, proof.oldEpoch, proof.digest).Scan(&exists)
	if err != nil {
		return InstanceState{}, err
	}
	if exists != 1 {
		return InstanceState{}, errors.New("isolation proof does not match a recorded migration source")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE zoe_instance_state SET dispatch_enabled=1,fence_reason='',fenced_at=NULL,updated_at=? WHERE singleton=1 AND epoch=?`, now, expectedEpoch); err != nil {
		return InstanceState{}, err
	}
	state, err := GetInstanceState(ctx, tx)
	if err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return InstanceState{}, err
	}
	return state, nil
}

// RecordIsolationProof links verified source evidence to the restored target's
// prior authority. The caller must obtain proof via VerifyIsolationProof.
func RecordIsolationProof(ctx context.Context, store *storage.Store, proof IsolationProof) error {
	if proof.oldInstanceID == "" || proof.oldEpoch < 1 || proof.digest == "" || proof.verifiedAt.IsZero() {
		return errors.New("verified isolation proof is required")
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := GetInstanceState(ctx, tx)
	if err != nil {
		return err
	}
	if state.DispatchEnabled {
		return errors.New("isolation proof can only be recorded while target is fenced")
	}
	var sourceID string
	var sourceEpoch int64
	if err = tx.QueryRowContext(ctx, `SELECT old_instance_id,old_epoch FROM zoe_instance_migration_source WHERE singleton=1`).Scan(&sourceID, &sourceEpoch); err != nil {
		return err
	}
	if sourceID != proof.oldInstanceID || sourceEpoch != proof.oldEpoch {
		return errors.New("isolation proof does not match this target's recorded migration source")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO zoe_instance_fence_proofs(old_instance_id,old_epoch,proof_digest,verified_at) VALUES(?,?,?,?) ON CONFLICT(old_instance_id,old_epoch) DO UPDATE SET proof_digest=excluded.proof_digest,verified_at=excluded.verified_at`, proof.oldInstanceID, proof.oldEpoch, proof.digest, proof.verifiedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func RegisterInstanceRoutes(server *Server) error {
	if err := server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/admin/instance", server.handleInstanceAdmin)
		mux.HandleFunc("GET /v1/admin/instance/migration-readiness", server.handleMigrationReadiness)
		mux.HandleFunc("POST /v1/admin/instance/fence", server.handleInstanceFence)
	}); err != nil {
		return err
	}
	return server.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /runner/v1/instance", server.handleRunnerInstance)
	})
}

func (s *Server) handleInstanceAdmin(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || !HasScope(p, "instance:admin") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "instance administration scope required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	state, err := GetInstanceState(r.Context(), tx)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleInstanceFence(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || !HasScope(p, "instance:admin") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "instance administration scope required", nil))
		return
	}
	var request struct {
		ExpectedEpoch int64  `json:"expected_epoch"`
		Reason        string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.Limits.Int64("instance_fence_max_bytes"))).Decode(&request); err != nil {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "invalid instance fence request", nil))
		return
	}
	state, err := FenceInstance(r.Context(), s.store, request.Reason, request.ExpectedEpoch)
	if err != nil {
		writeError(w, apiError(http.StatusConflict, "instance_fence_conflict", "instance could not be fenced", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleMigrationReadiness(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || !HasScope(p, "instance:admin") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "instance administration scope required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	state, err := GetInstanceState(r.Context(), tx)
	if err != nil {
		writeError(w, err)
		return
	}
	readiness, err := MigrationReadinessTx(r.Context(), tx)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		InstanceState
		MigrationReadiness
		MigrationReady bool `json:"migration_ready"`
	}{state, readiness, !state.DispatchEnabled && readiness.Ready()})
}

func (s *Server) handleRunnerInstance(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:control:read") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "runner control read scope required", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	state, err := GetInstanceState(r.Context(), tx)
	if err != nil {
		writeError(w, err)
		return
	}
	// Keep the runner DTO limited to the fencing contract.
	writeJSON(w, http.StatusOK, struct {
		InstanceID      string `json:"instance_id"`
		Epoch           int64  `json:"epoch"`
		DispatchEnabled bool   `json:"dispatch_enabled"`
	}{state.InstanceID, state.Epoch, state.DispatchEnabled})
}

func (s *Server) InstanceStatus(ctx context.Context) (InstanceState, error) {
	tx, err := s.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InstanceState{}, err
	}
	defer tx.Rollback()
	return GetInstanceState(ctx, tx)
}

func IsolationProofDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func hmacSHA256Valid(key, data []byte, provided string) bool {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return hmac.Equal([]byte(fmt.Sprintf("%x", mac.Sum(nil))), []byte(strings.ToLower(provided)))
}

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }
