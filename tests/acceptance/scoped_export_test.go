package acceptance

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

var exportInstanceScopes = []string{"client:read", "client:command", "export:read"}

func TestScopedExportExplicitlyOmitsWorkerMemory(t *testing.T) {
	_, handler, token, _, _ := scopedExportServer(t)
	response := reviewRequest(handler, http.MethodGet, "/v1/export?scope=personal", token, "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("export=%d %s", response.Code, response.Body.String())
	}
	manifest, contents := decodeExportPackage(t, response.Body.Bytes(), response.Header())
	if _, exists := contents["memory"]; exists {
		t.Fatal("export included memory")
	}
	omission, ok := manifest["memory"].(map[string]any)
	if !ok || omission["included"] != false || !strings.Contains(fmt.Sprint(omission["reason"]), "Mem0") || !strings.Contains(fmt.Sprint(omission["reason"]), "no export endpoint") {
		t.Fatalf("missing memory omission: %#v", manifest["memory"])
	}
}

func scopedExportServer(t *testing.T) (*storage.Store, http.Handler, string, string, string) {
	t.Helper()
	code, token := reviewToken(t), reviewToken(t)
	store, err := storage.Open(context.Background(), storage.Config{Path: filepath.Join(t.TempDir(), "scoped-export.db")})
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
	server, err := control.NewServer(store, control.Config{InstanceScopes: exportInstanceScopes})
	if err != nil {
		t.Fatal(err)
	}
	if err = memoryapi.RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = voice.RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = capabilities.RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterExportRoutes(server); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := bindReviewHandler(server.Handler(), store)
	registration := reviewInitializeInstance(t, h, code, token)
	var subject string
	if err = store.DB().QueryRow(`SELECT subject_id FROM actors WHERE id=?`, registration.Device.ID).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	return store, h, token, subject, registration.Device.ID
}

