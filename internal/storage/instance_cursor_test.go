package storage

import (
	"context"
	"testing"
)

func TestInstanceCursorAndOwnerIdempotencyBaseline(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.DB().Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if table == "workspaces" {
			t.Fatal("product tenancy table remains")
		}
		var tenancyColumns int
		if err = store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name IN ('workspace_id','workspace_ids')`, table).Scan(&tenancyColumns); err != nil {
			t.Fatal(err)
		}
		if tenancyColumns != 0 {
			t.Fatalf("%s retains product tenancy", table)
		}
	}
	// A fresh baseline has exactly one cursor authority, not one per namespace.
	var cursor int64
	if err = store.DB().QueryRow(`SELECT current_cursor FROM event_counters WHERE singleton=1`).Scan(&cursor); err != nil || cursor != 0 {
		t.Fatalf("fresh cursor=%d err=%v", cursor, err)
	}
	if _, err = store.DB().Exec(`INSERT INTO event_counters(singleton,current_cursor) VALUES(2,0)`); err == nil {
		t.Fatal("second cursor authority accepted")
	}
	if err = store.DB().QueryRow(`UPDATE event_counters SET current_cursor=current_cursor+1 WHERE singleton=1 RETURNING current_cursor`).Scan(&cursor); err != nil || cursor != 1 {
		t.Fatalf("next cursor=%d err=%v", cursor, err)
	}
	insertEvent := `INSERT INTO events(event_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,1,1,'test','2026-01-01','task','task',1,'{}')`
	if _, err = store.DB().Exec(insertEvent, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(insertEvent, "duplicate"); err == nil {
		t.Fatal("duplicate instance cursor accepted")
	}
	insertReceipt := `INSERT INTO command_receipts(command_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,'key',X'01','{}','2026-01-01')`
	if _, err = store.DB().Exec(insertReceipt, "first", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(insertReceipt, "duplicate", "owner"); err == nil {
		t.Fatal("duplicate owner idempotency key accepted")
	}
	if _, err = store.DB().Exec(insertReceipt, "another-owner", "another-owner"); err != nil {
		t.Fatalf("independent owner idempotency key rejected: %v", err)
	}
}
