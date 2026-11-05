package control

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

// Export data is written to a private temporary file in bounded chunks. The
// 64 MiB compatibility boundary is no longer an in-memory cap; packages above
// it still stream to disk and are stopped at the hard 1 GiB archive bound.
const maxScopedExportBytes int64 = 1 << 30
const maxScopedArchiveBytes int64 = 1 << 30

// Individual text fields are checked with SQLite length() before JSON values
// are materialized. This caps the largest Go/SQLite row value while allowing
// multi-row exports to exceed the old 64 MiB in-memory threshold.
const maxScopedExportFieldBytes int64 = 1 << 20

// RegisterExportRoutes adds the permission-scoped authoritative-data export.
func RegisterExportRoutes(s *Server) error {
	if s == nil {
		return errors.New("control server is required")
	}
	if err := s.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/export", s.handleScopedExport)
	}); err != nil {
		return err
	}
	return s.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("POST /runner/v1/memory/export-temp/cleanup", s.handleExportTempCleanup)
	})
}

type exportCounter struct {
	Projects         int64 `json:"projects"`
	Tasks            int64 `json:"tasks"`
	Dependencies     int64 `json:"task_dependencies"`
	TaskEntries      int64 `json:"task_entries"`
	Approvals        int64 `json:"approvals"`
	Runs             int64 `json:"runs"`
	Evidence         int64 `json:"evidence"`
	Memories         int64 `json:"memories"`
	Revisions        int64 `json:"memory_revisions"`
	Sources          int64 `json:"memory_sources"`
	VoiceLinks       int64 `json:"voice_associations"`
	VoiceResults     int64 `json:"voice_result_deliveries"`
	VoiceTranscripts int64 `json:"voice_transcript_turns"`
	VoiceSummaries   int64 `json:"voice_call_summaries"`
	Capabilities     int64 `json:"capability_authorizations"`
}

type exportFile struct {
	file  *os.File
	count int64
	hash  io.Writer
}

