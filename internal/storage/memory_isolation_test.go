package storage

import (
	"context"
	"testing"
)

func TestMemoryStoresAreIsolated(t *testing.T) {
	ctx := context.Background()
	first, err := Open(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := first.DB().Exec(`CREATE TABLE isolation_marker (id INTEGER); INSERT INTO isolation_marker VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	var count int
	if err := second.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='isolation_marker'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("separately opened in-memory store inherited another store's table")
	}
}
