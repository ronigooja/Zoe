package voice

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/internal/storage"
)

func RegisterMigrations(store *storage.Store) error {
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 200, Name: "voice_baseline", Up: func(ctx context.Context, tx *sql.Tx) error {
		// Core creates tasks before this baseline; voice owns its display flag.
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='ui_display')`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN ui_display INTEGER NOT NULL DEFAULT 0 CHECK(ui_display IN (0,1))`); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
CREATE TABLE voice_sessions (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  owner_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  request_hash BLOB NOT NULL,
  gateway_session_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL CHECK(generation > 0),
  state TEXT NOT NULL CHECK(state IN ('allocating','active','ending','ended','failed','quarantined')),
  offer_answer BLOB NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  heartbeat_at TEXT,
  ended_at TEXT,
  last_error_code TEXT NOT NULL DEFAULT '', account_lease_id TEXT NOT NULL DEFAULT '', account_lease_generation INTEGER NOT NULL DEFAULT 0, account_id_hash BLOB NOT NULL DEFAULT '', gateway_unbound INTEGER NOT NULL DEFAULT 0 CHECK(gateway_unbound IN (0,1)), gateway_unbind_unknown INTEGER NOT NULL DEFAULT 0 CHECK(gateway_unbind_unknown IN (0,1)), gateway_media_generation INTEGER NOT NULL DEFAULT 0, opening_context_delivery TEXT NOT NULL DEFAULT 'unsupported' CHECK(opening_context_delivery IN ('delivered','unverified','unsupported')), opening_context_version INTEGER NOT NULL DEFAULT 0, opening_context_chars INTEGER NOT NULL DEFAULT 0, opening_context_manifest BLOB NOT NULL DEFAULT '', gateway_context_chars INTEGER NOT NULL DEFAULT 0, gateway_request_id TEXT NOT NULL DEFAULT '',
  UNIQUE(workspace_id,owner_id,request_id)
);
CREATE UNIQUE INDEX voice_sessions_owner_lease ON voice_sessions(workspace_id,owner_id)
  WHERE state IN ('allocating','active','ending');
