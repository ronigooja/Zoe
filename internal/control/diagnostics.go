package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	maxDiagnosticBatch  = 50
	maxDiagnosticEvent  = 16 << 10
	diagnosticRetention = 30 * 24 * time.Hour
)

type clientDiagnosticRequest struct {
	Events []clientDiagnosticEvent `json:"events"`
}

type clientDiagnosticEvent struct {
	ClientRequestID       string  `json:"client_request_id"`
	OccurredAt            string  `json:"occurred_at"`
	AppVersion            string  `json:"app_version"`
	BuildVersion          string  `json:"build_version"`
	SourceRevision        string  `json:"source_revision"`
	OSVersion             string  `json:"os_version"`
	DeviceIDHash          string  `json:"device_id_hash"`
	Phase                 string  `json:"phase"`
	RequestPath           string  `json:"request_path"`
	HTTPStatus            int     `json:"http_status"`
	ResponseBytes         int64   `json:"response_bytes"`
	ErrorType             string  `json:"error_type"`
	ErrorSummary          string  `json:"error_summary"`
	Level                 string  `json:"level,omitempty"`
	VoiceSessionID        *string `json:"voice_session_id,omitempty"`
	VoiceGeneration       *int64  `json:"voice_generation,omitempty"`
	VoiceMediaEpoch       *uint64 `json:"voice_media_epoch,omitempty"`
	VoicePeerState        *string `json:"voice_peer_state,omitempty"`
	VoiceDataChannelState *string `json:"voice_data_channel_state,omitempty"`
}

func (s *Server) registerDiagnosticsRoute() {
	s.device.HandleFunc("POST /v1/diagnostics/client", s.handleClientDiagnostics)
}

func (s *Server) handleClientDiagnostics(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !HasScope(p, "client:read") && !HasScope(p, "client:command") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "client scope is required", nil))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, apiError(http.StatusRequestEntityTooLarge, "diagnostics_too_large", "diagnostic batch is too large", nil))
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "diagnostic batch must be JSON", nil))
		return
	}
	if hasSensitiveDiagnosticKey(raw) {
		writeError(w, apiError(http.StatusBadRequest, "sensitive_field", "diagnostic payload contains a prohibited field", nil))
		return
	}
	var req clientDiagnosticRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Events) == 0 || len(req.Events) > maxDiagnosticBatch {
		writeError(w, apiError(http.StatusBadRequest, "invalid_request", "events must contain between 1 and 50 entries", nil))
		return
	}
	deviceInput := strings.TrimSpace(r.Header.Get("X-Zoe-Installation-ID"))
	if deviceInput == "" {
		deviceInput = p.DeviceID
	}
	deviceHash := sha256.Sum256([]byte(deviceInput))
	deviceHashText := hex.EncodeToString(deviceHash[:])
	now := time.Now().UTC()
	cutoff := now.Add(-time.Minute).Format(time.RFC3339Nano)
	var recent int
	if err := s.store.DB().QueryRowContext(r.Context(), `SELECT count(*) FROM client_diagnostic_events WHERE device_hash=? AND received_at>=?`, deviceHashText, cutoff).Scan(&recent); err != nil {
		writeError(w, err)
		return
	}
	if recent+len(req.Events) > 200 {
		writeError(w, apiError(http.StatusTooManyRequests, "diagnostics_rate_limited", "diagnostic rate limit exceeded", nil))
		return
	}
	ids := make([]string, 0, len(req.Events))
	dropped := 0
	policy, err := s.ReadClientPolicy(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	for _, event := range req.Events {
		encoded, _ := json.Marshal(event)
		if len(encoded) > maxDiagnosticEvent || !validDiagnosticEvent(event) || !strings.EqualFold(strings.TrimSpace(event.DeviceIDHash), deviceHashText) {
			writeError(w, apiError(http.StatusBadRequest, "invalid_diagnostic", "diagnostic event is invalid or too large", nil))
			return
		}
		level := strings.ToLower(strings.TrimSpace(event.Level))
		if level == "" {
			level = "error"
			if event.HTTPStatus >= 200 && event.HTTPStatus < 300 && strings.TrimSpace(event.ErrorType) == "" && strings.TrimSpace(event.ErrorSummary) == "" {
				level = "info"
			}
		}
		if !validClientLogLevel(level) {
			writeError(w, apiError(http.StatusBadRequest, "invalid_diagnostic", "level must be error, info, or debug", nil))
			return
		}
		if diagnosticLevelRank(level) > diagnosticLevelRank(policy.LogLevel) {
			dropped++
			continue
		}
		id, err := randomID()
		if err != nil {
			writeError(w, err)
			return
		}
		occurred := strings.TrimSpace(event.OccurredAt)
		if _, err := time.Parse(time.RFC3339Nano, occurred); err != nil {
			writeError(w, apiError(http.StatusBadRequest, "invalid_diagnostic", "occurred_at must be RFC3339", nil))
			return
		}
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO client_diagnostic_events(id,workspace_id,subject_id,device_hash,client_request_id,occurred_at,received_at,app_version,build_version,source_revision,os_version,phase,request_path,http_status,response_bytes,error_type,error_summary,level,voice_session_id,voice_generation,voice_media_epoch,voice_peer_state,voice_data_channel_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.WorkspaceID, p.SubjectID, deviceHashText, strings.TrimSpace(event.ClientRequestID), occurred, now.Format(time.RFC3339Nano), bounded(event.AppVersion, 128), bounded(event.BuildVersion, 64), bounded(event.SourceRevision, 64), bounded(event.OSVersion, 128), bounded(event.Phase, 64), bounded(event.RequestPath, 256), event.HTTPStatus, event.ResponseBytes, bounded(event.ErrorType, 64), bounded(event.ErrorSummary, 512), level, boundedPtr(event.VoiceSessionID, 128), boundedInt64Ptr(event.VoiceGeneration), boundedUint64Ptr(event.VoiceMediaEpoch), boundedPtr(event.VoicePeerState, 32), boundedPtr(event.VoiceDataChannelState, 32)); err != nil {
			writeError(w, err)
			return
		}
		ids = append(ids, id)
		log.Printf("client diagnostic id=%s device_hash=%s phase=%s status=%d error_type=%s", id, deviceHashText, logSafe(event.Phase, 64), event.HTTPStatus, logSafe(event.ErrorType, 64))
	}
	// Keep this table short-lived and independent from the business event stream.
	_, _ = tx.ExecContext(r.Context(), `DELETE FROM client_diagnostic_events WHERE received_at < ?`, now.Add(-diagnosticRetention).Format(time.RFC3339Nano))
	if err := tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"diagnostic_ids": ids, "dropped": dropped})
}

