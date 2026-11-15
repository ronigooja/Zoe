package storage

import "testing"

func TestClientPolicyMigrationLevelConstraint(t *testing.T) {
	store, err := Open(t.Context(), Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"error", "warn", "info", "debug", "verbose"} {
		_, err := store.DB().Exec(`INSERT OR REPLACE INTO client_policy(singleton,log_level,poll_interval_seconds,revision,updated_at) VALUES(1,?,60,1,'2026-11-15T00:00:00Z')`, level)
		if level == "verbose" {
			if err == nil {
				t.Fatal("invalid policy level passed CHECK constraint")
			}
		} else if err != nil {
			t.Fatalf("level=%s err=%v", level, err)
		}
	}
}