func assertExportAuditProof(t *testing.T, db *sql.DB, manifest map[string]any) {
	t.Helper()
	authority, ok := manifest["authority"].(map[string]any)
	if !ok {
		t.Fatalf("export manifest authority is malformed: %#v", manifest["authority"])
	}
	snapshotCursor, err := strconv.ParseInt(authority["snapshot_event_cursor"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	validatedCursor, err := strconv.ParseInt(authority["validated_through_cursor"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	auditCursor, err := strconv.ParseInt(authority["export_audit_event_cursor"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	exportID, ok := manifest["export_id"].(string)
	if !ok || validatedCursor != snapshotCursor+1 || auditCursor != validatedCursor || authority["export_audit_event_id"] != exportID || authority["export_audit_event_type"] != "data.export.prepared" {
		t.Fatalf("manifest does not attest the single prepared audit cursor: %#v", authority)
	}
	var eventID, eventType, aggregateID string
	if err = db.QueryRow(`SELECT event_id,event_type,aggregate_id FROM events WHERE cursor=?`, auditCursor).Scan(&eventID, &eventType, &aggregateID); err != nil {
		t.Fatal(err)
	}
	if eventID != exportID || eventType != authority["export_audit_event_type"] || aggregateID != exportID {
		t.Fatalf("manifest audit proof disagrees with event: id=%s type=%s aggregate=%s", eventID, eventType, aggregateID)
	}
}

func TestScopedExportSizeLimitAndTemporaryCleanup(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	store, handler, token, subject, device := scopedExportServer(t)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	seedExportProjectTask(t, store.DB(), "large-project", "large-task", "default", subject, device, at)
	largeField := strings.Repeat("x", 520<<10)
	for i := 0; i < 130; i++ {
		id := fmt.Sprintf("large-task-%03d", i)
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id,owner_subject_id,lineage_root_task_id,lineage_depth,child_policy_json,root_child_limit) VALUES(?,'default','large-project',?,?, 'user','profile','complete',1,?,?,?,?,?,?,?,0,'{}',0)`, id, id, largeField, at, at, at, device, "command-"+id, subject, id); err != nil {
			t.Fatal(err)
		}
	}
	response := reviewRequest(handler, http.MethodGet, "/v1/export?scope=workspace", token, "", nil)
	if response.Code != http.StatusOK || response.Body.Len() <= 64<<20 {
		t.Fatalf("streamed export=%d body length=%d body=%s", response.Code, response.Body.Len(), response.Body.String())
	}
	decodeExportPackage(t, response.Body.Bytes(), response.Header())
	leftovers, err := filepath.Glob(filepath.Join(tempRoot, "zoe-export-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary export remains after bounded stream: %v err=%v", leftovers, err)
	}
	var audits int
	if err = store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='data.export.prepared'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("streamed export audit=%d err=%v", audits, err)
	}
}

func TestScopedExportRejectsOversizedFieldBeforePreparingArchive(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	store, handler, token, subject, device := scopedExportServer(t)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	seedExportProjectTask(t, store.DB(), "oversized-project", "oversized-task", "default", subject, device, at)
	if _, err := store.DB().Exec(`UPDATE tasks SET goal=? WHERE id='oversized-task'`, strings.Repeat("x", (1<<20)+1)); err != nil {
		t.Fatal(err)
	}
	response := reviewRequest(handler, http.MethodGet, "/v1/export?scope=workspace", token, "", nil)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "export_field_too_large") {
		t.Fatalf("oversized field export=%d %s", response.Code, response.Body.String())
	}
	leftovers, err := filepath.Glob(filepath.Join(tempRoot, "zoe-export-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary export remains after field-bound rejection: %v err=%v", leftovers, err)
	}
	var audits int
	if err = store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='data.export.prepared'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("rejected export audit=%d err=%v", audits, err)
	}
}

func TestScopedExportClientCancelCleansTemporaryFile(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	store, handler, token, subject, device := scopedExportServer(t)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	seedExportProjectTask(t, store.DB(), "cancel-project", "cancel-task", "default", subject, device, at)
	if _, err := store.DB().Exec(`UPDATE tasks SET goal=? WHERE id='cancel-task'`, strings.Repeat("x", 32<<20)); err != nil {
		t.Fatal(err)
	}
	live := httptest.NewServer(handler)
	t.Cleanup(live.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+"/v1/export?scope=workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		leftovers, globErr := filepath.Glob(filepath.Join(tempRoot, "zoe-export-*"))
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(leftovers) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	leftovers, _ := filepath.Glob(filepath.Join(tempRoot, "zoe-export-*"))
	t.Fatalf("temporary export remains after canceled response: %v", leftovers)
}

func assertExportAuditMatchesBody(db *sql.DB, body []byte, contentLength string) error {
	n, err := strconv.Atoi(contentLength)
	if err != nil {
		return fmt.Errorf("invalid content length %q: %w", contentLength, err)
	}
	if n != len(body) {
		return fmt.Errorf("content length=%d body bytes=%d", n, len(body))
	}
	var raw []byte
	err = db.QueryRow(`SELECT payload FROM events WHERE event_type='data.export.prepared' ORDER BY cursor DESC LIMIT 1`).Scan(&raw)
	if err != nil {
		return err
	}
	var event struct {
		Payload struct {
			Bytes  int64  `json:"archive_bytes"`
			SHA256 string `json:"archive_sha256"`
		} `json:"payload"`
	}
	if err = json.Unmarshal(raw, &event); err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if event.Payload.Bytes != int64(len(body)) || event.Payload.SHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("audit does not match actual export: bytes=%d hash=%s actual_bytes=%d actual_hash=%s", event.Payload.Bytes, event.Payload.SHA256, len(body), hex.EncodeToString(digest[:]))
	}
	return nil
}

func decodeExportPackage(t *testing.T, body []byte, headers http.Header) (map[string]any, map[string]any) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("export ZIP is invalid: %v", err)
	}
	if len(archive.File) != 2 || archive.File[0].Name != "manifest.json" || archive.File[1].Name != "data.json" || archive.File[0].Method != zip.Store || archive.File[1].Method != zip.Store {
		t.Fatalf("unexpected export archive entries: %#v", archive.File)
	}
	readEntry := func(file *zip.File) []byte {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	manifestBytes, dataBytes := readEntry(archive.File[0]), readEntry(archive.File[1])
	archiveHash := sha256.Sum256(body)
	manifestHash := sha256.Sum256(manifestBytes)
	if headers.Get("X-Zoe-Archive-SHA256") != hex.EncodeToString(archiveHash[:]) || headers.Get("X-Zoe-Manifest-SHA256") != hex.EncodeToString(manifestHash[:]) {
		t.Fatal("export package hash headers do not match bytes")
	}
	var manifest, data map[string]any
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if err = json.Unmarshal(dataBytes, &data); err != nil {
		t.Fatalf("data JSON: %v", err)
	}
	integrity := manifest["integrity"].(map[string]any)
	dataHash := sha256.Sum256(dataBytes)
	if integrity["data_sha256"] != hex.EncodeToString(dataHash[:]) || integrity["data_bytes"] != float64(len(dataBytes)) {
		t.Fatal("manifest data integrity does not match data.json")
	}
	return manifest, data
}

func TestScopedExportConcurrentWritesReturnOneSnapshotOrSafeRetry(t *testing.T) {
	store, handler, token, subject, deviceID := scopedExportServer(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	seedExportProjectTask(t, store.DB(), "concurrent-project", "concurrent-task", "default", subject, deviceID, now)
	// Many bounded rows give the writer a repeatable window in which to race an
	// authoritative update while the handler builds its on-disk file.
	large := strings.Repeat("x", 512<<10)
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("concurrent-task-%02d", i)
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id,owner_subject_id,lineage_root_task_id,lineage_depth,child_policy_json,root_child_limit) VALUES(?,'default','concurrent-project',?,?, 'user','profile','complete',1,?,?,?,?,?,?,?,0,'{}',0)`, id, id, large, now, now, now, deviceID, "command-"+id, subject, id); err != nil {
			t.Fatal(err)
		}
	}
	live := httptest.NewServer(handler)
	t.Cleanup(live.Close)
	var wg sync.WaitGroup
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest(http.MethodGet, live.URL+"/v1/export?scope=personal", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := live.Client().Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()
	changed := false
	for i := 0; i < 32; i++ {
		_, err := store.DB().Exec(`UPDATE tasks SET result=?,version=version+1,updated_at=? WHERE id='concurrent-task'`, fmt.Sprintf("revision-%d", i), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.DB().Exec(`UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id='default'`); err != nil {
			t.Fatal(err)
		}
		changed = true
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	resp := <-respCh
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("concurrent fixture did not update")
	}
	if resp.StatusCode == http.StatusOK {
		_, document := decodeExportPackage(t, body, resp.Header)
		tasks := document["tasks"].([]any)
		if len(tasks) != 1 {
			t.Fatalf("snapshot task count=%d", len(tasks))
		}
		got := tasks[0].(map[string]any)
		if got["result"] != "" && !strings.HasPrefix(got["result"].(string), "revision-") {
			t.Fatalf("mixed task version in export: %#v", got)
		}
	} else if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "export_snapshot_stale") {
		t.Fatalf("concurrent authority change had unsafe outcome: status=%d body=%s", resp.StatusCode, body)
	}
}

func seedExportProjectTask(t *testing.T, db *sql.DB, projectID, taskID, workspace, owner, deviceID, at string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects(id,workspace_id,name,description,status,version,created_at,updated_at,owner_subject_id) VALUES(?,?,?,'','active',1,?,?,?)`, projectID, workspace, projectID, at, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks(id,workspace_id,project_id,title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id,owner_subject_id,lineage_root_task_id,lineage_depth,child_policy_json,root_child_limit) VALUES(?,?,?,?,?,'user','profile','complete',1,?,?,?,?,?,?,?,0,'{}',0)`, taskID, workspace, projectID, taskID, "goal-"+taskID, at, at, at, deviceID, "command-"+taskID, owner, taskID); err != nil {
		t.Fatal(err)
	}
	entryID := "entry-" + taskID
	if _, err := db.Exec(`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,?,?,'user_message',?,?,?)`, entryID, taskID, workspace, "entry-"+taskID, deviceID, at); err != nil {
		t.Fatal(err)
	}
}
