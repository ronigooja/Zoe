package capabilities

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/internal/storage"
)

func RegisterMigrations(store *storage.Store) error {
	if err := store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 300, Name: "capability_releases_and_deployments_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE capability_releases (
  release_id TEXT PRIMARY KEY,
  digest TEXT NOT NULL,
  manifest BLOB NOT NULL,
  submitted_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(digest)
);
CREATE TABLE capability_deployments (
  deployment_id TEXT PRIMARY KEY,
  service_id TEXT NOT NULL,
  release_digest TEXT NOT NULL,
  requested_operation TEXT NOT NULL CHECK(requested_operation IN ('install','start','stop','pause','resume','update','uninstall')),
  desired_state TEXT NOT NULL CHECK(desired_state IN ('awaiting_authorization','installing','running','stopped','paused','updating','removing')),
  observed_state TEXT NOT NULL CHECK(observed_state IN ('unknown','pending','installing','running','stopped','paused','failed','uninstalling','removed','uncertain')),
  grant_id TEXT NOT NULL DEFAULT '',
  config_ref TEXT NOT NULL DEFAULT '',
  config_sha256 TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  latest_attempt_id TEXT NOT NULL DEFAULT '',
  latest_fence INTEGER NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  UNIQUE(service_id)
);
CREATE TABLE capability_grants (
  grant_id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES capability_deployments(deployment_id),
  release_digest TEXT NOT NULL,
  scope_kind TEXT NOT NULL,
  scope_id TEXT NOT NULL DEFAULT '',
  principal_id TEXT NOT NULL DEFAULT '',
  actions BLOB NOT NULL,
  budget BLOB NOT NULL,
  expires_at TEXT NOT NULL,
  authorized_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE capability_deployment_jobs (
  attempt_id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES capability_deployments(deployment_id),
  fence INTEGER NOT NULL,
  operation TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','claimed','observed','uncertain','failed','superseded')),
  runner_id TEXT NOT NULL DEFAULT '',
  claim_json BLOB NOT NULL,
  superseded_by TEXT NOT NULL DEFAULT '',
  claimed_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(deployment_id,fence)
);
CREATE TABLE capability_observations (
  observation_id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES capability_deployments(deployment_id),
  attempt_id TEXT NOT NULL REFERENCES capability_deployment_jobs(attempt_id),
  fence INTEGER NOT NULL,
  body BLOB NOT NULL,
  observed_at TEXT NOT NULL,
  UNIQUE(deployment_id,attempt_id,fence,observation_id)
);
CREATE INDEX capability_jobs_ready ON capability_deployment_jobs(status,created_at);

`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 301, Name: "capability_triggers_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE capability_trigger_definitions (
  trigger_id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES capability_deployments(deployment_id),
  release_digest TEXT NOT NULL,
  grant_id TEXT NOT NULL REFERENCES capability_grants(grant_id),
  trigger_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('event','schedule')),
  source TEXT NOT NULL,
  mode TEXT NOT NULL CHECK(mode IN ('program','worker','super')),
  scopes BLOB NOT NULL,
  filters BLOB NOT NULL,
  interval_seconds INTEGER NOT NULL DEFAULT 0,
  missed_run_policy TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
  fence_counter INTEGER NOT NULL DEFAULT 0,
  next_run_at TEXT,
  last_run_at TEXT,
  last_run_id TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(deployment_id,release_digest,grant_id,trigger_key)
);
CREATE INDEX capability_triggers_due ON capability_trigger_definitions(enabled,kind,next_run_at);
CREATE TABLE capability_trigger_runs (
  trigger_run_id TEXT PRIMARY KEY,
  trigger_id TEXT NOT NULL REFERENCES capability_trigger_definitions(trigger_id),
  deployment_id TEXT NOT NULL REFERENCES capability_deployments(deployment_id),
  trigger_key TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  event_id TEXT NOT NULL DEFAULT '',
  scheduled_for TEXT NOT NULL DEFAULT '',
  event_payload BLOB NOT NULL DEFAULT '{}',
  mode TEXT NOT NULL CHECK(mode IN ('program','worker','super')),
  state TEXT NOT NULL CHECK(state IN ('queued','claimed','running','waiting','completed','failed','uncertain','canceled')),
  task_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  attempt_id TEXT NOT NULL DEFAULT '',
  fence INTEGER NOT NULL DEFAULT 0,
  runner_id TEXT NOT NULL DEFAULT '',
  claim_json BLOB,
  observation_id TEXT NOT NULL DEFAULT '',
  observation_json BLOB,
  last_error_code TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(trigger_id,dedupe_key)
);
CREATE INDEX capability_trigger_runs_claim ON capability_trigger_runs(state,created_at);
CREATE INDEX capability_trigger_runs_history ON capability_trigger_runs(deployment_id,created_at);
CREATE TABLE capability_trigger_cursors (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  cursor INTEGER NOT NULL DEFAULT 0
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 302, Name: "capability_trigger_deployment_snapshot_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE capability_trigger_definitions ADD COLUMN expected_deployment_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE capability_trigger_definitions ADD COLUMN config_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE capability_trigger_definitions ADD COLUMN config_sha256 TEXT NOT NULL DEFAULT '';
UPDATE capability_trigger_definitions SET enabled=0;`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 303, Name: "capability_grant_authorizing_device_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE capability_grants ADD COLUMN authorized_by_device_id TEXT REFERENCES actors(id);`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 304, Name: "capability_deployment_revocation_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE capability_deployments ADD COLUMN revoked_at TEXT;
ALTER TABLE capability_deployments ADD COLUMN revoked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE capability_deployments ADD COLUMN revoked_by_device_id TEXT REFERENCES actors(id);
ALTER TABLE capability_grants ADD COLUMN revoked_at TEXT;
ALTER TABLE capability_grants ADD COLUMN revoked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE capability_grants ADD COLUMN revoked_by_device_id TEXT REFERENCES actors(id);
ALTER TABLE capability_deployment_jobs ADD COLUMN cleanup_only INTEGER NOT NULL DEFAULT 0 CHECK(cleanup_only IN (0,1));
`)
		return err
	}}); err != nil {
		return err
	}
	return store.RegisterMigration(storage.Migration{Namespace: "capabilities", Version: 305, Name: "capability_revocation_cleanup_state_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE capability_deployments ADD COLUMN revocation_cleanup_state TEXT CHECK(revocation_cleanup_state IS NULL OR revocation_cleanup_state IN ('pending','uncertain','confirmed'));`)
		return err
	}})
}
