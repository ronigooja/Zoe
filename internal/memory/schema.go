package memory

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
)

// RegisterMigrations keeps a separately ordered migration stream for memory.
func RegisterMigrations(store *storage.Store) error {
	// Every memory-aware opener must recognize worker ingestion state, including
	// offline key rotation, maintenance, and restore paths.
	if err := memoryapi.RegisterMigrations(store); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{
		Namespace: "memory",
		Version:   100,
		Name:      "memory_authority_v1",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
CREATE TABLE memory_sources (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  version INTEGER NOT NULL CHECK(version > 0),
  scope_kind TEXT NOT NULL CHECK(scope_kind IN ('personal','workspace','project','task')),
  scope_id TEXT NOT NULL DEFAULT '',
  principal_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  reference TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  recorded_at TEXT NOT NULL,
  producer TEXT NOT NULL,
  deleted_at TEXT,
  PRIMARY KEY(workspace_id,id,version),
  CHECK((scope_kind='personal' AND scope_id='' AND principal_id!='') OR
        (scope_kind='workspace' AND scope_id='' AND principal_id='') OR
        (scope_kind IN ('project','task') AND scope_id!='' AND principal_id=''))
);
CREATE INDEX memory_sources_reference ON memory_sources(workspace_id,reference,version);
CREATE TABLE memory_claims (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  scope_kind TEXT NOT NULL CHECK(scope_kind IN ('personal','workspace','project','task')),
  scope_id TEXT NOT NULL DEFAULT '',
  principal_id TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL,
  claim_key TEXT NOT NULL,
  authority_kind TEXT NOT NULL CHECK(authority_kind IN ('user_confirmed','execution_observed','imported_authoritative','model_inferred')),
  status TEXT NOT NULL CHECK(status IN ('active','candidate','conflicted','superseded','retracted','tombstoned')),
  revision INTEGER NOT NULL CHECK(revision > 0),
  valid_from TEXT,
  valid_to TEXT,
  supersedes_id TEXT REFERENCES memory_claims(id),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT,
  CHECK((scope_kind='personal' AND scope_id='' AND principal_id!='') OR
        (scope_kind='workspace' AND scope_id='' AND principal_id='') OR
        (scope_kind IN ('project','task') AND scope_id!='' AND principal_id=''))
);
CREATE INDEX memory_claims_scope ON memory_claims(workspace_id,scope_kind,scope_id,principal_id,status);
CREATE INDEX memory_claims_identity ON memory_claims(workspace_id,scope_kind,scope_id,principal_id,subject,claim_key,status);
CREATE TABLE memory_revisions (
  claim_id TEXT NOT NULL REFERENCES memory_claims(id),
  revision INTEGER NOT NULL,
  content TEXT NOT NULL,
  authority_kind TEXT NOT NULL,
  status TEXT NOT NULL,
  recorded_at TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  extractor_version TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(claim_id,revision)
);
CREATE TABLE memory_revision_sources (
  claim_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  workspace_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  source_version INTEGER NOT NULL,
  PRIMARY KEY(claim_id,revision,source_id,source_version),
  FOREIGN KEY(claim_id,revision) REFERENCES memory_revisions(claim_id,revision),
  FOREIGN KEY(workspace_id,source_id,source_version) REFERENCES memory_sources(workspace_id,id,version)
);
CREATE INDEX memory_revision_sources_source ON memory_revision_sources(workspace_id,source_id,source_version);
CREATE TABLE memory_terms (
  workspace_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  term TEXT NOT NULL,
  PRIMARY KEY(workspace_id,claim_id,revision,term),
  FOREIGN KEY(claim_id,revision) REFERENCES memory_revisions(claim_id,revision) ON DELETE CASCADE
);
CREATE INDEX memory_terms_lookup ON memory_terms(workspace_id,term,claim_id,revision);
CREATE TABLE memory_proposals (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  version INTEGER NOT NULL DEFAULT 1,
  source_id TEXT NOT NULL,
  source_version INTEGER NOT NULL,
  extractor_version TEXT NOT NULL,
  proposed_scope_kind TEXT NOT NULL,
  proposed_scope_id TEXT NOT NULL DEFAULT '',
  proposed_principal_id TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL,
  claim_key TEXT NOT NULL,
  content TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','accepted','rejected','stale','deleted')),
  baseline_claim_id TEXT,
  baseline_revision INTEGER,
  created_at TEXT NOT NULL,
  decided_at TEXT,
  UNIQUE(workspace_id,source_id,source_version,extractor_version,subject,claim_key),
  FOREIGN KEY(workspace_id,source_id,source_version) REFERENCES memory_sources(workspace_id,id,version)
);
CREATE INDEX memory_proposals_pending ON memory_proposals(workspace_id,status,created_at);
CREATE TABLE memory_tombstones (
  claim_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  revision INTEGER NOT NULL,
  actor_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);
CREATE TABLE memory_jobs (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  kind TEXT NOT NULL CHECK(kind IN ('extract','index','delete_cleanup')),
  entity_id TEXT NOT NULL,
  entity_version INTEGER NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  payload BLOB NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','leased','retry','done','blocked')),
  attempts INTEGER NOT NULL DEFAULT 0,
  available_at TEXT NOT NULL,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until TEXT,
  last_error_code TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(workspace_id,kind,entity_id,entity_version,target)
);
CREATE INDEX memory_jobs_ready ON memory_jobs(status,available_at,created_at);
CREATE TABLE memory_context_manifests (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL DEFAULT '',
  recipient_id TEXT NOT NULL,
  manifest BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX memory_context_workspace ON memory_context_manifests(workspace_id,created_at);
`)
			return err
		},
	}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{
		Namespace: "memory",
		Version:   101,
		Name:      "memory_model_call_ledger_v1",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
CREATE TABLE memory_model_calls (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  job_id TEXT NOT NULL REFERENCES memory_jobs(id),
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  request_key TEXT NOT NULL,
  runner_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  estimated_input_tokens INTEGER NOT NULL CHECK(estimated_input_tokens > 0),
  max_output_tokens INTEGER NOT NULL CHECK(max_output_tokens > 0),
  reserved_tokens INTEGER NOT NULL CHECK(reserved_tokens > 0),
  status TEXT NOT NULL CHECK(status IN ('reserved','finalized')),
  outcome TEXT NOT NULL DEFAULT '',
  usage_status TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER,
  output_tokens INTEGER,
  provider_request_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finalized_at TEXT,
  UNIQUE(workspace_id,job_id,request_key)
);
CREATE INDEX memory_model_calls_job ON memory_model_calls(workspace_id,job_id,created_at,id);
`)
			return err
		},
	}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{
		Namespace: "memory",
		Version:   102,
		Name:      "memory_claim_deletion_ledger_v1",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			ledgerID := uuid.NewString()
			_, err := tx.ExecContext(ctx, `
CREATE TABLE memory_deletion_ledger_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  ledger_id TEXT NOT NULL UNIQUE,
  ledger_generation INTEGER NOT NULL CHECK(ledger_generation=1),
  baseline_kind TEXT NOT NULL CHECK(baseline_kind='legacy_snapshot_only'),
  initialized_at TEXT NOT NULL
);
CREATE TABLE memory_deletion_events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  ledger_generation INTEGER NOT NULL CHECK(ledger_generation=1),
  event_type TEXT NOT NULL CHECK(event_type='claim_deleted'),
  workspace_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  deleted_at TEXT NOT NULL,
  UNIQUE(workspace_id,claim_id,revision)
);
CREATE TRIGGER memory_deletion_ledger_state_no_update
BEFORE UPDATE ON memory_deletion_ledger_state BEGIN
  SELECT RAISE(ABORT, 'memory deletion ledger identity is immutable');
END;
CREATE TRIGGER memory_deletion_ledger_state_no_delete
BEFORE DELETE ON memory_deletion_ledger_state BEGIN
  SELECT RAISE(ABORT, 'memory deletion ledger identity is immutable');
END;
CREATE TRIGGER memory_deletion_events_no_update
BEFORE UPDATE ON memory_deletion_events BEGIN
  SELECT RAISE(ABORT, 'memory deletion ledger events are append-only');
END;
CREATE TRIGGER memory_deletion_events_no_delete
BEFORE DELETE ON memory_deletion_events BEGIN
  SELECT RAISE(ABORT, 'memory deletion ledger events are append-only');
END;
`)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO memory_deletion_ledger_state(singleton,ledger_id,ledger_generation,baseline_kind,initialized_at) VALUES(1,?,1,'legacy_snapshot_only',strftime('%Y-%m-%dT%H:%M:%fZ','now'))`, ledgerID)
			return err
		},
	}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{
		Namespace: "memory",
		Version:   103,
		Name:      "memory_deletion_lineage_and_target_versions_v1",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
ALTER TABLE memory_deletion_events ADD COLUMN instance_lineage_id TEXT;
ALTER TABLE memory_deletion_events ADD COLUMN instance_generation INTEGER;
ALTER TABLE memory_deletion_events ADD COLUMN entity_kind TEXT;
ALTER TABLE memory_deletion_events ADD COLUMN entity_target_version INTEGER;
ALTER TABLE memory_deletion_events ADD COLUMN instance_deletion_sequence INTEGER;
CREATE INDEX memory_deletion_events_lineage ON memory_deletion_events(instance_lineage_id,instance_generation,sequence);
`)
			return err
		},
	}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "memory", Version: 104, Name: "memory_batch_boundary", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE memory_batch_boundaries (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), principal_id TEXT NOT NULL,