func boundedPtr(value *string, max int) any {
	if value == nil {
		return nil
	}
	return bounded(*value, max)
}
func boundedInt64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	if *value < 0 {
		return nil
	}
	return *value
}
func boundedUint64Ptr(value *uint64) any {
	if value == nil {
		return nil
	}
	return *value
}

func validDiagnosticEvent(e clientDiagnosticEvent) bool {
	return strings.TrimSpace(e.ClientRequestID) != "" && strings.TrimSpace(e.DeviceIDHash) != "" && len(e.DeviceIDHash) <= 128 && strings.TrimSpace(e.Phase) != "" && len(e.Phase) <= 64 && e.HTTPStatus >= 0 && e.HTTPStatus <= 599 && e.ResponseBytes >= 0 && e.ResponseBytes <= 1<<30 && !containsSensitiveValue(e.ErrorSummary) && !containsSensitiveValue(e.RequestPath)
}

func bounded(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

func logSafe(value string, max int) string {
	value = bounded(value, max)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '%' {
			return '_'
		}
		return r
	}, value)
}

func containsSensitiveValue(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"bearer ", "authorization:", "api_key=", "apikey=", "secret=", "password=", "v=0", "candidate:", "ice-pwd:", "ice-ufrag:", "transcript:"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasSensitiveDiagnosticKey(values map[string]json.RawMessage) bool {
	for key, raw := range values {
		lower := strings.ToLower(key)
		if lower == "authorization" || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "api_key") || strings.Contains(lower, "header") || strings.HasSuffix(lower, "_body") || strings.Contains(lower, "sdp") || strings.HasPrefix(lower, "ice_") || strings.HasSuffix(lower, "_ice") || strings.Contains(lower, "audio") || strings.Contains(lower, "transcript") || strings.Contains(lower, "credential") || strings.Contains(lower, "password") {
			return true
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && hasSensitiveDiagnosticKey(nested) {
			return true
		}
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			for _, item := range list {
				var object map[string]json.RawMessage
				if json.Unmarshal(item, &object) == nil && hasSensitiveDiagnosticKey(object) {
					return true
				}
			}
		}
	}
	return false
}
