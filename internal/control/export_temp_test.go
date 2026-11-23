package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

func exportTempTestStore(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestInMemoryExportTempRootExistsAndIsReused(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	path, err := exportTempRootPath(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		inMemoryExportRoots.Delete(store)
		_ = os.RemoveAll(path)
	})
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("expected existing private export directory, info=%v err=%v", info, err)
	}
	marker := filepath.Join(path, "marker")
	if err := os.WriteFile(marker, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := exportTempRootPath(store)
	if err != nil || again != path {
		t.Fatalf("root changed: got %q, want %q, err=%v", again, path, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reused export root lost its contents: %v", err)
	}
}

func addExportTempFixture(t *testing.T, store *storage.Store, id, owner string, sequence int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO zoe_export_temp_registry(export_id,owner_id,export_scope,created_deletion_sequence,status,invalidation_sequence,created_at,updated_at) VALUES(?,?,'instance',?,'active',0,?,?)`, id, owner, sequence, now, now); err != nil {
		t.Fatal(err)
	}
}

func createExportTempFixture(t *testing.T, store *storage.Store, id string) string {
	t.Helper()
	root, err := openPrivateExportRoot(store, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{exportDataName(id), exportArchiveName(id)} {
		file, openErr := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, openErr = file.Write([]byte("fixture")); openErr != nil {
			t.Fatal(openErr)
		}
		if openErr = file.Close(); openErr != nil {
			t.Fatal(openErr)
		}
	}
	path, err := exportTempRootPath(store)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExportTempCleanupIsIdempotentAndInstanceSequenceScoped(t *testing.T) {
	store := exportTempTestStore(t)
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	addExportTempFixture(t, store, "owner-a-old", "owner-a", 1)
	addExportTempFixture(t, store, "owner-b-old", "owner-b", 1)
	addExportTempFixture(t, store, "current-export", "owner-b", 2)
	rootPath := createExportTempFixture(t, store, "owner-a-old")
	createExportTempFixture(t, store, "owner-b-old")
	createExportTempFixture(t, store, "current-export")
	done, code, err := server.cleanExportTemps(context.Background(), 2)
	if err != nil || !done || code != "" {
		t.Fatalf("first cleanup completed=%t code=%q err=%v", done, code, err)
	}
	for _, id := range []string{"owner-a-old", "owner-b-old"} {
		for _, name := range []string{exportDataName(id), exportArchiveName(id)} {
			if _, err = os.Stat(filepath.Join(rootPath, name)); !os.IsNotExist(err) {
				t.Fatalf("stale artifact %s remains or stat failed: %v", name, err)
			}
		}
	}
	if _, err = os.Stat(filepath.Join(rootPath, exportDataName("current-export"))); err != nil {
		t.Fatalf("current export artifact was touched: %v", err)
	}
	if done, code, err = server.cleanExportTemps(context.Background(), 2); err != nil || !done || code != "" {
		t.Fatalf("idempotent cleanup completed=%t code=%q err=%v", done, code, err)
	}
	var otherStatus string
	if err = store.DB().QueryRow(`SELECT status FROM zoe_export_temp_registry WHERE export_id='current-export'`).Scan(&otherStatus); err != nil || otherStatus != "active" {
		t.Fatalf("current export registry status=%q err=%v", otherStatus, err)
	}
	var checkpoint int64
	if err = store.DB().QueryRow(`SELECT completed_sequence FROM zoe_deletion_reconciliation_checkpoints WHERE target='export_temp'`).Scan(&checkpoint); err != nil || checkpoint != 0 {
		t.Fatalf("file cleanup alone advanced reconciliation checkpoint=%d err=%v", checkpoint, err)
	}
}

func TestExportTempCleanupReturnsPendingForActiveExporter(t *testing.T) {
	store := exportTempTestStore(t)
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	addExportTempFixture(t, store, "active-export", "default", 1)
	rootPath := createExportTempFixture(t, store, "active-export")
	lock, ok := acquireExportTempLock("active-export", true)
	if !ok {
		t.Fatal("could not acquire test exporter lock")
	}
	done, code, err := server.cleanExportTemps(context.Background(), 2)
	if err != nil || done || code != "export_active" {
		t.Fatalf("active cleanup completed=%t code=%q err=%v", done, code, err)
	}
	releaseExportTempLock("active-export", lock)
	if _, err = os.Stat(filepath.Join(rootPath, exportDataName("active-export"))); err != nil {
		t.Fatalf("pending cleanup removed an active artifact: %v", err)
	}
	if done, code, err = server.cleanExportTemps(context.Background(), 2); err != nil || !done || code != "" {
		t.Fatalf("retry cleanup completed=%t code=%q err=%v", done, code, err)
	}
}

func TestExportTempCleanupFailsClosedForUnregisteredOrUnsafeArtifacts(t *testing.T) {
	t.Run("unregistered artifact", func(t *testing.T) {
		store := exportTempTestStore(t)
		server, err := NewServer(store, Config{})
		if err != nil {
			t.Fatal(err)
		}
		rootPath := createExportTempFixture(t, store, "registered-export")
		root, err := openPrivateExportRoot(store, false)
		if err != nil {
			t.Fatal(err)
		}
		file, err := root.OpenFile("unregistered.bin", os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		_ = root.Close()
		addExportTempFixture(t, store, "registered-export", "default", 1)
		if done, code, err := server.cleanExportTemps(context.Background(), 2); err == nil || done || code != "adapter_missing" {
			t.Fatalf("unregistered cleanup completed=%t code=%q err=%v", done, code, err)
		}
		if _, err = os.Stat(filepath.Join(rootPath, "unregistered.bin")); err != nil {
			t.Fatalf("unregistered artifact was removed: %v", err)
		}
	})

	t.Run("symlink root", func(t *testing.T) {
		store := exportTempTestStore(t)
		server, err := NewServer(store, Config{})
		if err != nil {
			t.Fatal(err)
		}
		addExportTempFixture(t, store, "symlink-export", "default", 1)
		rootPath, err := exportTempRootPath(store)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(t.TempDir(), rootPath); err != nil {
			t.Fatal(err)
		}
		if done, code, err := server.cleanExportTemps(context.Background(), 2); err == nil || done || code != "unsafe_private_root" {
			t.Fatalf("symlink cleanup completed=%t code=%q err=%v", done, code, err)
		}
	})

	t.Run("symlink artifact", func(t *testing.T) {
		store := exportTempTestStore(t)
		server, err := NewServer(store, Config{})
		if err != nil {
			t.Fatal(err)
		}
		addExportTempFixture(t, store, "symlink-artifact", "default", 1)
		rootPath := createExportTempFixture(t, store, "symlink-artifact")
		outside := filepath.Join(t.TempDir(), "outside.txt")
		if err = os.WriteFile(outside, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(filepath.Join(rootPath, exportDataName("symlink-artifact"))); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(outside, filepath.Join(rootPath, exportDataName("symlink-artifact"))); err != nil {
			t.Fatal(err)
		}
		if done, code, err := server.cleanExportTemps(context.Background(), 2); err == nil || done || code != "unsafe_or_unremovable_artifact" {
			t.Fatalf("symlink artifact cleanup completed=%t code=%q err=%v", done, code, err)
		}
		if payload, err := os.ReadFile(outside); err != nil || string(payload) != "keep" {
			t.Fatalf("symlink target was changed: data=%q err=%v", payload, err)
		}
	})
}