func (f *exportFile) Write(p []byte) (int, error) {
	if int64(len(p)) > maxScopedExportBytes-f.count {
		return 0, errExportTooLarge
	}
	n, err := f.file.Write(p)
	if n > 0 {
		f.count += int64(n)
		_, _ = f.hash.Write(p[:n])
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (f *exportFile) WriteString(s string) (int, error) {
	if int64(len(s)) > maxScopedExportBytes-f.count {
		return 0, errExportTooLarge
	}
	n, err := f.file.WriteString(s)
	if n > 0 {
		f.count += int64(n)
		hashErr := hashExportString(f.hash, s[:n])
		if err == nil {
			err = hashErr
		}
	}
	if err == nil && n != len(s) {
		err = io.ErrShortWrite
	}
	return n, err
}

func hashExportString(w io.Writer, value string) error {
	const chunk = 32 << 10
	for len(value) > 0 {
		n := len(value)
		if n > chunk {
			n = chunk
		}
		if _, err := w.Write([]byte(value[:n])); err != nil {
			return err
		}
		value = value[n:]
	}
	return nil
}

var errExportTooLarge = errors.New("export exceeds configured size limit")

func (s *Server) handleScopedExport(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, apiError(http.StatusMethodNotAllowed, "method_not_allowed", "GET is required", nil))
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope != "personal" && scope != "workspace" {
		writeError(w, apiError(http.StatusBadRequest, "invalid_scope", "scope must be personal or workspace", nil))
		return
	}
	if !HasScope(p, "export:read") || !HasScope(p, "client:read") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "export:read and client:read scopes are required", nil))
		return
	}
	if scope == "personal" && !HasScope(p, "memory:read:personal") || scope == "workspace" && !hasAnyExportMemoryGrant(p) {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "the requested memory read scope is required", nil))
		return
	}

	exportID, err := randomID()
	if err != nil {
		writeExportFailure(w)
		return
	}
	lease, err := s.beginExportTemp(r.Context(), p, scope, exportID)
	if err != nil {
		writeExportFailure(w)
		return
	}
	defer lease.closeAndClean(s.store.DB())
	f := lease.file
	h := sha256.New()
	output := &exportFile{file: f, hash: h}

	tx, err := s.store.DB().BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeExportFailure(w)
		return
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRowContext(r.Context(), `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, p.WorkspaceID).Scan(&cursor); err != nil {
		writeExportFailure(w)
		return
	}
	instanceLineage, instanceGeneration, deletionSequence, err := exportInstanceAuthority(r.Context(), tx)
	if err != nil {
		writeExportFailure(w)
		return
	}
	if err = verifyDeletionJournalHead(s.store.Path(), instanceLineage, instanceGeneration, deletionSequence); err != nil {
		writeError(w, apiError(http.StatusServiceUnavailable, "deletion_journal_unavailable", "deletion journal is not continuous with authoritative state", nil))
		return
	}
	if err = validateExportFieldBounds(r.Context(), tx, p, scope); err != nil {
		if errors.Is(err, errExportFieldTooLarge) {
			writeError(w, apiError(http.StatusRequestEntityTooLarge, "export_field_too_large", "an authorized export field exceeds the 1 MiB per-field limit", nil))
			return
		}
		writeExportFailure(w)
		return
	}
	// Run scalar-only field length checks before permission fingerprinting,
	// which inspects revision contents and would otherwise materialize them.
	permissionFingerprint, err := exportAuthorizationFingerprint(r.Context(), tx, p, scope)
	if err != nil {
		writeExportFailure(w)
		return
	}
	counters := exportCounter{}
	if err = writeExportDocument(r.Context(), tx, output, p, scope, exportID, cursor, &counters); err != nil {
		if errors.Is(err, errExportTooLarge) {
			writeError(w, apiError(http.StatusRequestEntityTooLarge, "export_too_large", "authorized export exceeds the bounded 1 GiB limit", nil))
			return
		}
		writeError(w, apiError(http.StatusInternalServerError, "export_failed", err.Error(), nil))
		return
	}
	if err = tx.Commit(); err != nil {
		writeExportFailure(w)
		return
	}
	if err = f.Sync(); err != nil {
		writeExportFailure(w)
		return
	}
	manifest := buildExportManifest(exportID, p, scope, cursor, instanceLineage, instanceGeneration, deletionSequence, counters, output.count, hex.EncodeToString(h.Sum(nil)))
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		writeExportFailure(w)
		return
	}
	archiveFile, err := lease.createArchive()
	if err != nil {
		writeExportFailure(w)
		return
	}
	defer archiveFile.Close()
	archiveHash := sha256.New()
	archiveOut := &boundedHashWriter{writer: io.MultiWriter(archiveFile, archiveHash), limit: maxScopedArchiveBytes}
	zipWriter := zip.NewWriter(archiveOut)
	manifestEntry, err := zipWriter.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Store})
	if err == nil {
		_, err = manifestEntry.Write(manifestBytes)
	}
	if err == nil {
		var dataEntry io.Writer
		dataEntry, err = zipWriter.CreateHeader(&zip.FileHeader{Name: "data.json", Method: zip.Store})
		if err == nil {
			if _, err = f.Seek(0, io.SeekStart); err == nil {
				_, err = io.Copy(dataEntry, f)
			}
		}
	}
	if closeErr := zipWriter.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if errors.Is(err, errExportTooLarge) {
			writeError(w, apiError(http.StatusRequestEntityTooLarge, "export_too_large", "export archive exceeds the configured bounded streaming limit", nil))
			return
		}
		writeExportFailure(w)
		return
	}
	if err = archiveFile.Sync(); err != nil {
		writeExportFailure(w)
		return
	}

	// Reject a stale artifact if any authoritative event or device revocation
	// occurred after the export snapshot was read.
	auditTx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeExportFailure(w)
		return
	}
	defer auditTx.Rollback()
	var currentCursor int64
	if err = auditTx.QueryRowContext(r.Context(), `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, p.WorkspaceID).Scan(&currentCursor); err != nil || currentCursor != cursor {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "authoritative data changed while preparing the export; retry the request", nil))
		return
	}
	if err = requireOwnerSessionTx(r.Context(), auditTx, p); err != nil {
		writeError(w, err)
		return
	}

	currentPermissionFingerprint, err := exportAuthorizationFingerprint(r.Context(), auditTx, p, scope)
	if err != nil {
		writeExportFailure(w)
		return
	}
	if !equalBytes(permissionFingerprint, currentPermissionFingerprint) {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "authoritative permissions changed while preparing the export; retry the request", nil))
		return
	}
	currentLineage, currentGeneration, currentDeletionSequence, stateErr := exportInstanceAuthority(r.Context(), auditTx)
	if stateErr != nil || currentLineage != instanceLineage || currentGeneration != instanceGeneration || currentDeletionSequence != deletionSequence {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "deletion or restore state changed while preparing the export; retry the request", nil))
		return
	}
	if verifyDeletionJournalHead(s.store.Path(), currentLineage, currentGeneration, currentDeletionSequence) != nil {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "deletion journal changed while preparing the export; retry the request", nil))
		return
	}
	if _, err = archiveFile.Seek(0, io.SeekStart); err != nil {
		writeExportFailure(w)
		return
	}
	manifestHash := sha256.Sum256(manifestBytes)
	auditPayload, marshalErr := json.Marshal(map[string]any{"schema_version": 2, "scope": scope, "archive_bytes": archiveOut.count, "archive_sha256": hex.EncodeToString(archiveHash.Sum(nil)), "manifest_sha256": hex.EncodeToString(manifestHash[:]), "counts": counters})
	if marshalErr != nil {
		writeExportFailure(w)
		return
	}
	if err = s.AppendEventsTx(r.Context(), auditTx, p, []contracts.Event{{
		EventID: exportID, Type: "data.export.prepared", AggregateType: "data_export", AggregateID: exportID, AggregateVersion: 1,
		Payload: auditPayload,
	}}); err != nil {
		writeExportFailure(w)
		return
	}
	var validatedCursor int64
	var eventID, eventType, aggregateID string
	if err = auditTx.QueryRowContext(r.Context(), `SELECT current_cursor FROM event_counters WHERE workspace_id=?`, p.WorkspaceID).Scan(&validatedCursor); err != nil || validatedCursor != cursor+1 {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "export audit cursor did not follow the data snapshot", nil))
		return
	}
	if err = auditTx.QueryRowContext(r.Context(), `SELECT event_id,event_type,aggregate_id FROM events WHERE workspace_id=? AND cursor=?`, p.WorkspaceID, validatedCursor).Scan(&eventID, &eventType, &aggregateID); err != nil || eventID != exportID || eventType != "data.export.prepared" || aggregateID != exportID {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "export audit event does not match the package authority cursor", nil))
		return
	}
	if err = auditTx.Commit(); err != nil {
		writeExportFailure(w)
		return
	}
	if invalidated, checkErr := lease.invalidated(r.Context(), s.store.DB()); checkErr != nil || invalidated {
		writeError(w, apiError(http.StatusConflict, "export_snapshot_stale", "export was invalidated during preparation; retry the request", nil))
		return
	}
	w.Header().Set("Content-Type", "application/vnd.zoe.export+zip")
	w.Header().Set("Content-Disposition", `attachment; filename="zoe-export-`+exportID+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Zoe-Archive-SHA256", hex.EncodeToString(archiveHash.Sum(nil)))
	w.Header().Set("X-Zoe-Manifest-SHA256", hex.EncodeToString(manifestHash[:]))
	w.Header().Set("Content-Length", strconv.FormatInt(archiveOut.count, 10))
	w.WriteHeader(http.StatusOK)
	copyRegisteredExport(w, r, lease, s.store.DB(), archiveFile)
}

