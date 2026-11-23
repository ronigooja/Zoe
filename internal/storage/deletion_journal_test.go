package storage

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDeletionJournalAppendIsContinuousIdempotentAndTamperEvident(t *testing.T) {
	database := filepath.Join(t.TempDir(), "zoe.sqlite")
	journal, err := NewDeletionJournal(database)
	if err != nil {
		t.Fatal(err)
	}
	record := DeletionRecord{Sequence: 1, LineageID: "lineage-a", Generation: 1, EntityKind: "test_record", EntityID: "claim-a", EntityVersion: 2, ActorID: "owner", ReasonCode: "user_requested", DeletedAt: "2026-10-22T00:00:00Z", CleanupTargets: []string{"mem0", "qdrant"}}
	first, err := journal.Append(record)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := journal.Append(record)
	if err != nil || duplicate.Hash != first.Hash {
		t.Fatalf("idempotent append record=%#v err=%v", duplicate, err)
	}
	gap := record
	gap.Sequence = 3
	gap.EntityID = "claim-c"
	if _, err = journal.Append(gap); err == nil {
		t.Fatal("sequence gap was accepted")
	}
	second := record
	second.Sequence = 2
	second.EntityID = "claim-b"
	second.Generation = 2
	if _, err = journal.Append(second); err != nil {
		t.Fatalf("generation transition append: %v", err)
	}
	got, err := journal.ReadAll()
	if err != nil || len(got) != 2 {
		t.Fatalf("read records=%d err=%v", len(got), err)
	}
	path := database + ".deletion-ledger"
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err = journal.ReadAll(); err == nil {
		t.Fatal("tampered append was accepted")
	}
}

func TestDeletionJournalHashMatchesPythonFixture(t *testing.T) {
	journal, err := NewDeletionJournal(filepath.Join(t.TempDir(), "zoe.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// Keep this body aligned with tools/data_lifecycle/test_restore_external_head.py.
	const body = `{"sequence":1,"lineage_id":"lineage-fixture","generation":3,"entity_kind":"test_record","entity_id":"claim-1","entity_version":2,"actor_id":"owner","reason_code":"user_requested","deleted_at":"2026-10-22T00:00:00Z","cleanup_targets":["claim","qdrant"],"previous_hash":""}`
	record, err := journal.Append(DeletionRecord{
		Sequence: 1, LineageID: "lineage-fixture", Generation: 3,
		EntityKind: "test_record", EntityID: "claim-1", EntityVersion: 2,
		ActorID: "owner", ReasonCode: "user_requested", DeletedAt: "2026-10-22T00:00:00Z",
		CleanupTargets: []string{"claim", "qdrant"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	if record.Hash != want {
		t.Fatalf("hash=%s want=%s", record.Hash, want)
	}
	records, err := journal.ReadAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("read records=%d err=%v", len(records), err)
	}
	if records[0].Hash != want {
		t.Fatalf("read hash=%s want=%s", records[0].Hash, want)
	}
}
