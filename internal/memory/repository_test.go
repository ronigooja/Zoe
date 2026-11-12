package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func newMemoryTestRepo(t *testing.T) (*storage.Store, *Repository) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store, NewRepository(store.DB())
}

func TestMemoryChineseVersionConflictAndDeletion(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", Scopes: []string{"memory:read:personal", "memory:write:personal", "memory:delete:personal"}}
	scope := Scope{Kind: ScopePersonal, PrincipalID: principal.SubjectID}

	first := writeMemory(t, ctx, store.DB(), repo, scope, "数据库改为 SQLite，因为本地事务需要稳定恢复", "source-1", 0)
	got, err := repo.Search(ctx, principal, "SQLite 原因", SearchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Content != first.Content || len(got.Items[0].Sources) != 1 {
		t.Fatalf("Chinese source-linked search returned %#v", got)
	}
	other := principal
	other.SubjectID = "user-2"
	got, err = repo.Search(ctx, other, "SQLite", SearchFilter{})
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("personal memory crossed subject boundary: result=%#v err=%v", got, err)
	}

	updated := writeMemory(t, ctx, store.DB(), repo, scope, "数据库改为 PostgreSQL，理由在来源中另有记录", "source-2", first.Revision)
	if updated.ID != first.ID || updated.Revision != first.Revision+1 || updated.Status != ClaimActive {
		t.Fatalf("confirmed revision did not update in place: %#v", updated)
	}
	if _, err := writeMemoryErr(ctx, store.DB(), repo, scope, "错误的过期覆盖", "source-stale", first.Revision); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale write error=%v, want ErrVersionConflict", err)
	}

	conflict := writeMemory(t, ctx, store.DB(), repo, scope, "数据库是 MySQL", "source-conflict", 0)
	if conflict.Status != ClaimConflicted {
		t.Fatalf("unversioned conflicting write status=%q", conflict.Status)
	}
	if _, err := writeMemoryErr(ctx, store.DB(), repo, scope, "数据库是 MariaDB", "source-third", 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("third unversioned value bypassed unresolved conflict: err=%v", err)
	}
	got, err = repo.Search(ctx, principal, "数据库", SearchFilter{})
	if err != nil || len(got.Conflicts) != 1 || len(got.Items) < 2 {
		t.Fatalf("conflict was hidden or overwritten: result=%#v err=%v", got, err)
	}

	// Delete both sides of the conflict. Each tombstone blocks retrieval before
	// its derived cleanup jobs have run.
	var latestRevision int64
	if err := store.DB().QueryRowContext(ctx, `SELECT revision FROM memory_claims WHERE id=?`, first.ID).Scan(&latestRevision); err != nil {
		t.Fatal(err)
	}
	deleted, err := runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: "user-1", ClaimID: first.ID, Expected: latestRevision, ReasonCode: "user_requested"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != ClaimTombstoned || deleted.DeletedAt == nil {
		t.Fatalf("delete did not return authoritative tombstone: %#v", deleted)
	}
	if _, err := runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: "user-1", ClaimID: conflict.ID, Expected: conflict.Revision, ReasonCode: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Search(ctx, principal, "PostgreSQL", SearchFilter{})
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("tombstoned claim remained retrievable: result=%#v err=%v", got, err)
	}
	var oldTextCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_revisions WHERE claim_id=? AND content!=''`, first.ID).Scan(&oldTextCount); err != nil {
		t.Fatal(err)
	}
	if oldTextCount != 0 {
		t.Fatalf("deleted revision content remains in authority: count=%d", oldTextCount)
	}
	var jobs int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_jobs WHERE entity_id=? AND kind='delete_cleanup' AND status='pending'`, first.ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 4 {
		t.Fatalf("delete cleanup jobs=%d, want 4 external cleanup targets", jobs)
	}
}