func copyRegisteredExport(w http.ResponseWriter, r *http.Request, lease *exportTempLease, db *sql.DB, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		invalidated, err := lease.invalidated(r.Context(), db)
		if err != nil || invalidated {
			return
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil || written != n {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

type boundedHashWriter struct {
	writer io.Writer
	limit  int64
	count  int64
}

func (w *boundedHashWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.count {
		return 0, errExportTooLarge
	}
	n, err := w.writer.Write(p)
	w.count += int64(n)
	return n, err
}

func exportInstanceAuthority(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, int64, int64, error) {
	var lineage string
	var generation, deletionSequence int64
	err := q.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage, &generation, &deletionSequence)
	return lineage, generation, deletionSequence, err
}

func verifyDeletionJournalHead(databasePath, lineage string, generation, sequence int64) error {
	if databasePath == ":memory:" {
		return nil
	}
	journal, err := storage.NewDeletionJournal(databasePath)
	if err != nil {
		return err
	}
	records, err := journal.ReadAll()
	if err != nil {
		return err
	}
	if int64(len(records)) != sequence {
		return errors.New("deletion journal head does not match authority snapshot")
	}
	if len(records) > 0 {
		last := records[len(records)-1]
		if last.LineageID != lineage || last.Generation > generation {
			return errors.New("deletion journal identity does not match authority snapshot")
		}
	}
	return nil
}

func buildExportManifest(id string, p contracts.Principal, scope string, cursor int64, lineage string, generation, deletionSequence int64, counts exportCounter, dataBytes int64, dataHash string) map[string]any {
	return map[string]any{
		"format": "zoe.authority-export", "schema_version": 2, "export_id": id,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano), "workspace_id": p.WorkspaceID, "subject_id": p.SubjectID, "scope": scope,
		"authority": map[string]any{
			"instance_lineage_id": lineage, "generation": generation, "deletion_sequence": strconv.FormatInt(deletionSequence, 10),
			"snapshot_event_cursor": strconv.FormatInt(cursor, 10), "validated_through_cursor": strconv.FormatInt(cursor+1, 10),
			"export_audit_event_cursor": strconv.FormatInt(cursor+1, 10), "export_audit_event_id": id,
			"export_audit_event_type": "data.export.prepared", "source": "sqlite_authority",
		},
		"content":     map[string]any{"inventory": []string{"projects", "tasks", "task_dependencies", "task_entries", "approvals", "runs", "evidence", "memory_claims", "memory_revisions", "memory_sources", "voice_associations", "voice_result_deliveries", "voice_transcript_turns", "voice_call_summaries", "capability_authorizations"}, "counts": counts},
		"permissions": map[string]any{"scope": scope, "row_visibility": "principal_authorized", "secret_values": "excluded"},
		"artifacts":   map[string]any{"bytes": "referenced_not_included", "voice_audio_bytes": "omitted"},
		"derived":     map[string]any{"mem0": "not_included_rebuildable", "qdrant": "not_included_rebuildable", "cache": "not_included"},
		"omissions":   []string{"backup_credentials_external_volume", "service_configuration_secret_values", "large_artifact_bytes", "voice_audio_bytes", "legacy_voice_turn_delegation_records"},
		"integrity":   map[string]any{"data_bytes": dataBytes, "data_sha256": dataHash},
		"streaming":   map[string]any{"memory_threshold_bytes": int64(64 << 20), "archive_hard_limit_bytes": maxScopedArchiveBytes, "max_field_bytes": maxScopedExportFieldBytes},
	}
}

var errExportFieldTooLarge = errors.New("export field exceeds configured size limit")

// Validate the byte lengths of user-controlled text while SQLite only returns
// scalar lengths. This runs before json_object or Go JSON encoding can copy a
// large field into an intermediate value.
func validateExportFieldBounds(ctx context.Context, tx *sql.Tx, p contracts.Principal, scope string) error {
	if err := validateVoiceTranscriptExportBounds(ctx, tx, p); err != nil {
		return err
	}
	visible := exportVisibilityPredicate()
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT coalesce(max(max(length(CAST(p.name AS BLOB)),length(CAST(p.description AS BLOB)))),0) FROM projects p WHERE p.workspace_id=?1 AND p.status='active' AND ` + visible.project, []any{p.WorkspaceID, p.SubjectID}},
		{`SELECT coalesce(max(max(length(CAST(t.title AS BLOB)),length(CAST(t.goal AS BLOB)),length(CAST(t.source AS BLOB)),length(CAST(t.result AS BLOB)),length(CAST(t.error AS BLOB)))),0) FROM tasks t WHERE t.workspace_id=?1 AND ` + visible.task, []any{p.WorkspaceID, p.SubjectID}},
		{`SELECT coalesce(max(length(CAST(e.content AS BLOB))),0) FROM task_entries e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.workspace_id=?1 AND ` + visible.taskOnT, []any{p.WorkspaceID, p.SubjectID}},
		{`SELECT coalesce(max(length(CAST(a.prompt AS BLOB))),0) FROM approvals a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE a.workspace_id=?1 AND ` + visible.taskOnT, []any{p.WorkspaceID, p.SubjectID}},
		{`SELECT coalesce(max(max(length(CAST(e.summary AS BLOB)),length(CAST(e.source AS BLOB)))),0) FROM evidence e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.workspace_id=?1 AND ` + visible.taskOnT, []any{p.WorkspaceID, p.SubjectID}},
	}
	for _, query := range queries {
		var size int64
		if err := tx.QueryRowContext(ctx, query.sql, query.args...).Scan(&size); err != nil {
			return err
		}
		if size > maxScopedExportFieldBytes {
			return errExportFieldTooLarge
		}
	}
	claims, err := tx.QueryContext(ctx, `SELECT id,scope_kind,scope_id,principal_id,length(CAST(subject AS BLOB)),length(CAST(claim_key AS BLOB)) FROM memory_claims WHERE workspace_id=? AND deleted_at IS NULL AND status!='tombstoned'`, p.WorkspaceID)
	if err != nil {
		return err
	}
	for claims.Next() {
		var id, kind, scopeID, owner string
		var subjectBytes, keyBytes int64
		if err = claims.Scan(&id, &kind, &scopeID, &owner, &subjectBytes, &keyBytes); err != nil {
			claims.Close()
			return err
		}
		allowed, accessErr := exportMemoryScopeAllowed(ctx, tx, p, kind, scopeID, owner)
		if accessErr != nil {
			claims.Close()
			return accessErr
		}
		if !allowed || (scope == "personal" && kind != "personal") || (scope == "workspace" && kind == "personal") {
			continue
		}
		if subjectBytes > maxScopedExportFieldBytes || keyBytes > maxScopedExportFieldBytes {
			claims.Close()
			return errExportFieldTooLarge
		}
		var revisionBytes int64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(length(CAST(content AS BLOB))),0) FROM memory_revisions WHERE claim_id=?`, id).Scan(&revisionBytes); err != nil {
			claims.Close()
			return err
		}
		if revisionBytes > maxScopedExportFieldBytes {
			claims.Close()
			return errExportFieldTooLarge
		}
		sources, sourceErr := tx.QueryContext(ctx, `SELECT s.scope_kind,s.scope_id,s.principal_id,length(CAST(s.reference AS BLOB)),length(CAST(s.content_hash AS BLOB)),length(CAST(s.producer AS BLOB)) FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version WHERE rs.claim_id=? AND rs.workspace_id=? AND s.deleted_at IS NULL`, id, p.WorkspaceID)
		if sourceErr != nil {
			claims.Close()
			return sourceErr
		}
		for sources.Next() {
			var sourceKind, sourceScope, sourceOwner string
			var referenceBytes, hashBytes, producerBytes int64
			if sourceErr = sources.Scan(&sourceKind, &sourceScope, &sourceOwner, &referenceBytes, &hashBytes, &producerBytes); sourceErr != nil {
				break
			}
			sourceAllowed, accessErr := exportMemoryScopeAllowed(ctx, tx, p, sourceKind, sourceScope, sourceOwner)
			if accessErr != nil {
				sourceErr = accessErr
				break
			}
			if sourceAllowed && (referenceBytes > maxScopedExportFieldBytes || hashBytes > maxScopedExportFieldBytes || producerBytes > maxScopedExportFieldBytes) {
				sourceErr = errExportFieldTooLarge
				break
			}
		}
		if sourceErr == nil {
			sourceErr = sources.Err()
		}
		closeErr := sources.Close()
		if sourceErr != nil {
			claims.Close()
			return sourceErr
		}
		if closeErr != nil {
			claims.Close()
			return closeErr
		}
	}
	if err = claims.Err(); err != nil {
		claims.Close()
		return err
	}
	if err = claims.Close(); err != nil {
		return err
	}
	return nil
}

