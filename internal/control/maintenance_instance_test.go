package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

func TestPersistentInstanceFenceAndVerifiedRestore(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := instanceStatus(t, store)
	if err != nil || !state.DispatchEnabled || state.Epoch != 1 || state.RestoreDeletionReconciliationRequired {
		t.Fatalf("initial state=%#v err=%v", state, err)
	}
	fenced, err := FenceInstance(ctx, store, "migration", state.Epoch)
	if err != nil || fenced.DispatchEnabled || fenced.Epoch != 2 {
		t.Fatalf("fenced state=%#v err=%v", fenced, err)
	}
	if _, err = FenceInstance(ctx, store, "stale", 1); err == nil {
		t.Fatal("stale fence epoch was accepted")
	}
	prepared, err := PrepareRestoredInstance(ctx, store, "target-instance", fenced.Epoch)
	if err != nil || prepared.DispatchEnabled || prepared.DerivedRetrievalEnabled || !prepared.RestoreDeletionReconciliationRequired || prepared.InstanceID != "target-instance" || prepared.Epoch != 3 {
		t.Fatalf("prepared state=%#v err=%v", prepared, err)
	}
	if lineage, err := storage.InstanceLineage(ctx, store.DB()); err != nil || lineage != state.InstanceID {
		t.Fatalf("restore changed external namespace: lineage=%q initial=%q err=%v", lineage, state.InstanceID, err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	proofDoc := signedProof(t, key, fenced.InstanceID, fenced.Epoch)
	proof, err := VerifyIsolationProof(proofDoc, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = RecordIsolationProof(ctx, store, proof); err != nil {
		t.Fatal(err)
	}
	if _, err = EnableInstance(ctx, store, prepared.Epoch, proof); !errors.Is(err, storage.ErrRestoreDeletionReconciliationRequired) {
		t.Fatalf("restore activation without reconciliation returned %v", err)
	}
	stillClosed, err := instanceStatus(t, store)
	if err != nil || stillClosed.Epoch != prepared.Epoch || stillClosed.DispatchEnabled || stillClosed.DerivedRetrievalEnabled || !stillClosed.RestoreDeletionReconciliationRequired {
		t.Fatalf("rejected activation changed target gates: state=%#v err=%v", stillClosed, err)
	}
	if _, err = PrepareRestoredInstance(ctx, store, "second-target", prepared.Epoch); err == nil {
		t.Fatal("active instance was accepted as a restored target")
	}
}

func TestRestoreGateMigrationBackfillsPreviouslyPreparedTargets(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	legacySchema := `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT NOT NULL,applied_at TEXT NOT NULL,namespace TEXT NOT NULL DEFAULT '');
INSERT INTO schema_migrations(version,name,applied_at,namespace) VALUES(1000,'persistent_instance_dispatch_fence','now','instance');
CREATE TABLE zoe_instance_state(singleton INTEGER PRIMARY KEY,instance_id TEXT NOT NULL,epoch INTEGER NOT NULL,dispatch_enabled INTEGER NOT NULL,derived_retrieval_enabled INTEGER NOT NULL,fence_reason TEXT NOT NULL DEFAULT '',fenced_at TEXT,updated_at TEXT NOT NULL);
CREATE TABLE zoe_instance_migration_source(singleton INTEGER PRIMARY KEY,old_instance_id TEXT NOT NULL,old_epoch INTEGER NOT NULL);
INSERT INTO zoe_instance_state(singleton,instance_id,epoch,dispatch_enabled,derived_retrieval_enabled,fence_reason,updated_at) VALUES(1,'old-target',3,0,0,'restored target awaiting isolation','now');
INSERT INTO zoe_instance_migration_source(singleton,old_instance_id,old_epoch) VALUES(1,'prior-source',2);`
	if _, err = store.DB().ExecContext(ctx, legacySchema); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := instanceStatus(t, store)
	if err != nil || !state.RestoreDeletionReconciliationRequired || state.DispatchEnabled || state.DerivedRetrievalEnabled {
		t.Fatalf("legacy prepared restore was not backfilled closed: state=%#v err=%v", state, err)
	}
}

func TestRestoreGateFailsClosedWhenInstanceSingletonIsMissing(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `DELETE FROM zoe_instance_state WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err == nil || errors.Is(err, storage.ErrRestoreDeletionReconciliationRequired) {
		t.Fatalf("missing instance singleton did not fail closed with a state error: %v", err)
	}
}

func TestIsolationProofRejectsUnverifiedAndActiveSource(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	good := signedProof(t, key, "source", 4)
	var envelope isolationProofEnvelope
	if err := json.Unmarshal(good, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyIsolationProof([]byte(`{"payload":"`+envelope.Payload+`","hmac":"deadbeef"}`), key); err == nil {
		t.Fatal("forged proof HMAC was accepted")
	}

	payload := map[string]any{
		"schema_version": 1, "old_instance_id": "source", "old_epoch": 4,
		"source_host": "source.example", "units": []map[string]string{{"name": "zoe.service", "active_state": "active", "unit_file_state": "masked"}},
		"active_runs": 0, "unknown_effects": 0, "verified_at": time.Now().UTC(),
	}
	raw, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	active := isolationProofEnvelope{Payload: base64.RawURLEncoding.EncodeToString(raw), HMAC: hex.EncodeToString(mac.Sum(nil))}
	doc, _ := json.Marshal(active)
	if _, err := VerifyIsolationProof(doc, key); err == nil {
		t.Fatal("active systemd unit proof was accepted")
	}

	readiness := map[string]any{
		"schema_version": 1, "old_instance_id": "source", "old_epoch": 4,
		"source_host": "source.example", "units": []map[string]string{{"name": "zoe.service", "active_state": "inactive", "unit_file_state": "masked"}},
		"active_runs": 0, "unknown_effects": 0, "active_deployment_jobs": 1,
		"live_deployments": 0, "open_voice_sessions": 0, "verified_at": time.Now().UTC(),
	}
	raw, _ = json.Marshal(readiness)
	mac = hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	blocked := isolationProofEnvelope{Payload: base64.RawURLEncoding.EncodeToString(raw), HMAC: hex.EncodeToString(mac.Sum(nil))}
	doc, _ = json.Marshal(blocked)
	if _, err := VerifyIsolationProof(doc, key); err == nil {
		t.Fatal("proof with active deployment work was accepted")
	}
}

func instanceStatus(t *testing.T, store *storage.Store) (InstanceState, error) {
	t.Helper()
	tx, err := store.DB().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InstanceState{}, err
	}
	defer tx.Rollback()
	return GetInstanceState(context.Background(), tx)
}

func signedProof(t *testing.T, key []byte, oldID string, oldEpoch int64) []byte {
	t.Helper()
	payload := map[string]any{
		"schema_version": 1, "old_instance_id": oldID, "old_epoch": oldEpoch,
		"source_host": "source.example", "units": []map[string]string{{"name": "zoe.service", "active_state": "inactive", "unit_file_state": "masked"}},
		"active_runs": 0, "unknown_effects": 0, "verified_at": time.Now().UTC(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	doc, err := json.Marshal(isolationProofEnvelope{Payload: base64.RawURLEncoding.EncodeToString(raw), HMAC: hex.EncodeToString(mac.Sum(nil))})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