boundary_ns INTEGER NOT NULL DEFAULT 0, next_attempt_ms INTEGER NOT NULL DEFAULT 0,
lease_token TEXT NOT NULL DEFAULT '', lease_until_ms INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY(workspace_id,principal_id));
CREATE TABLE memory_batch_inputs (
workspace_id TEXT NOT NULL, source_id TEXT NOT NULL, source_version INTEGER NOT NULL,
input BLOB NOT NULL, PRIMARY KEY(workspace_id,source_id,source_version),
FOREIGN KEY(workspace_id,source_id,source_version) REFERENCES memory_sources(workspace_id,id,version));
CREATE TRIGGER memory_batch_inputs_delete_source AFTER UPDATE OF deleted_at ON memory_sources
WHEN NEW.deleted_at IS NOT NULL BEGIN
DELETE FROM memory_batch_inputs WHERE workspace_id=NEW.workspace_id AND source_id=NEW.id AND source_version=NEW.version;
END;`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "memory", Version: 105, Name: "memory_batch_trusted_revisions", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE memory_batch_claims (
workspace_id TEXT NOT NULL, claim_id TEXT NOT NULL, revision INTEGER NOT NULL,
source_id TEXT NOT NULL, source_version INTEGER NOT NULL,
PRIMARY KEY(workspace_id,claim_id,revision),
FOREIGN KEY(claim_id,revision) REFERENCES memory_revisions(claim_id,revision) ON DELETE CASCADE,
FOREIGN KEY(workspace_id,source_id,source_version) REFERENCES memory_sources(workspace_id,id,version));`)
		return err
	}}); err != nil {
		return err
	}
	return store.RegisterMigration(storage.Migration{Namespace: "memory", Version: 106, Name: "memory_staged_writes", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE memory_staged_writes (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), principal_id TEXT NOT NULL,
idempotency_key TEXT NOT NULL, original_text TEXT NOT NULL CHECK(length(original_text) BETWEEN 1 AND 4000),
staged_at INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','processed')),
processed_at INTEGER, priority INTEGER NOT NULL DEFAULT 100,
PRIMARY KEY(workspace_id,principal_id,idempotency_key),
CHECK((status='pending' AND processed_at IS NULL) OR (status='processed' AND processed_at IS NOT NULL)));
CREATE INDEX memory_staged_writes_pending ON memory_staged_writes(workspace_id,principal_id,status,priority DESC,staged_at);`)
		return err
	}})
}
