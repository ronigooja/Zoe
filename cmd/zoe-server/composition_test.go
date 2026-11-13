package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestComposedServerRegistersDerivedWorkerCredentialAndFailsClosedWithoutVoiceGateway(t *testing.T) {
	// Worker-only model configuration must not be parsed by Go startup.
	t.Setenv("ZOE_MEM0_LLM_BASE_URL", "invalid worker-only URL")
	t.Setenv("ZOE_MEM0_LLM_MODEL", "")
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	deviceToken, workerToken := compositionSecret(t), compositionSecret(t)
	tokenFile := filepath.Join(t.TempDir(), "memory-worker-token")
	if err = os.WriteFile(tokenFile, []byte(workerToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOE_MEMORY_RUNNER_TOKEN_FILE", tokenFile)
	t.Setenv("ZOE_MEMORY_RUNNER_ID", "memory-worker-test")
	t.Setenv("ZOE_MEMORY_RUNNER_WORKSPACES", "default")
	t.Setenv("ZOE_MEMORY_RUNNER_SCOPES", "memory:propose,memory:jobs:complete,memory:jobs:claim")
	credential, err := loadMemoryRunnerCredentialFromEnv()
	if err != nil || !credential.Enabled || credential.Token != workerToken || !sameStringSet(credential.Scopes, memoryWorkerScopes) || len(credential.WorkspaceIDs) != 1 || credential.WorkspaceIDs[0] != "default" {
		t.Fatalf("memory runner credential did not load with exact scopes/workspaces: %#v err=%v", credential, err)
	}
	registerConfig, err := registerConfigFromEnv(func(k string) string { return registerEnvFixture()[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
	if err != nil {
		t.Fatal(err)
	}
	server, err := composeControlServer(store, control.Config{}, voice.Config{RegisterConfig: registerConfig}, capabilities.Config{}, memory.DerivedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = registerMemoryRunnerCredential(ctx, store, credential); err != nil {
		t.Fatal(err)
	}
	seedComposedMemoryClaim(t, store.DB())
	handler := server.Handler()
	if _, _, err = control.InitializeInstanceAuth(ctx, store, deviceToken); err != nil {
		t.Fatal(err)
	}
	claim := compositionRequest(t, handler, http.MethodPost, "/runner/v1/memory/jobs/claim", workerToken, map[string]any{"limit": 1})
	if claim.Code != http.StatusOK {
		t.Fatalf("derived worker claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed struct {
		Items []struct {
			ID      string `json:"id"`
			Attempt int    `json:"attempt"`
		} `json:"items"`
	}
	if err = json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil || len(claimed.Items) != 1 {
		t.Fatalf("derived worker did not claim seeded job: body=%s err=%v", claim.Body.String(), err)
	}
	validate := compositionRequest(t, handler, http.MethodPost, "/runner/v1/memory/jobs/"+claimed.Items[0].ID+"/validate", workerToken, map[string]any{"attempt": claimed.Items[0].Attempt})
	if validate.Code != http.StatusOK || !bytes.Contains(validate.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("derived worker lease validation status=%d body=%s", validate.Code, validate.Body.String())
	}
	voiceResponse := compositionRequest(t, handler, http.MethodGet, "/v1/voice/config", deviceToken, nil)
	if voiceResponse.Code != http.StatusServiceUnavailable || !bytes.Contains(voiceResponse.Body.Bytes(), []byte("voice_unavailable")) {
		t.Fatalf("voice without configured gateway did not fail closed: status=%d body=%s", voiceResponse.Code, voiceResponse.Body.String())
	}
	exportResponse := compositionRequest(t, handler, http.MethodGet, "/v1/export?scope=personal", deviceToken, nil)
	if exportResponse.Code != http.StatusOK || exportResponse.Header().Get("Content-Disposition") == "" || !bytes.Contains(exportResponse.Body.Bytes(), []byte(`"format":"zoe.authority-export"`)) {
		t.Fatalf("composed server did not register the scoped export route: status=%d body=%s", exportResponse.Code, exportResponse.Body.String())
	}
}

func TestVoiceAccountMediaAdapterRequiresExplicitSecureGatewayConfiguration(t *testing.T) {
	t.Setenv("ZOE_VOICE_ACCOUNT_MEDIA_ENABLED", "true")
	if _, err := composeControlServer(nil, control.Config{}, voice.Config{}, capabilities.Config{}, memory.DerivedConfig{}); err == nil {
		t.Fatal("account media adapter enabled without a configured HTTPS gateway and deployment key")
	}
}

func seedComposedMemoryClaim(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO memory_sources(workspace_id,id,version,scope_kind,scope_id,principal_id,kind,reference,content_hash,recorded_at,producer) VALUES('default','source-composition',1,'workspace','','','user_note','user_note','',?,'user')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_claims(id,workspace_id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,created_at,updated_at) VALUES('claim-composition','default','workspace','','','composition','derived worker','user_confirmed','active',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_revisions(claim_id,revision,content,authority_kind,status,recorded_at,actor_id) VALUES('claim-composition',1,'synthetic worker composition fixture','user_confirmed','active',?,'owner')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) VALUES('claim-composition',1,'default','source-composition',1)`); err != nil {
		t.Fatal(err)
	}
}

func compositionRequest(t *testing.T, handler http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func compositionSecret(t *testing.T) string {
	t.Helper()
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}