func writeExportFailure(w http.ResponseWriter) {
	writeError(w, apiError(http.StatusInternalServerError, "export_failed", "export could not be prepared", nil))
}

func exportAuthorizationFingerprint(ctx context.Context, q *sql.Tx, p contracts.Principal, exportScope string) ([]byte, error) {
	h := sha256.New()
	visible := exportVisibilityPredicate()
	for _, statement := range []string{
		`SELECT p.id FROM projects p WHERE p.workspace_id=?1 AND p.status='active' AND ` + visible.project + ` ORDER BY p.id`,
		`SELECT t.id FROM tasks t WHERE t.workspace_id=?1 AND ` + visible.task + ` ORDER BY t.id`,
	} {
		rows, err := q.QueryContext(ctx, statement, p.WorkspaceID, p.SubjectID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			_, _ = io.WriteString(h, id)
			_, _ = io.WriteString(h, "\x00")
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	var claims *sql.Rows
	var err error
	if exportScope == "personal" {
		claims, err = q.QueryContext(ctx, `SELECT id,scope_kind,scope_id,principal_id,revision,status FROM memory_claims WHERE workspace_id=? AND scope_kind='personal' AND principal_id=? AND deleted_at IS NULL AND status!='tombstoned' ORDER BY id`, p.WorkspaceID, p.SubjectID)
	} else {
		claims, err = q.QueryContext(ctx, `SELECT id,scope_kind,scope_id,principal_id,revision,status FROM memory_claims WHERE workspace_id=? AND scope_kind IN ('workspace','project','task') AND deleted_at IS NULL AND status!='tombstoned' ORDER BY id`, p.WorkspaceID)
	}
	if err != nil {
		return nil, err
	}
	for claims.Next() {
		var id, kind, scopeID, principalID, status string
		var revision int64
		if err = claims.Scan(&id, &kind, &scopeID, &principalID, &revision, &status); err != nil {
			claims.Close()
			return nil, err
		}
		allowed, accessErr := exportMemoryScopeAllowed(ctx, q, p, kind, scopeID, principalID)
		if accessErr != nil {
			claims.Close()
			return nil, accessErr
		}
		if !allowed {
			continue
		}
		_, _ = io.WriteString(h, id+"\x00"+strconv.FormatInt(revision, 10)+"\x00"+status+"\x00")
		revisions, queryErr := q.QueryContext(ctx, `SELECT revision,content,authority_kind,status,recorded_at,extractor_version FROM memory_revisions WHERE claim_id=? ORDER BY revision`, id)
		if queryErr != nil {
			claims.Close()
			return nil, queryErr
		}
		for revisions.Next() {
			var rev int64
			var content, authority, revStatus, recorded, extractor string
			if queryErr = revisions.Scan(&rev, &content, &authority, &revStatus, &recorded, &extractor); queryErr != nil {
				break
			}
			_, _ = io.WriteString(h, strconv.FormatInt(rev, 10)+"\x00"+content+"\x00"+authority+"\x00"+revStatus+"\x00"+recorded+"\x00"+extractor+"\x00")
			sources, sourceErr := q.QueryContext(ctx, `SELECT s.id,s.version,s.scope_kind,s.scope_id,s.principal_id,s.reference,coalesce(s.deleted_at,'') FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version WHERE rs.claim_id=? AND rs.revision=? AND rs.workspace_id=? ORDER BY s.id,s.version`, id, rev, p.WorkspaceID)
			if sourceErr != nil {
				queryErr = sourceErr
				break
			}
			for sources.Next() {
				var sourceID, sourceKind, sourceScopeID, sourcePrincipalID, reference, deleted string
				var sourceVersion int64
				if sourceErr = sources.Scan(&sourceID, &sourceVersion, &sourceKind, &sourceScopeID, &sourcePrincipalID, &reference, &deleted); sourceErr != nil {
					break
				}
				sourceAllowed, permissionErr := exportMemoryScopeAllowed(ctx, q, p, sourceKind, sourceScopeID, sourcePrincipalID)
				if permissionErr != nil {
					sourceErr = permissionErr
					break
				}
				if sourceAllowed {
					_, _ = io.WriteString(h, sourceID+"\x00"+strconv.FormatInt(sourceVersion, 10)+"\x00"+reference+"\x00"+deleted+"\x00")
				}
			}
			if sourceErr == nil {
				sourceErr = sources.Err()
			}
			closeSourceErr := sources.Close()
			if sourceErr != nil {
				queryErr = sourceErr
				break
			}
			if closeSourceErr != nil {
				queryErr = closeSourceErr
				break
			}
		}
		if queryErr == nil {
			queryErr = revisions.Err()
		}
		closeRevisionErr := revisions.Close()
		if queryErr != nil {
			claims.Close()
			return nil, queryErr
		}
		if closeRevisionErr != nil {
			claims.Close()
			return nil, closeRevisionErr
		}
	}
	err = claims.Err()
	closeErr := claims.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return h.Sum(nil), nil
}

func writeExportDocument(ctx context.Context, tx *sql.Tx, out io.Writer, p contracts.Principal, scope, exportID string, cursor int64, counts *exportCounter) error {
	if _, err := io.WriteString(out, `{"projects":`); err != nil {
		return err
	}
	visible := exportVisibilityPredicate()
	args := []any{p.WorkspaceID, p.SubjectID}
	if err := writeRows(ctx, tx, out, &counts.Projects, `SELECT json_object('id',p.id,'name',p.name,'description',p.description,'status',p.status,'version',p.version,'created_at',p.created_at,'updated_at',p.updated_at) FROM projects p WHERE p.workspace_id=?1 AND p.status='active' AND `+visible.project+` ORDER BY p.created_at,p.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"tasks":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.Tasks, `SELECT json_object('id',t.id,'project_id',CASE WHEN EXISTS(SELECT 1 FROM projects p WHERE p.workspace_id=t.workspace_id AND p.id=t.project_id AND p.status='active' AND (p.owner_subject_id=?2)) THEN t.project_id ELSE '' END,'title',t.title,'goal',t.goal,'source',t.source,'profile_id',t.profile_id,'status',t.status,'version',t.version,'created_at',t.created_at,'updated_at',t.updated_at,'accepted_at',t.accepted_at,'started_at',t.started_at,'finished_at',t.finished_at,'result',t.result,'error',t.error) FROM tasks t WHERE t.workspace_id=?1 AND `+visible.task+` ORDER BY t.created_at,t.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"task_dependencies":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.Dependencies, `SELECT json_object('task_id',d.task_id,'dependency_task_id',d.dependency_task_id,'status',d.status,'created_at',d.created_at) FROM task_dependencies d JOIN tasks t ON t.id=d.task_id AND t.workspace_id=d.workspace_id JOIN tasks dep ON dep.id=d.dependency_task_id AND dep.workspace_id=d.workspace_id WHERE d.workspace_id=?1 AND `+visible.task+` AND `+visible.taskOther+` ORDER BY d.task_id,d.dependency_task_id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"task_entries":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.TaskEntries, `SELECT json_object('id',e.id,'task_id',e.task_id,'kind',e.kind,'content',e.content,'created_at',e.created_at) FROM task_entries e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY e.created_at,e.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"approvals":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.Approvals, `SELECT json_object('id',a.id,'task_id',a.task_id,'task_version',a.task_version,'status',a.status,'prompt',a.prompt,'version',a.version,'created_at',a.created_at,'expires_at',a.expires_at) FROM approvals a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id WHERE a.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY a.created_at,a.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"runs":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.Runs, `SELECT json_object('id',r.id,'task_id',r.task_id,'attempt_id',r.attempt_id,'status',r.status,'desired_control',r.desired_control,'control_version',r.control_version,'started_at',r.started_at,'stopped_at',r.stopped_at) FROM runs r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id WHERE r.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY r.started_at,r.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"evidence":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.Evidence, `SELECT json_object('id',e.id,'task_id',e.task_id,'run_id',CASE WHEN EXISTS(SELECT 1 FROM runs er WHERE er.workspace_id=e.workspace_id AND er.id=e.run_id AND er.task_id=e.task_id) THEN coalesce(e.run_id,'') ELSE '' END,'kind',e.kind,'summary',e.summary,'source',e.source,'created_at',e.created_at,'verified',e.verified) FROM evidence e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY e.created_at,e.id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"voice_associations":`); err != nil {
		return err
	}
	if err := writeRows(ctx, tx, out, &counts.VoiceLinks, `SELECT json_object('session_id',l.session_id,'generation',l.generation,'turn_id',l.turn_id,'turn_revision',l.turn_revision,'task_id',l.task_id,'linked_at',l.linked_at) FROM voice_task_links l JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.task_id WHERE l.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY l.linked_at,l.session_id,l.turn_id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"voice_result_deliveries":`); err != nil {
		return err
	}
	var voiceResultsTable int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='voice_result_deliveries'`).Scan(&voiceResultsTable); err != nil {
		return err
	}
	if voiceResultsTable == 0 {
		if _, err := io.WriteString(out, `[]`); err != nil {
			return err
		}
	} else if err := writeRows(ctx, tx, out, &counts.VoiceResults, `SELECT json_object('entry_id',d.entry_id,'task_id',d.task_id,'run_id',d.run_id,'status',d.status,'attempts',d.attempts,'last_error_code',d.last_error_code,'voice_session_id',d.voice_session_id,'generation',d.generation,'created_at',d.created_at,'updated_at',d.updated_at) FROM voice_result_deliveries d JOIN tasks t ON t.workspace_id=d.workspace_id AND t.id=d.task_id WHERE d.workspace_id=?1 AND `+visible.taskOnT+` ORDER BY d.created_at,d.entry_id`, append([]any(nil), args...)); err != nil {
		return err
	}
	if err := writeVoiceTranscriptExport(ctx, tx, out, p, counts); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"capability_authorizations":`); err != nil {
		return err
	}
	capabilityQuery := `SELECT json_object('grant_id',g.grant_id,'deployment_id',d.deployment_id,'release_digest',g.release_digest,'scope_kind',g.scope_kind,'scope_id',g.scope_id,'principal_id',g.principal_id,'actions',CAST(g.actions AS TEXT),'budget',CAST(g.budget AS TEXT),'expires_at',g.expires_at,'authorized_by',g.authorized_by,'created_at',g.created_at,'desired_state',d.desired_state,'observed_state',d.observed_state,'version',d.version,'revoked_at',coalesce(g.revoked_at,'')) FROM capability_grants g JOIN capability_deployments d ON d.workspace_id=g.workspace_id AND d.deployment_id=g.deployment_id WHERE g.workspace_id=?1 AND ((?3='personal' AND g.principal_id=?2) OR (?3='workspace' AND (g.scope_kind='workspace' OR g.principal_id=?2 OR (g.scope_kind='project' AND EXISTS(SELECT 1 FROM projects p WHERE p.workspace_id=g.workspace_id AND p.id=g.scope_id AND ` + visible.project + `)) OR (g.scope_kind='task' AND EXISTS(SELECT 1 FROM tasks t WHERE t.workspace_id=g.workspace_id AND t.id=g.scope_id AND ` + visible.task + `))))) ORDER BY g.created_at,g.grant_id`
	if err := writeRows(ctx, tx, out, &counts.Capabilities, capabilityQuery, []any{p.WorkspaceID, p.SubjectID, scope}); err != nil {
		return err
	}
	if _, err := io.WriteString(out, `,"memory":`); err != nil {
		return err
	}
	if err := writeMemoryRows(ctx, tx, out, p, scope, counts); err != nil {
		return err
	}
	_, err := io.WriteString(out, `}`)
	return err
}

type visibility struct{ project, task, taskOther, taskOnT string }

func exportVisibilityPredicate() visibility {
	return visibility{
		project:   `(p.owner_subject_id=?2)`,
		task:      `(t.owner_subject_id=?2)`,
		taskOther: `(dep.owner_subject_id=?2)`,
		taskOnT:   `(t.owner_subject_id=?2)`}
}

func hasAnyExportMemoryGrant(p contracts.Principal) bool {
	for _, scope := range p.Scopes {
		if scope == "memory:read:workspace" || strings.HasPrefix(scope, "memory:read:project:") || strings.HasPrefix(scope, "memory:read:task:") {
			return true
		}
	}
	return false
}

func writeRows(ctx context.Context, tx *sql.Tx, out io.Writer, target *int64, query string, args []any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, `[`); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(out, `,`); err != nil {
				return err
			}
		}
		first = false
		if _, err = io.WriteString(out, raw); err != nil {
			return err
		}
		(*target)++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, `]`)
	return err
}

