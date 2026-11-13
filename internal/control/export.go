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
	return nil
}

type exportCounter struct {
	Projects         int64 `json:"projects"`
	Tasks            int64 `json:"tasks"`
	Dependencies     int64 `json:"task_dependencies"`
	TaskEntries      int64 `json:"task_entries"`
	Approvals        int64 `json:"approvals"`
	Runs             int64 `json:"runs"`
	Evidence         int64 `json:"evidence"`
	VoiceLinks       int64 `json:"voice_associations"`
	VoiceResults     int64 `json:"voice_result_deliveries"`
	VoiceTranscripts int64 `json:"voice_transcript_turns"`
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
		"content":     map[string]any{"inventory": []string{"projects", "tasks", "task_dependencies", "task_entries", "approvals", "runs", "evidence", "voice_associations", "voice_result_deliveries", "voice_transcript_turns", "capability_authorizations"}, "counts": counts},
		"permissions": map[string]any{"scope": scope, "row_visibility": "principal_authorized", "secret_values": "excluded"},
		"artifacts":   map[string]any{"bytes": "referenced_not_included", "voice_audio_bytes": "omitted"},
		"derived":     map[string]any{"mem0": "not_included_rebuildable", "qdrant": "not_included_rebuildable", "cache": "not_included"},
		"omissions":   []string{"backup_credentials_external_volume", "service_configuration_secret_values", "large_artifact_bytes", "voice_audio_bytes", "legacy_voice_turn_delegation_records"},
		"integrity":   map[string]any{"data_bytes": dataBytes, "data_sha256": dataHash},
		"memory":      map[string]any{"included": false, "reason": "Memory is stored in Mem0 and is not included; the worker has no export endpoint."},
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