func TestVoiceOutcomeCreatesScopedSourceWithoutPerJobExtraction(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner-1"}
	if err := repo.RecordVoiceOutcome(ctx, p, "voice-1", "The call completed with the agreed plan.", "completed"); err != nil {
		t.Fatal(err)
	}
	var kind, reference, producer, scope, principal string
	if err := store.DB().QueryRow(`SELECT kind,reference,producer,scope_kind,principal_id FROM memory_sources WHERE workspace_id='default'`).Scan(&kind, &reference, &producer, &scope, &principal); err != nil {
		t.Fatal(err)
	}
	if kind != "voice_call_outcome" || reference != "voice_session:voice-1" || producer != "voice" || scope != "personal" || principal != "owner-1" {
		t.Fatalf("source=%q %q %q %q %q", kind, reference, producer, scope, principal)
	}
	var jobs, claims int
	if err := store.DB().QueryRow(`SELECT count(*) FROM memory_jobs WHERE entity_id=(SELECT id FROM memory_sources WHERE workspace_id='default') AND kind='extract' AND target='voice_call_outcome'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM memory_claims WHERE workspace_id='default'`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || claims != 0 {
		t.Fatalf("jobs=%d claims=%d; outcome must enter source/proposal path only", jobs, claims)
	}
}

func TestClaimDeletionInvalidatesWorkspaceContextCacheTransactionally(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewRepositoryWithStore(store)
	scope := Scope{Kind: ScopePersonal, PrincipalID: "cache-user"}
	claim := writeMemory(t, ctx, store.DB(), repo, scope, "cached claim", "cache-source", 0)
	manifest := ContextManifest{ID: "cache-manifest", WorkspaceID: "default", RecipientID: scope.PrincipalID, Entries: []ContextEntry{{ClaimID: claim.ID, Revision: claim.Revision}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO memory_context_manifests(id,workspace_id,task_id,recipient_id,manifest,created_at) VALUES(?,?,'',?,?,?)`, manifest.ID, manifest.WorkspaceID, manifest.RecipientID, body, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO zoe_export_temp_registry(export_id,workspace_id,owner_id,export_scope,created_deletion_sequence,status,invalidation_sequence,created_at,updated_at) VALUES('active-export','default',?,'personal',0,'active',0,?,?)`, scope.PrincipalID, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err = runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: claim.ID, Expected: claim.Revision, ReasonCode: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	var manifests, completed int
	var exportStatus string
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_context_manifests WHERE workspace_id='default'`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT completed_sequence FROM zoe_deletion_reconciliation_checkpoints WHERE target='cache'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if manifests != 0 || completed != 1 {
		t.Fatalf("workspace cache manifests=%d, cache checkpoint=%d; want 0 and 1", manifests, completed)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT status FROM zoe_export_temp_registry WHERE export_id='active-export'`).Scan(&exportStatus); err != nil || exportStatus != "invalidated" {
		t.Fatalf("active export status=%q err=%v; deletion must invalidate it transactionally", exportStatus, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT completed_sequence FROM zoe_deletion_reconciliation_checkpoints WHERE target='export_temp'`).Scan(&completed); err != nil || completed != 0 {
		t.Fatalf("export-temp checkpoint advanced before physical cleanup: sequence=%d err=%v", completed, err)
	}
}

func TestContextAssemblyCannotRestoreManifestAfterDeletionBarrier(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "context-race.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewRepositoryWithStore(store)
	scope := Scope{Kind: ScopePersonal, PrincipalID: "context-race-user"}
	claim := writeMemory(t, ctx, store.DB(), repo, scope, "context race payload", "context-race-source", 0)
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: scope.PrincipalID, Scopes: []string{"memory:read:personal"}}
	deleteErr := error(nil)
	manifest, returned, err := repo.assembleContext(ctx, principal, "", scope.PrincipalID, "context race", 100, func() {
		_, deleteErr = runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: claim.ID, Expected: claim.Revision, ReasonCode: "user_requested"})
	})
	if deleteErr != nil {
		t.Fatalf("barrier deletion failed: %v", deleteErr)
	}
	if !errors.Is(err, ErrContextStale) || manifest.ID != "" || returned != nil {
		t.Fatalf("context assembly after barrier: manifest=%#v returned=%#v err=%v", manifest, returned, err)
	}
	var manifests, checkpoint int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_context_manifests WHERE workspace_id='default'`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT completed_sequence FROM zoe_deletion_reconciliation_checkpoints WHERE target='cache'`).Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	if manifests != 0 || checkpoint != 1 {
		t.Fatalf("stale context resurrected after cache checkpoint: manifests=%d checkpoint=%d", manifests, checkpoint)
	}
}

func TestSaveContextRejectsDeletionJournalIntentAheadOfSQLite(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "context-journal-race.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewRepositoryWithStore(store)
	scope := Scope{Kind: ScopePersonal, PrincipalID: "journal-race-user"}
	claim := writeMemory(t, ctx, store.DB(), repo, scope, "journal race payload", "journal-race-source", 0)
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: scope.PrincipalID, Scopes: []string{"memory:read:personal"}}
	result, err := repo.Search(ctx, principal, "journal race", SearchFilter{})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("pre-delete search returned %#v err=%v", result.Items, err)
	}
	entry := ContextEntry{ClaimID: claim.ID, Revision: claim.Revision, ContentHash: hashString(result.Items[0].Content)}
	manifest := ContextManifest{ID: "journal-race-manifest", WorkspaceID: "default", RecipientID: scope.PrincipalID, Budget: 100, CreatedAt: time.Now().UTC(), Entries: []ContextEntry{entry}}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.DeleteTx(ctx, tx, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: claim.ID, Expected: claim.Revision, ReasonCode: "user_requested"}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = repo.SaveContext(ctx, tx, manifest); !errors.Is(err, ErrContextStale) {
		t.Fatalf("SaveContext accepted sidecar-only deletion intent: %v", err)
	}
}

func TestClaimDeletionLedgerIsAtomicAndMonotonic(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	scope := Scope{Kind: ScopePersonal, PrincipalID: "ledger-user"}
	secondScope := Scope{Kind: ScopePersonal, PrincipalID: "ledger-user-2"}
	first := writeMemory(t, ctx, store.DB(), repo, scope, "first claim", "ledger-source-1", 0)
	second := writeMemory(t, ctx, store.DB(), repo, secondScope, "second claim", "ledger-source-2", 0)

	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = repo.DeleteTx(ctx, tx, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: first.ID, Expected: first.Revision, ReasonCode: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_deletion_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back delete left %d ledger events", count)
	}

	if _, err = runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: first.ID, Expected: first.Revision, ReasonCode: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	if _, err = runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: scope.PrincipalID, ClaimID: first.ID, Expected: first.Revision, ReasonCode: "user_requested"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated delete error=%v, want ErrNotFound", err)
	}
	if _, err = runDelete(ctx, store.DB(), repo, DeleteInput{WorkspaceID: "default", ActorID: secondScope.PrincipalID, ClaimID: second.ID, Expected: second.Revision, ReasonCode: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.DB().QueryContext(ctx, `SELECT sequence,event_type,workspace_id,claim_id,revision,deleted_at FROM memory_deletion_events ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for want := int64(1); rows.Next(); want++ {
		var sequence, revision int64
		var eventType, workspace, claim, deletedAt string
		if err = rows.Scan(&sequence, &eventType, &workspace, &claim, &revision, &deletedAt); err != nil {
			t.Fatal(err)
		}
		if sequence != want || eventType != "claim_deleted" || workspace != "default" || revision != 2 || deletedAt == "" {
			t.Fatalf("ledger event %d has unexpected fields: sequence=%d type=%q workspace=%q revision=%d deleted_at=%q", want, sequence, eventType, workspace, revision, deletedAt)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	var ledgerID, baseline string
	var generation int
	if err = store.DB().QueryRowContext(ctx, `SELECT ledger_id,ledger_generation,baseline_kind FROM memory_deletion_ledger_state WHERE singleton=1`).Scan(&ledgerID, &generation, &baseline); err != nil {
		t.Fatal(err)
	}
	if ledgerID == "" || generation != 1 || baseline != "legacy_snapshot_only" {
		t.Fatalf("unexpected ledger identity/baseline: id=%q generation=%d baseline=%q", ledgerID, generation, baseline)
	}
}

func TestInferredClaimCannotBePromotedAndChineseTokenizerKeepsShortTerms(t *testing.T) {
	c := Claim{ID: "candidate", WorkspaceID: "default", Scope: Scope{Kind: ScopeWorkspace}, Subject: "项目", Key: "数据库", Content: "SQLite", Authority: AuthorityModelInferred, Status: ClaimActive, Revision: 1,
		Sources: []Source{{ID: "source", Version: 1, Scope: Scope{Kind: ScopeWorkspace}, Kind: "event", Reference: "event-1", Producer: "extractor"}}}
	if err := c.ValidateForWrite(); err == nil {
		t.Fatal("model inference was allowed to enter the active authority")
	}
	terms := tokenize("项目 A1 数据库")
	want := map[string]bool{"项": false, "项目": false, "目": false, "数据": false, "据库": false, "a1": false}
	for _, term := range terms {
		if _, ok := want[term]; ok {
			want[term] = true
		}
	}
	for term, seen := range want {
		if !seen {
			t.Errorf("tokenizer omitted %q from %#v", term, terms)
		}
	}
}

func TestContextManifestCapturesVerifiedRecipientVisibleVersions(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	owner := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", Scopes: []string{"memory:read:personal"}}
	claim := writeMemory(t, ctx, store.DB(), repo, Scope{Kind: ScopePersonal, PrincipalID: owner.SubjectID}, "保留 SQLite 作为本地事务数据库", "context-source", 0)

	manifest, claims, err := repo.AssembleContext(ctx, owner, "", owner.SubjectID, "SQLite", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != claim.ID || len(manifest.Entries) != 1 || manifest.Entries[0].Revision != claim.Revision {
		t.Fatalf("manifest did not capture the visible revision: manifest=%#v claims=%#v", manifest, claims)
	}
	var saved []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT manifest FROM memory_context_manifests WHERE id=?`, manifest.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) == 0 {
		t.Fatal("context manifest was not durably recorded")
	}
	other := owner
	other.SubjectID = "user-2"
	manifest, claims, err = repo.AssembleContext(ctx, other, "", other.SubjectID, "SQLite", 100)
	if err != nil || len(claims) != 0 || len(manifest.Entries) != 0 {
		t.Fatalf("context leaked across personal principals: manifest=%#v claims=%#v err=%v", manifest, claims, err)
	}
}

func TestNormalizeTextPreservesOrderAndRepetition(t *testing.T) {
	if normalizeText("Alice trusts Bob") == normalizeText("Bob trusts Alice") {
		t.Fatal("reordered claim was treated as equal")
	}
	if normalizeText("Alice trusts Bob") == normalizeText("Alice trusts Bob Bob") {
		t.Fatal("repeated token was treated as equal")
	}
	if normalizeText("Alice trusts Bob") != normalizeText("ALICE trusts, Bob!") {
		t.Fatal("punctuation/case normalization changed")
	}
}

func TestTaskContextExcludesMemoriesFromOtherProjects(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	_, err := store.DB().ExecContext(ctx, `INSERT INTO projects(id,workspace_id,name,status,version,created_at,updated_at,owner_subject_id) VALUES('project-a','default','A','active',1,'now','now','owner'),('project-b','default','B','active',1,'now','now','other')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-a','default','owner','test','approved','now')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES('task-a','default','project-a','Task','Goal','user','default','queued',1,'now','now','device-a','cmd','owner')`)
	if err != nil {
		t.Fatal(err)
	}
	writeMemory(t, ctx, store.DB(), repo, Scope{Kind: ScopeProject, ID: "project-a"}, "sharedword belongs to project A", "project-source-a", 0)
	writeMemory(t, ctx, store.DB(), repo, Scope{Kind: ScopeProject, ID: "project-b"}, "sharedword belongs to project B", "project-source-b", 0)
	owner := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:read:workspace"}}
	projectSearch, err := repo.Search(ctx, owner, "sharedword", SearchFilter{ScopeKind: ScopeProject, ScopeID: "project-b"})
	if err != nil || len(projectSearch.Items) != 0 {
		t.Fatalf("same-workspace project crossed ownership boundary: result=%#v err=%v", projectSearch, err)
	}
	_, claims, err := repo.AssembleContext(ctx, owner, "task-a", owner.SubjectID, "sharedword", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Scope.ID != "project-a" {
		t.Fatalf("task context crossed project boundary: %#v", claims)
	}
	other := contracts.Principal{WorkspaceID: "default", SubjectID: "other", Scopes: []string{"memory:read:workspace"}}
	_, _, err = repo.AssembleContext(ctx, other, "task-a", other.SubjectID, "sharedword", 1000)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("different subject accessed another subject's task: err=%v", err)
	}
}

func TestMemoryActionGrantsAreExplicitAndScopeBound(t *testing.T) {
	personal := Scope{Kind: ScopePersonal, PrincipalID: "owner"}
	projectA := Scope{Kind: ScopeProject, ID: "project-a"}
	projectB := Scope{Kind: ScopeProject, ID: "project-b"}
	legacy := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"client:command"}}
	if mayWriteScope(legacy, personal) || mayWriteScope(legacy, projectA) || mayDeleteScope(legacy, personal) {
		t.Fatal("generic command scope granted memory mutation")
	}
	narrow := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:write:project:project-a", "memory:read:project:project-a"}}
	if !mayWriteScope(narrow, projectA) || mayWriteScope(narrow, projectB) || !mayReadScope(narrow, projectA) || mayReadScope(narrow, projectB) {
		t.Fatal("project grant escaped its explicit project ID")
	}
	owner := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:read:personal", "memory:write:personal", "memory:delete:personal", "memory:read:workspace", "memory:write:workspace", "memory:delete:workspace"}}
	if !mayWriteScope(owner, personal) || !mayWriteScope(owner, projectA) || !mayDeleteScope(owner, projectA) || !mayReadScope(owner, projectB) {
		t.Fatal("configured owner scopes did not include personal and workspace contexts")
	}
}

func TestProjectViewerCannotWriteMemoryDespiteExplicitMemoryGrant(t *testing.T) {
	ctx := context.Background()
	store, _ := newMemoryTestRepo(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO projects(id,workspace_id,name,status,version,created_at,updated_at,owner_subject_id) VALUES('project-roles','default','Roles','active',1,?,?,?)`, now, now, "owner"); err != nil {
		t.Fatal(err)
	}

	scope := Scope{Kind: ScopeProject, ID: "project-roles"}
	viewer := contracts.Principal{WorkspaceID: "default", SubjectID: "viewer", Scopes: []string{"memory:write:project:project-roles", "memory:delete:project:project-roles"}}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := mayWriteAccessScope(ctx, tx, viewer, scope)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if allowed {
		t.Fatal("project viewer with explicit memory write grant passed project write-role check")
	}
	member := contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:write:project:project-roles"}}
	tx, err = store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err = mayWriteAccessScope(ctx, tx, member, scope)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if !allowed {
		t.Fatal("project member with explicit memory write grant was denied")
	}
}

func TestProposalApprovalRejectsDeletedEventSource(t *testing.T) {
	ctx := context.Background()
	store, repo := newMemoryTestRepo(t)
	payload := []byte(`{"result":"verified"}`)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('source-event','default',1,1,'task.completed','now','owner','task','task-a',1,?)`, payload); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: ScopeWorkspace}
	source := Source{ID: "proposal-source", Version: 1, Scope: scope, Kind: "event", Reference: "event:source-event", ContentHash: hashBytes(payload), RecordedAt: time.Now().UTC(), Producer: "runner"}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSourceTx(ctx, tx, "default", source); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	proposal, err := repo.CreateProposal(ctx, contracts.RunnerPrincipal{RunnerID: "runner", WorkspaceIDs: []string{"default"}, Scopes: []string{"memory:propose:workspace"}}, runnerProposalRequest{WorkspaceID: "default", SourceID: source.ID, SourceVersion: 1, ExtractorVersion: "extractor-v1", Scope: scope, Subject: "project", Key: "setting", Content: "verified setting"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM events WHERE event_id='source-event' AND workspace_id='default'`); err != nil {
		t.Fatal(err)
	}
	approveTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.ApproveProposalTx(ctx, approveTx, contracts.Principal{WorkspaceID: "default", SubjectID: "owner", Scopes: []string{"memory:read:workspace", "memory:write:workspace"}}, "approve-command", proposal.ID, proposal.Version, time.Now().UTC())
	_ = approveTx.Rollback()
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("approval accepted a deleted event source: err=%v", err)
	}
}

func writeMemory(t *testing.T, ctx context.Context, db *sql.DB, repo *Repository, scope Scope, content, sourceID string, expected int64) Claim {
	t.Helper()
	claim, err := writeMemoryErr(ctx, db, repo, scope, content, sourceID, expected)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func writeMemoryErr(ctx context.Context, db *sql.DB, repo *Repository, scope Scope, content, sourceID string, expected int64) (Claim, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	now := time.Now().UTC()
	actor := scope.PrincipalID
	if actor == "" {
		actor = "user-1"
	}
	source := Source{ID: sourceID, Version: 1, Scope: scope, Kind: "user_note", Reference: "command:" + sourceID, RecordedAt: now, Producer: "user"}
	claim, err := repo.PutTx(ctx, tx, WriteInput{WorkspaceID: "default", ActorID: actor, Expected: expected, Source: source, Claim: Claim{WorkspaceID: "default", Scope: scope, Subject: "project", Key: "database", Content: content, Authority: AuthorityUserConfirmed, Status: ClaimActive, Revision: 1, Sources: []Source{source}}})
	if err != nil {
		_ = tx.Rollback()
		return Claim{}, err
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, err
	}
	return claim, nil
}

func runDelete(ctx context.Context, db *sql.DB, repo *Repository, in DeleteInput) (Claim, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	claim, err := repo.DeleteTx(ctx, tx, in)
	if err != nil {
		_ = tx.Rollback()
		return Claim{}, err
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, err
	}
	return claim, nil
}
