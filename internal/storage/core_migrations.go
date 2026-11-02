package storage

import (
	"context"
	"database/sql"
)

func RegisterCoreMigrations(s *Store) error {
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 1, Name: "core_authority", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL
);
INSERT INTO workspaces(id,name,created_at) VALUES('default','Default',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE TABLE actors (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','approved','revoked')),
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  approved_at TEXT,
  last_seen_at TEXT,
  revoked_at TEXT
);
CREATE INDEX devices_subject ON actors(workspace_id,subject_id,status);
CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('active','archived')),
  version INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(workspace_id,id)
);
CREATE INDEX projects_workspace ON projects(workspace_id,created_at,id);
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  project_id TEXT,
  title TEXT NOT NULL,
  goal TEXT NOT NULL,
  source TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('queued','active','waiting','awaiting_user','paused','complete','failed','canceled')),
  version INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  accepted_at TEXT,
  started_at TEXT,
  finished_at TEXT,
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_by_device_id TEXT NOT NULL REFERENCES actors(id),
  command_id TEXT NOT NULL,
  FOREIGN KEY(workspace_id,project_id) REFERENCES projects(workspace_id,id)
);
CREATE INDEX tasks_workspace ON tasks(workspace_id,created_at,id);
CREATE TABLE task_entries (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  author_device_id TEXT NOT NULL REFERENCES actors(id),
  created_at TEXT NOT NULL
);
CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected','expired','canceled')),
  prompt TEXT NOT NULL,
  task_version INTEGER NOT NULL,
  requested_action BLOB,
  requested_action_digest TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  expires_at TEXT,
  decided_at TEXT,
  decided_by_device_id TEXT REFERENCES actors(id)
);
CREATE INDEX approvals_workspace_status ON approvals(workspace_id,status,created_at);
CREATE TABLE command_receipts (
  command_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash BLOB NOT NULL,
  receipt_json BLOB NOT NULL,
  accepted_at TEXT NOT NULL,
  UNIQUE(workspace_id,subject_id,idempotency_key)
);
CREATE TABLE command_work (
  command_id TEXT PRIMARY KEY REFERENCES command_receipts(command_id),
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE TABLE events (
  event_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  cursor INTEGER NOT NULL,
  schema_version INTEGER NOT NULL,
  event_type TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  subject_id TEXT,
  aggregate_type TEXT NOT NULL,
  aggregate_id TEXT NOT NULL,
  aggregate_version INTEGER NOT NULL,
  payload BLOB NOT NULL,
  delivered_at TEXT,
  UNIQUE(workspace_id,cursor)
);
CREATE TABLE event_counters (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  current_cursor INTEGER NOT NULL DEFAULT 0
);
INSERT INTO event_counters(workspace_id,current_cursor) VALUES('default',0);
CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  attempt_id TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL CHECK(status IN ('queued','dispatching','active','uncertain','stopped','verification_pending','verified','failed')),
  desired_control TEXT NOT NULL DEFAULT 'run' CHECK(desired_control IN ('run','stop')),
  control_version INTEGER NOT NULL DEFAULT 1,
  fence INTEGER NOT NULL UNIQUE,
  runner_id TEXT NOT NULL DEFAULT '',
  authorized_device_id TEXT NOT NULL REFERENCES actors(id),
  command_id TEXT NOT NULL,
  goal TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  runtime_route_id TEXT NOT NULL,
  model_route_id TEXT NOT NULL,
  role TEXT NOT NULL,
  ao_session_id TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  stopped_at TEXT,
  last_observation_at TEXT
);
CREATE INDEX runs_capacity ON runs(status);
CREATE TABLE evidence (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  run_id TEXT REFERENCES runs(id),
  kind TEXT NOT NULL,
  summary TEXT NOT NULL,
  source TEXT NOT NULL,
  provenance BLOB NOT NULL,
  created_at TEXT NOT NULL,
  verified INTEGER NOT NULL DEFAULT 0,
  verifier_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE runner_observations (
  observation_id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL,
  run_id TEXT NOT NULL REFERENCES runs(id),
  attempt_id TEXT NOT NULL,
  fence INTEGER NOT NULL,
  runner_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  body_hash BLOB NOT NULL,
  body BLOB NOT NULL,
  UNIQUE(run_id,runner_id,idempotency_key)
);
CREATE TABLE external_effects (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  run_id TEXT NOT NULL REFERENCES runs(id),
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  external_reference TEXT NOT NULL DEFAULT '',
  first_observed_at TEXT NOT NULL,
  last_observed_at TEXT NOT NULL,
  details BLOB NOT NULL
);
CREATE TABLE runner_credentials (
  token_hash BLOB PRIMARY KEY,
  runner_id TEXT NOT NULL,
  workspace_ids BLOB NOT NULL,
  scopes BLOB NOT NULL,
  revoked_at TEXT
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 2, Name: "run_claim_snapshots_and_notifications", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE runs ADD COLUMN task_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN authorization_json BLOB NOT NULL DEFAULT '{}';
CREATE TABLE notifications (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  source_event_id TEXT NOT NULL REFERENCES events(event_id),
  aggregate_type TEXT NOT NULL,
  aggregate_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at TEXT NOT NULL,
  read_at TEXT,
  UNIQUE(workspace_id,subject_id,source_event_id)
);
CREATE INDEX notifications_inbox ON notifications(workspace_id,subject_id,created_at,id);
ALTER TABLE projects ADD COLUMN owner_subject_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN owner_subject_id TEXT NOT NULL DEFAULT '';
UPDATE projects SET owner_subject_id=coalesce((SELECT e.subject_id FROM events e WHERE e.aggregate_type='project' AND e.aggregate_id=projects.id AND e.event_type='project.created' AND e.subject_id IS NOT NULL ORDER BY e.cursor LIMIT 1),'');
UPDATE tasks SET owner_subject_id=coalesce((SELECT d.subject_id FROM actors d WHERE d.id=tasks.created_by_device_id),'');
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 3, Name: "run_isolation_policy_snapshot", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN isolation_policy TEXT NOT NULL DEFAULT ''`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 4, Name: "child_task_lineage_and_proposals", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE tasks ADD COLUMN lineage_root_task_id TEXT REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN lineage_parent_task_id TEXT REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN lineage_depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN child_policy_json BLOB NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN root_child_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN pregrant_capabilities_json BLOB NOT NULL DEFAULT '[]';
ALTER TABLE runs ADD COLUMN child_policy_json BLOB NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN lineage_root_task_id TEXT REFERENCES tasks(id);
ALTER TABLE runs ADD COLUMN lineage_parent_task_id TEXT REFERENCES tasks(id);
ALTER TABLE runs ADD COLUMN lineage_depth INTEGER NOT NULL DEFAULT 0;
CREATE INDEX tasks_lineage_root ON tasks(lineage_root_task_id,lineage_depth,status);
CREATE TABLE child_proposals (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  attempt_id TEXT NOT NULL,
  proposal_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  parent_task_id TEXT NOT NULL REFERENCES tasks(id),
  root_task_id TEXT NOT NULL REFERENCES tasks(id),
  status TEXT NOT NULL CHECK(status IN ('admitted','pending','approved','rejected','canceled')),
  version INTEGER NOT NULL DEFAULT 1,
  request_hash BLOB NOT NULL,
  request_json BLOB NOT NULL,
  response_json BLOB NOT NULL,
  child_task_id TEXT REFERENCES tasks(id),
  created_at TEXT NOT NULL,
  decided_at TEXT,
  decided_by_device_id TEXT REFERENCES actors(id),
  UNIQUE(run_id,attempt_id,proposal_id),
  UNIQUE(workspace_id,subject_id,proposal_id)
);
CREATE INDEX child_proposals_inbox ON child_proposals(workspace_id,subject_id,status,created_at);
CREATE TABLE task_dependencies (
  task_id TEXT NOT NULL REFERENCES tasks(id),
  dependency_task_id TEXT NOT NULL REFERENCES tasks(id),
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  status TEXT NOT NULL CHECK(status IN ('waiting','satisfied','failed')),
  created_at TEXT NOT NULL,
  PRIMARY KEY(task_id,dependency_task_id),
  CHECK(task_id<>dependency_task_id)
);
CREATE INDEX task_dependencies_dependency ON task_dependencies(dependency_task_id,status);
CREATE TABLE child_budget_reservations (
	  scope_task_id TEXT NOT NULL REFERENCES tasks(id),
  proposal_id TEXT NOT NULL REFERENCES child_proposals(id),
  reserved_at TEXT NOT NULL,
	  PRIMARY KEY(scope_task_id,proposal_id)
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 5, Name: "device_push_tokens_and_delivery", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE device_push_tokens (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  device_id TEXT NOT NULL REFERENCES actors(id),
  installation_id TEXT NOT NULL,
  platform TEXT NOT NULL CHECK(platform IN ('ios','macos')),
  token_hex TEXT NOT NULL,
  token_hash BLOB NOT NULL UNIQUE,
  status TEXT NOT NULL CHECK(status IN ('active','disabled')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  disabled_at TEXT
);
CREATE UNIQUE INDEX device_push_tokens_active_device_platform ON device_push_tokens(installation_id,platform) WHERE status='active';
CREATE INDEX device_push_tokens_subject ON device_push_tokens(workspace_id,subject_id,status);
CREATE TABLE push_deliveries (
  id TEXT PRIMARY KEY,
  notification_id TEXT NOT NULL REFERENCES notifications(id),
  push_token_id TEXT NOT NULL REFERENCES device_push_tokens(id),
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed','disabled')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  apns_id TEXT NOT NULL UNIQUE,
  collapse_id TEXT NOT NULL,
  inbox_cursor TEXT NOT NULL DEFAULT '',
  last_status_code INTEGER NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(notification_id,push_token_id)
);
CREATE INDEX push_deliveries_due ON push_deliveries(status,next_attempt_at,created_at);
CREATE INDEX push_deliveries_token ON push_deliveries(push_token_id,status);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 6, Name: "task_and_run_harness_snapshot", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE tasks ADD COLUMN harness TEXT NOT NULL DEFAULT 'codex';
ALTER TABLE runs ADD COLUMN harness TEXT NOT NULL DEFAULT 'codex';
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 7, Name: "run_budget_snapshot", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN budget_snapshot_json BLOB NOT NULL DEFAULT '{}'`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 8, Name: "client_diagnostic_events", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE client_diagnostic_events (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  subject_id TEXT NOT NULL,
  device_hash TEXT NOT NULL,
  client_request_id TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  app_version TEXT NOT NULL,
  os_version TEXT NOT NULL,
  phase TEXT NOT NULL,
  request_path TEXT NOT NULL,
  http_status INTEGER NOT NULL DEFAULT 0,
  response_bytes INTEGER NOT NULL DEFAULT 0,
  error_type TEXT NOT NULL DEFAULT '',
  error_summary TEXT NOT NULL DEFAULT ''
);
CREATE INDEX client_diagnostic_events_device_time ON client_diagnostic_events(device_hash,received_at);
CREATE INDEX client_diagnostic_events_request ON client_diagnostic_events(client_request_id);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 14, Name: "client_policy_and_diagnostic_level", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE client_diagnostic_events ADD COLUMN level TEXT NOT NULL DEFAULT 'error';
CREATE TABLE client_policy (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  log_level TEXT NOT NULL CHECK(log_level IN ('error','info','debug')),
  poll_interval_seconds INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 11, Name: "server_owned_task_interaction", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE tasks ADD COLUMN interaction_state TEXT NOT NULL DEFAULT 'none' CHECK(interaction_state IN ('none','awaiting_message','explicit_confirmation','approval_required'));
ALTER TABLE tasks ADD COLUMN interaction_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN interaction_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN interaction_action_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN interaction_expires_at TEXT;
ALTER TABLE tasks ADD COLUMN interaction_expected_version INTEGER NOT NULL DEFAULT 0;
CREATE TABLE task_confirmations (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  run_id TEXT NOT NULL,
  action_digest TEXT NOT NULL,
  task_version INTEGER NOT NULL,
  scope TEXT NOT NULL,
  confirmed_text TEXT NOT NULL,
  device_id TEXT NOT NULL REFERENCES actors(id),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX task_confirmations_task ON task_confirmations(workspace_id,task_id,created_at);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := s.RegisterMigration(Migration{Namespace: "core", Version: 15, Name: "pending_runtime_actions", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE tasks ADD COLUMN interaction_question TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN waiting_since TEXT;
CREATE TABLE pending_runtime_actions (
 id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), task_id TEXT NOT NULL REFERENCES tasks(id), run_id TEXT NOT NULL REFERENCES runs(id), fence INTEGER NOT NULL, request_id TEXT NOT NULL, kind TEXT NOT NULL, action TEXT NOT NULL DEFAULT '', resources_json BLOB NOT NULL DEFAULT '[]', question TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '', action_digest TEXT NOT NULL, expires_at TEXT NOT NULL, decision TEXT NOT NULL DEFAULT '', authorization_used INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
 UNIQUE(run_id,request_id)
);`)
		return err
	}}); err != nil {
		return err
	}
	if err := RegisterInstanceAuthMigration(s); err != nil {
		return err
	}
	if err := RegisterRuntimeMigration(s); err != nil {
		return err
	}
	return RegisterRunBudgetMigrations(s)
}
