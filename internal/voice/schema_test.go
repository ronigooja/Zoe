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
	owned := map[string][]string{}
	for name, obj := range after {
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
		rows, err := store.DB().Query(`SELECT name,type,"notnull",dflt_value,pk FROM pragma_table_info(?) ORDER BY cid`, table)
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
	if err = store.DB().QueryRow(`SELECT max(version),count(*),max(name) FROM schema_migrations WHERE namespace='voice'`).Scan(&version, &count, &name); err != nil || version != 200 || count != 1 || name != "voice_baseline" {
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