func writeJSONValue(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

func writeMemoryRows(ctx context.Context, tx *sql.Tx, out io.Writer, p contracts.Principal, exportScope string, counts *exportCounter) error {
	var rows *sql.Rows
	var err error
	if exportScope == "personal" {
		rows, err = tx.QueryContext(ctx, `SELECT id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,coalesce(valid_from,''),coalesce(valid_to,''),coalesce(supersedes_id,'') FROM memory_claims WHERE workspace_id=? AND scope_kind='personal' AND principal_id=? AND deleted_at IS NULL AND status!='tombstoned' ORDER BY created_at,id`, p.WorkspaceID, p.SubjectID)
	} else {
		rows, err = tx.QueryContext(ctx, `SELECT id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,coalesce(valid_from,''),coalesce(valid_to,''),coalesce(supersedes_id,'') FROM memory_claims WHERE workspace_id=? AND scope_kind IN ('workspace','project','task') AND deleted_at IS NULL AND status!='tombstoned' ORDER BY created_at,id`, p.WorkspaceID)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, `[`); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var id, kind, scopeID, principalID, subject, key, authority, status, validFrom, validTo, supersedes string
		var revision int64
		if err = rows.Scan(&id, &kind, &scopeID, &principalID, &subject, &key, &authority, &status, &revision, &validFrom, &validTo, &supersedes); err != nil {
			return err
		}
		allowed, err := exportMemoryScopeAllowed(ctx, tx, p, kind, scopeID, principalID)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		if !first {
			if _, err = io.WriteString(out, `,`); err != nil {
				return err
			}
		}
		first = false
		counts.Memories++
		if _, err = io.WriteString(out, `{"id":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, id); err != nil {
			return err
		}
		claim := map[string]any{"scope": map[string]any{"kind": kind, "id": scopeID, "principal_id": principalID}, "subject": subject, "key": key, "authority_kind": authority, "status": status, "revision": revision}
		if validFrom != "" {
			claim["valid_from"] = validFrom
		}
		if validTo != "" {
			claim["valid_to"] = validTo
		}
		if supersedes != "" {
			included, checkErr := exportMemoryClaimIncluded(ctx, tx, p, exportScope, supersedes)
			if checkErr != nil {
				return checkErr
			}
			if included {
				claim["supersedes"] = supersedes
			}
		}
		if _, err = io.WriteString(out, `,"record":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, claim); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"revisions":`); err != nil {
			return err
		}
		if err = writeMemoryRevisions(ctx, tx, out, p, id, counts); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `}`); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, `]`)
	return err
}

func exportMemoryScopeAllowed(ctx context.Context, tx *sql.Tx, p contracts.Principal, kind, id, owner string) (bool, error) {
	switch kind {
	case "personal":
		return owner == p.SubjectID && HasScope(p, "memory:read:personal"), nil
	case "workspace":
		return HasScope(p, "memory:read:workspace"), nil
	case "project":
		if !HasScope(p, "memory:read:workspace") && !HasScope(p, "memory:read:project:"+id) {
			return false, nil
		}
		return PrincipalCanAccessProject(ctx, tx, p, id)
	case "task":
		if !HasScope(p, "memory:read:workspace") && !HasScope(p, "memory:read:task:"+id) {
			return false, nil
		}
		return PrincipalCanAccessTask(ctx, tx, p, id)
	default:
		return false, nil
	}
}

func writeMemoryRevisions(ctx context.Context, tx *sql.Tx, out io.Writer, p contracts.Principal, claimID string, counts *exportCounter) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,content,authority_kind,status,recorded_at,extractor_version FROM memory_revisions WHERE claim_id=? ORDER BY revision`, claimID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, `[`); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var revision int64
		var content, authority, status, recorded, extractor string
		if err = rows.Scan(&revision, &content, &authority, &status, &recorded, &extractor); err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(out, `,`); err != nil {
				return err
			}
		}
		first = false
		counts.Revisions++
		if _, err = io.WriteString(out, `{"revision":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, revision); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"content":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, content); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"authority_kind":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, authority); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"status":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, status); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"recorded_at":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, recorded); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"extractor_version":`); err != nil {
			return err
		}
		if err = writeJSONValue(out, extractor); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `,"sources":`); err != nil {
			return err
		}
		if err = writeMemorySources(ctx, tx, out, p, claimID, revision, counts); err != nil {
			return err
		}
		if _, err = io.WriteString(out, `}`); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, `]`)
	return err
}

