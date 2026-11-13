package ops_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

// TestMaintenanceRESTEncryptedRestoreDrill is an explicit disposable local
// acceptance drill. Set ZOE_OPS_PYTHON to the isolated Python interpreter
// with tools/data_lifecycle/requirements.txt installed.
func TestMaintenanceRESTEncryptedRestoreDrill(t *testing.T) {
	python := os.Getenv("ZOE_OPS_PYTHON")
	if python == "" {
		t.Skip("set ZOE_OPS_PYTHON to run the encrypted restore drill")
	}
	repo := repositoryRoot(t)
	ctx := context.Background()
	tmp := t.TempDir()
	db := filepath.Join(tmp, "source", "data", "zoe.sqlite")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := openDrillStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	deviceToken := randomBearer(t)
	if _, _, err = control.InitializeInstanceAuth(ctx, store, deviceToken); err != nil {
		t.Fatal(err)
	}
	runnerToken := randomBearer(t)
	if err = control.RegisterRunnerCredentialIfMissing(ctx, store, runnerToken, "ops-drill-runner", []string{"default"}, []string{"runner:control:read"}); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(store, control.Config{InstanceScopes: []string{"instance:admin"}, RunnerToken: runnerToken, RunnerID: "ops-drill-runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:control:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceRoutes(server); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	initial := getInstance(t, httpServer.URL, runnerToken)
	if initial.InstanceID == "" || initial.Epoch != 1 || !initial.DispatchEnabled {
		t.Fatalf("unexpected initial runner instance response: %#v", initial)
	}
	response, err := postJSON(httpServer.URL+"/v1/admin/instance/fence", deviceToken, map[string]any{"reason": "ops restore drill", "expected_epoch": initial.Epoch})
	if err != nil || response.Code != http.StatusOK {
		t.Fatalf("REST fence response code=%d body=%s err=%v", response.Code, response.Body, err)
	}
	fenced := getInstance(t, httpServer.URL, runnerToken)
	if fenced.DispatchEnabled || fenced.Epoch != 2 {
		t.Fatalf("REST fence did not advance and close dispatch: %#v", fenced)
	}
	gateTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	derivedEnabled, err := control.DerivedRetrievalEnabledTx(ctx, gateTx)
	_ = gateTx.Rollback()
	if err != nil || derivedEnabled {
		t.Fatalf("REST fence left derived retrieval enabled: enabled=%t err=%v", derivedEnabled, err)
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(tmp, "backup.key")
	writePrivate(t, keyFile, key)
	backupDir := filepath.Join(tmp, "backups")
	if runPythonExpectFailure(t, python, repo, "tools/zoe_backup.py", "--database", db, "--destination", backupDir, "--key-file", keyFile) == nil {
		t.Fatal("backup was allowed while the source SQLite writer lock was held")
	}
	httpServer.Close()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	dataRoot := filepath.Join(tmp, "managed")
	if err = os.MkdirAll(filepath.Join(dataRoot, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dataRoot, "config", "service.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(tmp, "inventory.json")
	inventoryJSON, _ := json.Marshal(map[string]any{"schema_version": 1, "roots": map[string]string{"managed": dataRoot}, "service_config_paths": []any{map[string]string{"root_id": "managed", "path": "config/service.json"}}, "artifact_paths": []any{}, "persistent_volume_paths": []any{}, "secret_refs": []any{}})
	if err = os.WriteFile(inventory, inventoryJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	volumeInventory := filepath.Join(tmp, "volume-inventory.json")
	volumePath := filepath.Join(dataRoot, "volume.db")
	if err = os.WriteFile(volumePath, []byte("persistent data"), 0o600); err != nil {
		t.Fatal(err)
	}
	volumeJSON, _ := json.Marshal(map[string]any{"schema_version": 1, "roots": map[string]string{"managed": dataRoot}, "service_config_paths": []any{}, "artifact_paths": []any{}, "persistent_volume_paths": []any{map[string]string{"root_id": "managed", "path": "volume.db"}}, "secret_refs": []any{}})
	if err = os.WriteFile(volumeInventory, volumeJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if runPythonExpectFailure(t, python, repo, "tools/zoe_backup.py", "--database", db, "--destination", filepath.Join(tmp, "volume-backups"), "--key-file", keyFile, "--inventory", volumeInventory) == nil {
		t.Fatal("backup was allowed to claim online persistent-volume consistency without a quiesce adapter")
	}
	runPython(t, python, repo, "tools/zoe_backup.py", "--database", db, "--destination", backupDir, "--key-file", keyFile, "--inventory", inventory)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	var artifact string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zoe-backup") {
			if artifact != "" {
				t.Fatal("backup command created more than one encrypted artifact")
			}
			artifact = filepath.Join(backupDir, entry.Name())
		}
	}
	if artifact == "" {
		t.Fatal("backup command did not create an encrypted artifact")
	}
	maintenance := filepath.Join(tmp, "zoe-maintenance")
	build := exec.Command("go", "build", "-o", maintenance, "./cmd/zoe-maintenance")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build maintenance CLI: %v\n%s", err, output)
	}
	latestLedger := filepath.Join(tmp, "latest.deletion-ledger")
	latestBytes, err := os.ReadFile(db + ".deletion-ledger")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err = os.WriteFile(latestLedger, latestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	latestHead := filepath.Join(tmp, "latest-deletion-head.json")
	deletionHead := exec.Command(maintenance, "deletion-head", "--database", db, "--output", latestHead)
	if output, err := deletionHead.CombinedOutput(); err != nil {
		t.Fatalf("write current deletion head: %v\n%s", err, output)
	}
	corrupt := filepath.Join(tmp, "corrupt.zoe-backup")
	artifactBytes, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifactBytes[len(artifactBytes)-1] ^= 0xff
	if err = os.WriteFile(corrupt, artifactBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	badRestore := filepath.Join(tmp, "bad-restore")
	if runPythonExpectFailure(t, python, repo, "tools/zoe_restore.py", "--artifact", corrupt, "--key-file", keyFile, "--latest-deletion-ledger", latestLedger, "--latest-deletion-head-file", latestHead, "--restore-directory", badRestore) == nil {
		t.Fatal("tampered encrypted archive was accepted")
	}
	if _, err = os.Lstat(badRestore); !os.IsNotExist(err) {
		t.Fatalf("tampered archive created a staged restore path: %v", err)
	}
	restore := filepath.Join(tmp, "restore")
	runPython(t, python, repo, "tools/zoe_restore.py", "--artifact", artifact, "--key-file", keyFile, "--latest-deletion-ledger", latestLedger, "--latest-deletion-head-file", latestHead, "--restore-directory", restore)
	pendingDB := filepath.Join(restore, "data", "zoe.sqlite.restore-pending")
	proofKey := make([]byte, 32)
	if _, err = rand.Read(proofKey); err != nil {
		t.Fatal(err)
	}
	proofKeyFile := filepath.Join(tmp, "proof.key")
	writePrivate(t, proofKeyFile, proofKey)
	fakeSystemd := filepath.Join(tmp, "fake-systemd")
	if err = os.MkdirAll(fakeSystemd, 0o700); err != nil {
		t.Fatal(err)
	}
	systemctl := filepath.Join(fakeSystemd, "systemctl")
	fakeSystemctl := []byte("#!/bin/sh\nif [ \"$1\" = \"mask\" ]; then exit 0; fi\nif [ \"$1\" = \"show\" ]; then case \"$3\" in --property=ActiveState) echo inactive;; --property=UnitFileState) echo masked;; *) exit 2;; esac; exit 0; fi\nexit 2\n")
	if err = os.WriteFile(systemctl, fakeSystemctl, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err = os.Setenv("PATH", fakeSystemd+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	proofFile := filepath.Join(tmp, "isolation-proof.json")
	runPython(t, python, repo, "tools/zoe_migrate.py", "--maintenance", maintenance, "--database", db, "--proof-key-file", proofKeyFile, "--proof-output", proofFile, "--unit", "zoe-ops-drill.service", "--stop-mask-and-fence")
	activeDB := filepath.Join(restore, "data", "zoe.sqlite")
	prepare := exec.Command(maintenance, "prepare-restore", "--database", pendingDB, "--activate-as", activeDB, "--new-instance-id", "ops-drill-target", "--expected-epoch", "2")
	if output, err := prepare.CombinedOutput(); err != nil {
		t.Fatalf("prepare restore: %v\n%s", err, output)
	}
	reconcile := exec.Command(maintenance, "reconcile-deletions", "--database", activeDB, "--restore-state", filepath.Join(restore, "restore-state.json"))
	if output, err := reconcile.CombinedOutput(); err != nil {
		t.Fatalf("reconcile trusted deletion head: %v\n%s", err, output)
	}
	assertRestoredAuthority(t, activeDB, false, true, false)
	enable := exec.Command(maintenance, "enable", "--database", activeDB, "--expected-epoch", "3", "--proof-file", proofFile, "--proof-key-file", proofKeyFile)
	if output, err := enable.CombinedOutput(); err != nil {
		t.Fatalf("enable after verified zero-head reconciliation: %v\n%s", err, output)
	}
	assertRestoredAuthority(t, activeDB, true, true, false)
}

type runnerInstance struct {
	InstanceID      string `json:"instance_id"`
	Epoch           int64  `json:"epoch"`
	DispatchEnabled bool   `json:"dispatch_enabled"`
}

func getInstance(t *testing.T, base, token string) runnerInstance {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/runner/v1/instance", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET runner instance status=%d body=%s", resp.StatusCode, body)
	}
	var state runnerInstance
	if err = json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

type responseBody struct {
	Code int
	Body []byte
}

func postJSON(url, token string, body any) (responseBody, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return responseBody{}, err
	}
	defer resp.Body.Close()
	value, _ := io.ReadAll(resp.Body)
	return responseBody{Code: resp.StatusCode, Body: value}, nil
}

func openDrillStore(ctx context.Context, path string) (*storage.Store, error) {
	store, err := storage.Open(ctx, storage.Config{Path: path})
	if err != nil {
		return nil, err
	}
	register := []func(*storage.Store) error{storage.RegisterCoreMigrations, memoryapi.RegisterMigrations, voice.RegisterMigrations, capabilities.RegisterMigrations, control.RegisterInstanceMaintenanceMigration}
	for _, fn := range register {
		if err = fn(store); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	if err = store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func assertRestoredAuthority(t *testing.T, path string, expectedDispatch, expectedDerived, expectedReconciliation bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dispatch, derived, reconciliationRequired int
	if err = db.QueryRow(`SELECT dispatch_enabled,derived_retrieval_enabled,restore_deletion_reconciliation_required FROM zoe_instance_state WHERE singleton=1`).Scan(&dispatch, &derived, &reconciliationRequired); err != nil {
		t.Fatal(err)
	}
	if (dispatch == 1) != expectedDispatch || (derived == 1) != expectedDerived || (reconciliationRequired == 1) != expectedReconciliation {
		t.Fatalf("restored dispatch=%d derived=%d reconciliation_required=%d", dispatch, derived, reconciliationRequired)
	}
}

func signedProof(t *testing.T, key []byte, oldID string, epoch int64, unknown int) []byte {
	t.Helper()
	payload := map[string]any{"schema_version": 1, "old_instance_id": oldID, "old_epoch": epoch, "source_host": "local-ops-drill", "units": []map[string]string{{"name": "zoe-ops-drill.service", "active_state": "inactive", "unit_file_state": "masked"}}, "active_runs": 0, "unknown_effects": unknown, "active_deployment_jobs": 0, "live_deployments": 0, "open_voice_sessions": 0, "verified_at": time.Now().UTC()}
	raw, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	doc, _ := json.Marshal(map[string]string{"payload": base64.RawURLEncoding.EncodeToString(raw), "hmac": hex.EncodeToString(mac.Sum(nil))})
	return doc
}

func runPython(t *testing.T, python, repo, script string, args ...string) {
	t.Helper()
	if output := runPythonRaw(python, repo, script, args...); output.err != nil {
		t.Fatalf("%s failed: %v\n%s", script, output.err, output.text)
	}
}

func runPythonExpectFailure(t *testing.T, python, repo, script string, args ...string) error {
	t.Helper()
	output := runPythonRaw(python, repo, script, args...)
	if output.err == nil {
		return nil
	}
	return output.err
}

func runPythonRaw(python, repo, script string, args ...string) struct {
	err  error
	text string
} {
	commandArgs := append([]string{filepath.Join(repo, script)}, args...)
	command := exec.Command(python, commandArgs...)
	command.Dir = repo
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repo, "tools"), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	return struct {
		err  error
		text string
	}{err, string(output)}
}

func runPythonCode(python, repo, code string, args ...string) struct {
	err  error
	text string
} {
	commandArgs := append([]string{"-c", code}, args...)
	command := exec.Command(python, commandArgs...)
	command.Dir = repo
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repo, "tools"), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	return struct {
		err  error
		text string
	}{err, string(output)}
}

func randomBearer(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func hashBearer(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return digest[:]
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyDB(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate ops drill source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
