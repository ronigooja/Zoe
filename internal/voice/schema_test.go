package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/storage"
)

// TestVoiceBaselineSchemaMatchesReference applies to fresh databases only. Its
// canonical testdata reference records the approved fresh baseline, diffed
// against a store with all core dependency migrations already applied.
func TestVoiceBaselineSchemaMatchesReference(t *testing.T) {
	var reference struct {
		Objects map[string][]string `json:"objects"`
		Columns map[string][][]any  `json:"columns"`
	}
	data, err := os.ReadFile("testdata/voice-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][]string {
		t.Helper()
		rows, err := store.DB().Query(`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		objects := map[string][]string{}
		for rows.Next() {
			var kind, name, table string
			var statement sql.NullString
			if err = rows.Scan(&kind, &name, &table, &statement); err != nil {
				t.Fatal(err)
			}
			objects[name] = []string{kind, name, table, strings.Join(strings.Fields(statement.String), " ")}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return objects
	}
	before := snapshot()
	if err = RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	baseline := make(map[string][]string, len(after))
	for name, obj := range after {
		if obj[2] == "voice_context_events" {
			continue
		}
		if strings.HasPrefix(name, "model_config_fingerprints") || strings.HasPrefix(name, "sqlite_autoindex_model_config_fingerprints") || strings.HasPrefix(name, "model_retry_state") || strings.HasPrefix(name, "sqlite_autoindex_model_retry_state") || strings.HasPrefix(name, "voice_text_inputs") || strings.HasPrefix(name, "voice_input_notices") || strings.HasPrefix(name, "sqlite_autoindex_voice_text_inputs") || strings.HasPrefix(name, "sqlite_autoindex_voice_input_notices") {
			continue
		}
		copyObj := append([]string(nil), obj...)
		if name == "voice_context_registers" {
			copyObj[3] = strings.Replace(copyObj[3], ", event_watermark INTEGER NOT NULL DEFAULT 0", "", 1)
		}
		if name == "ui_requests" {
			copyObj[3] = strings.Replace(copyObj[3], ", chat_input INTEGER NOT NULL DEFAULT 0 CHECK(chat_input IN (0,1))", "", 1)
		}
		// Task summary is a core-owned additive column, not part of the
		// historical voice baseline captured by this reference.
		if name == "tasks" {
			copyObj[3] = strings.Replace(copyObj[3], ", summary_result TEXT", "", 1)
		}
		baseline[name] = copyObj
	}
	owned := map[string][]string{}
	for name, obj := range baseline {
		if !reflect.DeepEqual(before[name], obj) {
			owned[name] = obj
		}
	}
	for _, obj := range reference.Objects {
		obj[3] = strings.Join(strings.Fields(obj[3]), " ")
	}
	if !reflect.DeepEqual(owned, reference.Objects) {
		t.Fatalf("voice-owned schema differs from reference: got=%v want=%v", owned, reference.Objects)
	}
	for table, want := range reference.Columns {
		rows, err := store.DB().Query(`SELECT name,type,"notnull",dflt_value,pk FROM pragma_table_info(?) WHERE NOT (?='ui_requests' AND name='chat_input') AND NOT (?='tasks' AND name='summary_result') AND NOT (?='voice_context_registers' AND name='event_watermark') ORDER BY cid`, table, table, table, table)
		if err != nil {
			t.Fatal(err)
		}
		var got [][]any
		for rows.Next() {
			var name, kind string
			var notnull, pk int
			var def sql.NullString
			if err = rows.Scan(&name, &kind, &notnull, &def, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			var defaultValue any
			if def.Valid {
				defaultValue = def.String
			}
			got = append(got, []any{name, kind, float64(notnull), defaultValue, float64(pk)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s columns: got=%v want=%v", table, got, want)
		}
	}
	var version, count int
	var name string
	if err = store.DB().QueryRow(`SELECT max(version),count(*),max(name) FROM schema_migrations WHERE namespace='voice'`).Scan(&version, &count, &name); err != nil || version != 207 || count != 8 || name != "voice_baseline" {
		t.Fatalf("voice receipt=%d/%d/%s err=%v", version, count, name, err)
	}
	var changesBefore, changesAfter int
	if err = store.DB().QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	if changesBefore != changesAfter || !reflect.DeepEqual(after, snapshot()) {
		t.Fatal("second migrate changed database")
	}
}

func TestUISurfaceRetentionMigrationPreservesDocuments(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Exercise the actual legacy constraints and primary key without unrelated
	// ownership tables; the production migration runner supplies the transaction.
	_, err = store.DB().Exec(`CREATE TABLE ui_surfaces (
workspace_id TEXT NOT NULL, owner_subject_id TEXT NOT NULL,
surface_id TEXT NOT NULL, container TEXT NOT NULL CHECK(container IN ('popover','window')),
state_json TEXT NOT NULL CHECK(json_valid(state_json) AND json_type(state_json)='array'),
width INTEGER, height INTEGER,
version INTEGER NOT NULL CHECK(version >= 0), created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
request_id TEXT, deleted INTEGER NOT NULL DEFAULT 0 CHECK(deleted IN (0,1)),
PRIMARY KEY(workspace_id,owner_subject_id,surface_id),
CHECK((width IS NULL AND height IS NULL) OR (width IS NOT NULL AND height IS NOT NULL)));
INSERT INTO ui_surfaces VALUES('w','o','keep','window','[]',400,300,1,'now','now','request',0);
INSERT INTO ui_surfaces VALUES('w','o','remove','window','[]',NULL,NULL,2,'now','now','old',1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 205, Name: "ui_surface_retention", Up: migrateUISurfaceRetention}); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('ui_surfaces') WHERE name='deleted'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("removed column count=%d err=%v", count, err)
	}
	var id, raw string
	var width, height, version int
	if err = store.DB().QueryRow(`SELECT surface_id,state_json,width,height,version FROM ui_surfaces`).Scan(&id, &raw, &width, &height, &version); err != nil || id != "keep" || raw != "[]" || width != 400 || height != 300 || version != 1 {
		t.Fatalf("document=%s/%s/%d/%d/%d err=%v", id, raw, width, height, version, err)
	}
	if err = store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained=%d err=%v", count, err)
	}
	for _, statement := range []string{`UPDATE ui_surfaces SET container='invalid'`, `UPDATE ui_surfaces SET state_json='{}'`, `UPDATE ui_surfaces SET version=-1`, `UPDATE ui_surfaces SET width=NULL`, `INSERT INTO ui_surfaces SELECT * FROM ui_surfaces`} {
		if _, err = store.DB().Exec(statement); err == nil {
			t.Fatalf("lost constraint: %s", statement)
		}
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
