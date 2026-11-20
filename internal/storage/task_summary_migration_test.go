package storage

import "testing"

func TestTaskSummaryMigration(t *testing.T) {
	store, err := Open(t.Context(), Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var kind string
	var notNull int
	var noDefault bool
	if err := store.DB().QueryRow(`SELECT type,"notnull",dflt_value IS NULL FROM pragma_table_info('tasks') WHERE name='summary_result'`).Scan(&kind, &notNull, &noDefault); err != nil {
		t.Fatal(err)
	}
	if kind != "TEXT" || notNull != 0 || !noDefault {
		t.Fatalf("summary column type=%s notnull=%d noDefault=%t", kind, notNull, noDefault)
	}
}
