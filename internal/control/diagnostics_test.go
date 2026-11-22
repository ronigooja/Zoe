package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ronigooja/Zoe/internal/limits"
	"github.com/ronigooja/Zoe/internal/logging"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func captureDiagnosticLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	output, flags, level := log.Writer(), log.Flags(), logging.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { log.SetOutput(output); log.SetFlags(flags); logging.SetLevel(level) })
	return &logs
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestClientDiagnosticsAcceptsBoundedRedactedEvents(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	logs := captureDiagnosticLogs(t)
	h := server.Handler()
	deviceHash := sha256Hex("instance")
	body := map[string]any{"events": []any{map[string]any{
		"client_request_id": "req-1", "occurred_at": "2026-10-29T00:00:00Z", "app_version": "1.0", "os_version": "macOS", "device_id_hash": deviceHash,
		"phase": "config_request", "request_path": "/v1/voice/config", "http_status": 503, "response_bytes": 42, "error_type": "http_error", "error_summary": "voice_unavailable", "build_version": "42", "source_revision": "abcdef123456", "voice_session_id": "session-1", "voice_generation": 2, "voice_media_epoch": 7, "voice_peer_state": "failed", "voice_data_channel_state": "closed",
	}}}
	data, _ := json.Marshal(body)
	r := instanceRequest(h, http.MethodPost, "/v1/diagnostics/client", key, string(data))
	if r.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var result struct {
		IDs     []string `json:"diagnostic_ids"`
		Dropped int      `json:"dropped"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || len(result.IDs) != 1 || result.Dropped != 0 {
		t.Fatalf("response=%s err=%v", r.Body, err)
	}
	want := fmt.Sprintf("level=info client diagnostic id=%s device_hash=%s phase=config_request status=503 error_type=http_error level=error error_summary=voice_unavailable app_version=1.0 build_version=42 source_revision=abcdef123456 request_path=/v1/voice/config voice_session=session-1 voice_generation=2 voice_media_epoch=7 voice_peer_state=failed voice_data_channel_state=closed\n", result.IDs[0], deviceHash)
	if logs.String() != want {
		t.Fatalf("log=%q want=%q", logs.String(), want)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM client_diagnostic_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
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
	logs := captureDiagnosticLogs(t)
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
	if !strings.Contains(logs.String(), " level=info error_summary=") {
		t.Fatalf("log=%q", logs.String())
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM client_diagnostic_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestClientDiagnosticsLevelFiltering(t *testing.T) {
	levels := []string{"error", "warn", "info", "debug"}
	for policyRank, policyLevel := range levels {
		t.Run(policyLevel, func(t *testing.T) {
			store, server, key := instanceAuthFixture(t)
			logs := captureDiagnosticLogs(t)
			if _, err := server.UpdateClientPolicy(t.Context(), policyLevel, 60); err != nil {
				t.Fatal(err)
			}
			for eventRank, eventLevel := range levels {
				body := map[string]any{"events": []any{map[string]any{
					"client_request_id": "req-" + eventLevel, "occurred_at": "2026-10-29T00:00:00Z",
					"device_id_hash": sha256Hex("instance"), "phase": "test", "level": eventLevel,
				}}}
				logs.Reset()
				data, _ := json.Marshal(body)
				r := instanceRequest(server.Handler(), http.MethodPost, "/v1/diagnostics/client", key, string(data))
				if r.Code != http.StatusAccepted {
					t.Fatalf("event=%s status=%d body=%s", eventLevel, r.Code, r.Body.String())
				}
				want := 0
				if eventRank <= policyRank {
					want = 1
				}
				got := strings.Count(logs.String(), "client diagnostic id=")
				if got != want || (want == 1 && !strings.Contains(logs.String(), " level="+eventLevel+" ")) {
					t.Fatalf("event=%s log=%q", eventLevel, logs.String())
				}
				var count int
				if err := store.DB().QueryRow(`SELECT count(*) FROM client_diagnostic_events`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("count=%d err=%v", count, err)
				}
				var result struct {
					Dropped int `json:"dropped"`
				}
				if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.Dropped != 1-want {
					t.Fatalf("event=%s dropped=%d err=%v", eventLevel, result.Dropped, err)
				}
			}
		})
	}
}

func TestClientDiagnosticsRejectsInvalidLevel(t *testing.T) {
	_, server, key := instanceAuthFixture(t)
	data := `{"events":[{"client_request_id":"req-invalid","occurred_at":"2026-10-29T00:00:00Z","device_id_hash":"` + sha256Hex("instance") + `","phase":"test","level":"verbose"}]}`
	r := instanceRequest(server.Handler(), http.MethodPost, "/v1/diagnostics/client", key, data)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestDiagnosticLogInjection(t *testing.T) {
	_, server, key := instanceAuthFixture(t)
	logs := captureDiagnosticLogs(t)
	body := map[string]any{"events": []any{map[string]any{"client_request_id": "x", "occurred_at": "2026-10-29T00:00:00Z", "device_id_hash": sha256Hex("instance"), "phase": "test", "error_summary": "oops \nstatus=200 phase=x"}}}
	data, _ := json.Marshal(body)
	r := instanceRequest(server.Handler(), http.MethodPost, "/v1/diagnostics/client", key, string(data))
	if r.Code != 202 || strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "error_summary=oops__status_200_phase_x ") || strings.Contains(logs.String(), " status=200") || strings.Contains(logs.String(), " phase=x") {
		t.Fatalf("status=%d log=%q", r.Code, logs.String())
	}
}

func TestDiagnosticRateLimit(t *testing.T) {
	_, server, key := instanceAuthFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	server.diagnostics.now = func() time.Time { return now }
	send := func(device string, n int) int {
		events := make([]any, n)
		for i := range events {
			events[i] = map[string]any{"client_request_id": "x", "occurred_at": "2026-10-29T00:00:00Z", "device_id_hash": sha256Hex(device), "phase": "test", "level": "error"}
		}
		data, _ := json.Marshal(map[string]any{"events": events})
		req := httptest.NewRequest(http.MethodPost, "/v1/diagnostics/client", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Zoe-Installation-ID", device)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		if w.Code == 429 && !strings.Contains(w.Body.String(), "diagnostics_rate_limited") {
			t.Fatalf("body=%s", w.Body.String())
		}
		return w.Code
	}
	for i := 0; i < 4; i++ {
		if got := send("instance", 50); got != 202 {
			t.Fatalf("batch=%d status=%d", i, got)
		}
	}
	if got := send("instance", 1); got != 429 {
		t.Fatalf("status=%d", got)
	}
	if got := send("other", 1); got != 202 {
		t.Fatalf("other status=%d", got)
	}
	now = now.Add(time.Minute + time.Nanosecond)
	if got := send("instance", 1); got != 202 {
		t.Fatalf("after window status=%d", got)
	}
}

func TestDiagnosticLimiterBounded(t *testing.T) {
	var limiter diagnosticLimiter
	now := time.Now()
	maxDiagnosticDevices := limits.Defaults().Int("diagnostic_max_devices")
	for i := 0; i < maxDiagnosticDevices+10; i++ {
		limiter.prune(now)
		limiter.reserve(fmt.Sprint(i), now, 1, maxDiagnosticDevices)
		now = now.Add(time.Nanosecond)
	}
	if len(limiter.devices) != maxDiagnosticDevices {
		t.Fatalf("devices=%d", len(limiter.devices))
	}
	if _, ok := limiter.devices["0"]; ok {
		t.Fatal("oldest device not evicted")
	}
	limiter.prune(now.Add(time.Minute))
	if len(limiter.devices) != 0 {
		t.Fatalf("stale devices=%d", len(limiter.devices))
	}
}