CREATE INDEX voice_sessions_device ON voice_sessions(workspace_id,device_id,state);
CREATE TABLE voice_turns (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  turn_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  provider_message_id TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  content_hash BLOB NOT NULL,
  confirmed_at TEXT NOT NULL,
  command_id TEXT NOT NULL, resumed_task_id TEXT NOT NULL DEFAULT '', handoff_handled INTEGER NOT NULL DEFAULT 0, steward_action TEXT NOT NULL DEFAULT '', steward_task_id TEXT NOT NULL DEFAULT '', report_text TEXT NOT NULL DEFAULT '', exits TEXT, ui_handled INTEGER NOT NULL DEFAULT 0 CHECK(ui_handled IN (0,1)), request TEXT, steward_question_id TEXT NOT NULL DEFAULT '', steward_task_version INTEGER NOT NULL DEFAULT 0, steward_snapshot TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id,generation,turn_id,revision)
);
CREATE TABLE voice_task_inputs (
 session_id TEXT NOT NULL REFERENCES voice_sessions(id), generation INTEGER NOT NULL, turn_id TEXT NOT NULL,
 workspace_id TEXT NOT NULL REFERENCES workspaces(id), turn_revision INTEGER NOT NULL,
 task_id TEXT NOT NULL REFERENCES tasks(id), entry_id TEXT NOT NULL REFERENCES task_entries(id),
 question_id TEXT REFERENCES pending_runtime_actions(id), content_hash BLOB NOT NULL,
 PRIMARY KEY(session_id,generation,turn_id)
);
CREATE INDEX voice_turns_session ON voice_turns(session_id,generation,confirmed_at);
CREATE TABLE voice_task_links (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  turn_id TEXT NOT NULL,
  turn_revision INTEGER NOT NULL,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  linked_by_device_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  linked_at TEXT NOT NULL,
  PRIMARY KEY(session_id,generation,turn_id,turn_revision),
  UNIQUE(workspace_id,task_id)
);
CREATE INDEX voice_task_links_task ON voice_task_links(workspace_id,task_id);
CREATE UNIQUE INDEX voice_task_links_stable_turn ON voice_task_links(session_id,generation,turn_id);
CREATE TABLE voice_pool_state (
  id INTEGER PRIMARY KEY CHECK(id=1),
  blocked INTEGER NOT NULL CHECK(blocked IN (0,1)),
  reason_code TEXT NOT NULL DEFAULT '',
  source_session_id TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT ''
);
INSERT INTO voice_pool_state(id,blocked,updated_at) VALUES(1,0,strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE TABLE voice_pool_reconciliations (
  id TEXT PRIMARY KEY,
  actor_id TEXT NOT NULL,
  source_session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  reason TEXT NOT NULL,
  method TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE voice_account_terminations (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  account_id_hash BLOB NOT NULL,
  lease_id_hash BLOB NOT NULL,
  evidence_id TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  adapter_kind TEXT NOT NULL
);
CREATE TABLE voice_result_deliveries (
  entry_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed','skipped_no_call','skipped_stale')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error_code TEXT NOT NULL DEFAULT '',
  voice_session_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX voice_result_deliveries_due ON voice_result_deliveries(status,next_attempt_at,created_at);
CREATE TABLE voice_result_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_cursor INTEGER NOT NULL);
INSERT INTO voice_result_cursor(id,last_cursor) VALUES(1,0);
CREATE TABLE voice_fast_replies (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  turn_id TEXT NOT NULL,
  text TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed','skipped')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error_code TEXT NOT NULL DEFAULT '',
  voice_session_id TEXT NOT NULL DEFAULT '',
  voice_generation INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, intent_kind TEXT NOT NULL DEFAULT 'progress',
  UNIQUE(session_id,generation,turn_id)
);
CREATE INDEX voice_fast_replies_due ON voice_fast_replies(status,next_attempt_at,created_at);
CREATE TABLE voice_transcript_turns (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  seq INTEGER NOT NULL CHECK(seq >= 0),
  role TEXT NOT NULL CHECK(role IN ('user','assistant')),
  text TEXT NOT NULL,
  at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  PRIMARY KEY(session_id,seq)
);
CREATE INDEX voice_transcript_turns_workspace ON voice_transcript_turns(workspace_id,session_id,seq);
CREATE TABLE voice_recall_jobs (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, owner_id TEXT NOT NULL,
session_id TEXT NOT NULL, generation INTEGER NOT NULL, turn_id TEXT NOT NULL,
question TEXT NOT NULL, holding_id TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
UNIQUE(session_id,generation,turn_id));
CREATE TABLE voice_context_registers (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
compact TEXT NOT NULL DEFAULT '', compact_through TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z',
built_at TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z', version INTEGER NOT NULL DEFAULT 0,
source_hash TEXT NOT NULL DEFAULT '', claim_token TEXT NOT NULL DEFAULT '', lease_until_ms INTEGER NOT NULL DEFAULT 0, source_memory_boundary TEXT NOT NULL DEFAULT '',
PRIMARY KEY(workspace_id,owner_subject_id));
CREATE TABLE ui_screens (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
width INTEGER NOT NULL, height INTEGER NOT NULL, updated_at TEXT NOT NULL,
PRIMARY KEY(workspace_id,owner_subject_id));
CREATE TABLE ui_surfaces (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
surface_id TEXT NOT NULL, container TEXT NOT NULL CHECK(container IN ('popover','window')),
state_json TEXT NOT NULL CHECK(json_valid(state_json) AND json_type(state_json)='array'),
width INTEGER, height INTEGER,
version INTEGER NOT NULL CHECK(version >= 0), created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
request_id TEXT REFERENCES ui_requests(id), deleted INTEGER NOT NULL DEFAULT 0 CHECK(deleted IN (0,1)),
PRIMARY KEY(workspace_id,owner_subject_id,surface_id),
CHECK((width IS NULL AND height IS NULL) OR (width IS NOT NULL AND height IS NOT NULL)));
CREATE TABLE ui_requests (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
source TEXT NOT NULL CHECK(source IN ('register','task')), voice_turn_id TEXT REFERENCES voice_turns(id),
task_id TEXT REFERENCES tasks(id), surface_id TEXT,
awaiting_input INTEGER NOT NULL DEFAULT 0 CHECK(awaiting_input IN (0,1)), input_note TEXT,
status TEXT NOT NULL DEFAULT 'generated' CHECK(status IN ('generated','rendered','input_received','failed')),
created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
request TEXT CHECK(request IS NULL OR (length(trim(request))>0 AND trim(request)<>'none')),
question_id TEXT REFERENCES pending_runtime_actions(id), surface_version INTEGER CHECK(surface_version IS NULL OR surface_version>=1), feedback TEXT);
CREATE UNIQUE INDEX ui_requests_voice_turn ON ui_requests(workspace_id,voice_turn_id) WHERE voice_turn_id IS NOT NULL;
CREATE TABLE ui_result_sources (
 request_id TEXT PRIMARY KEY REFERENCES ui_requests(id), workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 task_id TEXT NOT NULL REFERENCES tasks(id), run_id TEXT NOT NULL REFERENCES runs(id),
 entry_id TEXT NOT NULL REFERENCES task_entries(id), event_cursor INTEGER NOT NULL CHECK(event_cursor>0),
 content_hash TEXT NOT NULL
);
CREATE TABLE ui_feedback_deliveries (
request_id TEXT PRIMARY KEY REFERENCES ui_requests(id), workspace_id TEXT NOT NULL REFERENCES workspaces(id),
owner_subject_id TEXT NOT NULL, text TEXT NOT NULL,
status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')),
attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0), next_attempt_at TEXT, last_error TEXT,
created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE ui_jobs (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
kind TEXT NOT NULL CHECK(kind IN ('register','task_entry')), dedupe_key TEXT NOT NULL UNIQUE,
ui_request_id TEXT NOT NULL REFERENCES ui_requests(id), status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','done','failed')),
attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt_at TEXT NOT NULL, last_error TEXT,
created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX ui_jobs_due ON ui_jobs(status,next_attempt_at);
CREATE TABLE ui_task_cursor(workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),last_cursor INTEGER NOT NULL DEFAULT 0);
CREATE INDEX ui_feedback_due ON ui_feedback_deliveries(status,next_attempt_at);
`+ResultCheckpointSchema+TextCheckpointSchema)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 201, Name: "conversation_text_input", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE ui_requests ADD COLUMN voice_input INTEGER NOT NULL DEFAULT 0 CHECK(voice_input IN (0,1));
CREATE TABLE voice_text_inputs (command_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, owner_subject_id TEXT NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')), attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt_at TEXT, last_error TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX voice_text_inputs_due ON voice_text_inputs(status,next_attempt_at);
CREATE TABLE voice_input_notices (kind TEXT NOT NULL CHECK(kind IN ('ui_input_request','ui_input_received')), source_id TEXT NOT NULL, workspace_id TEXT NOT NULL, owner_subject_id TEXT NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')), attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt_at TEXT, last_error TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY(kind,source_id));
CREATE INDEX voice_input_notices_due ON voice_input_notices(status,next_attempt_at);`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 202, Name: "model_retry_state", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE model_retry_state (
			consumer TEXT NOT NULL CHECK(consumer IN ('register','steward','ui_controller','compact','memory_write','memory_ingest')),
			scope TEXT NOT NULL, failures INTEGER NOT NULL CHECK(failures >= 0),
			next_attempt_at TEXT NOT NULL, auth_failed INTEGER NOT NULL CHECK(auth_failed IN (0,1)),
			last_error TEXT NOT NULL CHECK(last_error IN ('','auth','transient','permanent')),
			updated_at TEXT NOT NULL, PRIMARY KEY(consumer,scope))`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 203, Name: "model_config_fingerprints", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE model_config_fingerprints (
			consumer TEXT PRIMARY KEY CHECK(consumer IN ('register','steward','ui_controller','compact','memory_write','memory_ingest')),
			fingerprint TEXT NOT NULL CHECK(length(fingerprint)=64))`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 204, Name: "task_summary_retry_consumer", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE model_retry_state RENAME TO old_model_retry_state;
CREATE TABLE model_retry_state (
consumer TEXT NOT NULL CHECK(consumer IN ('register','steward','ui_controller','compact','task_summary','memory_write','memory_ingest')),
scope TEXT NOT NULL, failures INTEGER NOT NULL CHECK(failures >= 0),
next_attempt_at TEXT NOT NULL, auth_failed INTEGER NOT NULL CHECK(auth_failed IN (0,1)),
last_error TEXT NOT NULL CHECK(last_error IN ('','auth','transient','permanent')),
updated_at TEXT NOT NULL, PRIMARY KEY(consumer,scope));
INSERT INTO model_retry_state SELECT * FROM old_model_retry_state;
DROP TABLE old_model_retry_state;
ALTER TABLE model_config_fingerprints RENAME TO old_model_config_fingerprints;
CREATE TABLE model_config_fingerprints (
consumer TEXT PRIMARY KEY CHECK(consumer IN ('register','steward','ui_controller','compact','task_summary','memory_write','memory_ingest')),
fingerprint TEXT NOT NULL CHECK(length(fingerprint)=64));
INSERT INTO model_config_fingerprints SELECT * FROM old_model_config_fingerprints;
DROP TABLE old_model_config_fingerprints;`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 205, Name: "ui_surface_retention", Up: migrateUISurfaceRetention}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 206, Name: "ui_input_modes", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE ui_requests RENAME COLUMN awaiting_input TO task_question;
ALTER TABLE ui_requests RENAME COLUMN voice_input TO chat_input;`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 207, Name: "conversation_context_events", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, contextEventSchema)
		return err
	}}); err != nil {
		return err
	}
	return store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 208, Name: "opening_result_reservations", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, openingResultSchema)
		return err
	}})
}

func migrateUISurfaceRetention(ctx context.Context, tx *sql.Tx) error {
	// Only the column's own CHECK references it; SQLite removes both together.
	// Remove legacy deletion records before dropping their discriminator.
	_, err := tx.ExecContext(ctx, `DELETE FROM ui_surfaces WHERE deleted=1;
ALTER TABLE ui_surfaces DROP COLUMN deleted;`)
	return err
}
