package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
)

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestClientDiagnosticsAcceptsBoundedRedactedEvents(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	h := server.Handler()
	deviceHash := sha256Hex("instance")
	body := map[string]any{"events": []any{map[string]any{
		"client_request_id": "req-1", "occurred_at": "2026-10-29T00:00:00Z", "app_version": "1.0", "os_version": "macOS", "device_id_hash": deviceHash,
		"phase": "config_request", "request_path": "/v1/voice/config", "http_status": 503, "response_bytes": 42, "error_type": "http_error", "error_summary": "voice_unavailable", "build_version": "42", "source_revision": "abcdef123456", "voice_session_id": "session-1", "voice_generation": 2, "voice_media_epoch": 7, "voice_peer_state": "failed",
	}}}
	data, _ := json.Marshal(body)
	r := instanceRequest(h, http.MethodPost, "/v1/diagnostics/client", key, string(data))
	if r.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM client_diagnostic_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var build, revision, session, peer string
	if err := store.DB().QueryRow(`SELECT build_version, source_revision, voice_session_id, voice_peer_state FROM client_diagnostic_events`).Scan(&build, &revision, &session, &peer); err != nil || build != "42" || revision != "abcdef123456" || session != "session-1" || peer != "failed" {
		t.Fatalf("context=%s/%s/%s/%s err=%v", build, revision, session, peer, err)
	}
	unauth := instanceRequest(h, http.MethodPost, "/v1/diagnostics/client", "", string(data))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth=%d", unauth.Code)
	}
}

func TestClientDiagnosticsRejectsSensitiveFields(t *testing.T) {
	_, server, key := instanceAuthFixture(t)
	data := `{"events":[{"client_request_id":"req-1","occurred_at":"2026-10-29T00:00:00Z","device_id_hash":"` + sha256Hex("instance") + `","phase":"config_request","authorization":"Bearer secret"}]}`
	r := instanceRequest(server.Handler(), http.MethodPost, "/v1/diagnostics/client", key, data)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestClientDiagnosticsDefaultsSuccessfulEventsToInfo(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	body := map[string]any{"events": []any{map[string]any{
		"client_request_id": "req-success", "occurred_at": "2026-10-29T00:00:00Z", "app_version": "1.0", "build_version": "7", "source_revision": "abcdef123456", "os_version": "macOS", "device_id_hash": sha256Hex("instance"),
		"phase": "connected", "request_path": "/v1/voice/sessions/connected", "http_status": 200, "response_bytes": 12, "error_type": "", "error_summary": "",
	}}}
	data, _ := json.Marshal(body)
	if err := EnsureClientPolicy(t.Context(), store.DB()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE client_policy SET log_level='info' WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	r := instanceRequest(server.Handler(), http.MethodPost, "/v1/diagnostics/client", key, string(data))
	if r.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var level string
	if err := store.DB().QueryRow(`SELECT level FROM client_diagnostic_events`).Scan(&level); err != nil || level != "info" {
		t.Fatalf("level=%q err=%v", level, err)
	}
}