func writeMemorySources(ctx context.Context, tx *sql.Tx, out io.Writer, p contracts.Principal, claimID string, revision int64, counts *exportCounter) error {
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.version,s.scope_kind,s.scope_id,s.principal_id,s.kind,s.reference,s.content_hash,s.recorded_at,s.producer
FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version
WHERE rs.claim_id=? AND rs.revision=? AND rs.workspace_id=? AND s.deleted_at IS NULL ORDER BY s.id,s.version`, claimID, revision, p.WorkspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, `[`); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var id, kind, scopeID, principalID, sourceKind, reference, contentHash, recorded, producer string
		var version int64
		if err = rows.Scan(&id, &version, &kind, &scopeID, &principalID, &sourceKind, &reference, &contentHash, &recorded, &producer); err != nil {
			return err
		}
		allowed, err := exportMemoryScopeAllowed(ctx, tx, p, kind, scopeID, principalID)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		if !first {
			if _, err = io.WriteString(out, `,`); err != nil {
				return err
			}
		}
		first = false
		counts.Sources++
		safeReference, err := safeExportSourceReference(ctx, tx, p, reference)
		if err != nil {
			return err
		}
		if err = writeJSONValue(out, map[string]any{"id": id, "version": version, "scope": map[string]any{"kind": kind, "id": scopeID, "principal_id": principalID}, "kind": sourceKind, "reference": safeReference, "content_hash": contentHash, "recorded_at": recorded, "producer": producer}); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, `]`)
	return err
}

func exportMemoryClaimIncluded(ctx context.Context, tx *sql.Tx, p contracts.Principal, exportScope, claimID string) (bool, error) {
	var kind, scopeID, principalID string
	err := tx.QueryRowContext(ctx, `SELECT scope_kind,scope_id,principal_id FROM memory_claims WHERE workspace_id=? AND id=? AND deleted_at IS NULL AND status!='tombstoned'`, p.WorkspaceID, claimID).Scan(&kind, &scopeID, &principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if exportScope == "personal" && kind != "personal" || exportScope == "workspace" && kind == "personal" {
		return false, nil
	}
	return exportMemoryScopeAllowed(ctx, tx, p, kind, scopeID, principalID)
}

func safeExportSourceReference(ctx context.Context, tx *sql.Tx, p contracts.Principal, reference string) (string, error) {
	prefix, id, ok := strings.Cut(reference, ":")
	if !ok || id == "" {
		return "", nil
	}
	visible := exportVisibilityPredicate()
	var count int
	switch prefix {
	case "task_entry":
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM task_entries e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.id=?3 AND e.workspace_id=?1 AND `+visible.taskOnT, p.WorkspaceID, p.SubjectID, id).Scan(&count)
		if err != nil {
			return "", err
		}
	case "evidence":
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM evidence e JOIN tasks t ON t.id=e.task_id AND t.workspace_id=e.workspace_id WHERE e.id=?3 AND e.workspace_id=?1 AND `+visible.taskOnT, p.WorkspaceID, p.SubjectID, id).Scan(&count)
		if err != nil {
			return "", err
		}
	default:
		// Events and command receipts are intentionally outside this export
		// projection, so their opaque identities are redacted here.
		return "", nil
	}
	if count != 1 {
		return "", nil
	}
	return reference, nil
}
