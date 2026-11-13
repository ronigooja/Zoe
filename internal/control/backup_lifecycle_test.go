package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/storage"
)

func TestAppendBackupDeleteManifestIsDurableAndIdempotent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "backup-deletions.jsonl")
	record := backupManifestFixture(1, "claim-a")
	if err := AppendBackupDeleteManifest(path, record, []storage.DeletionRecord{record}); err != nil {
		t.Fatal(err)
	}
	if err := AppendBackupDeleteManifest(path, record, []storage.DeletionRecord{record}); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "" || strings.Contains(string(contents), "claim-a") || strings.Contains(string(contents), "fixture-owner") {
		t.Fatalf("manifest leaked deletion entity data: %s", contents)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readBackupDeleteManifest(file)
	_ = file.Close()
	if err != nil || len(rows) != 1 || rows[0].SourceHash != record.Hash {
		t.Fatalf("manifest rows=%#v err=%v", rows, err)
	}
}

func TestAppendBackupDeleteManifestRejectsGapTamperingAndUnsafePath(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "backup-deletions.jsonl")
	if err := AppendBackupDeleteManifest(path, backupManifestFixture(2, "claim-b"), []storage.DeletionRecord{backupManifestFixture(1, "claim-a")}); err == nil {
		t.Fatal("record absent from trusted journal was accepted")
	}
	record := backupManifestFixture(1, "claim-a")
	if err := AppendBackupDeleteManifest("relative.jsonl", record, []storage.DeletionRecord{record}); err == nil {
		t.Fatal("relative manifest path was accepted")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendBackupDeleteManifest(path, record, []storage.DeletionRecord{record}); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
}

func TestAppendBackupDeleteManifestRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target.jsonl")
	link := filepath.Join(directory, "backup-deletions.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	record := backupManifestFixture(1, "claim-a")
	if err := AppendBackupDeleteManifest(link, record, []storage.DeletionRecord{record}); err == nil {
		t.Fatal("symlink manifest path was accepted")
	}
}

func backupManifestFixture(sequence int64, claimID string) storage.DeletionRecord {
	return storage.DeletionRecord{
		Sequence: sequence, LineageID: "fixture-lineage", Generation: 1,
		WorkspaceID: "default", EntityKind: "test_record", EntityID: claimID,
		EntityVersion: sequence + 1, ActorID: "fixture-owner", ReasonCode: "user_requested",
		DeletedAt: "2026-10-22T00:00:00Z", CleanupTargets: []string{"backup_delete_manifest"},
		Hash: "fixture-journal-hash",
	}
}
