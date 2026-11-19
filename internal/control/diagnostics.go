package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

const (
	maxDiagnosticBatch   = 50
	maxDiagnosticEvent   = 16 << 10
	maxDiagnosticDevices = 1024
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
	// Serialize the check and reservation, including validation, across requests.
	s.diagnostics.mu.Lock()
	defer s.diagnostics.mu.Unlock()
	now := time.Now().UTC()
	if s.diagnostics.now != nil {
		now = s.diagnostics.now()
	}
	s.diagnostics.prune(now)
	if len(s.diagnostics.devices[deviceHashText])+len(req.Events) > 200 {
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
	type pendingDiagnostic struct {
		id, level string
		event     clientDiagnosticEvent
	}
	pending := make([]pendingDiagnostic, 0, len(req.Events))
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
			writeError(w, apiError(http.StatusBadRequest, "invalid_diagnostic", "level must be error, warn, info, or debug", nil))
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
		ids = append(ids, id)
		pending = append(pending, pendingDiagnostic{id: id, level: level, event: event})
	}
	if len(pending) > 0 {
		s.diagnostics.reserve(deviceHashText, now, len(pending))
	}
	for _, item := range pending {
		event := item.event
		line := fmt.Sprintf("client diagnostic id=%s device_hash=%s phase=%s status=%d error_type=%s level=%s error_summary=%s app_version=%s build_version=%s source_revision=%s request_path=%s", logSafe(item.id, 128), logSafe(deviceHashText, 64), logSafe(event.Phase, 64), event.HTTPStatus, logSafe(event.ErrorType, 64), logSafe(item.level, 16), logSafe(event.ErrorSummary, 128), logSafe(event.AppVersion, 128), logSafe(event.BuildVersion, 64), logSafe(event.SourceRevision, 64), logSafe(event.RequestPath, 128))
		if event.VoiceSessionID != nil {
			line += " voice_session=" + logSafe(*event.VoiceSessionID, 128)
		}
		if event.VoiceGeneration != nil {
			line += fmt.Sprintf(" voice_generation=%d", *event.VoiceGeneration)
		}
		if event.VoiceMediaEpoch != nil {
			line += fmt.Sprintf(" voice_media_epoch=%d", *event.VoiceMediaEpoch)
		}
		if event.VoicePeerState != nil {
			line += " voice_peer_state=" + logSafe(*event.VoicePeerState, 32)
		}
		if event.VoiceDataChannelState != nil {
			line += " voice_data_channel_state=" + logSafe(*event.VoiceDataChannelState, 32)
		}
		logging.Infof("%s", line)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"diagnostic_ids": ids, "dropped": dropped})
}

// diagnosticLimiter is protected by mu; timestamps are ordered by admission.
type diagnosticLimiter struct {
	mu      sync.Mutex
	devices map[string][]time.Time
	now     func() time.Time
}

func (l *diagnosticLimiter) prune(now time.Time) {
	cutoff := now.Add(-time.Minute)
	for device, times := range l.devices {
		i := 0
		for i < len(times) && times[i].Before(cutoff) {
			i++
		}
		if i == len(times) {
			delete(l.devices, device)
		} else {
			l.devices[device] = times[i:]
		}
	}
}

func (l *diagnosticLimiter) reserve(device string, now time.Time, count int) {
	if l.devices == nil {
		l.devices = make(map[string][]time.Time)
	}
	if _, exists := l.devices[device]; !exists && len(l.devices) >= maxDiagnosticDevices {
		oldest := ""
		var at time.Time
		for key, times := range l.devices {
			last := times[len(times)-1]
			if oldest == "" || last.Before(at) {
				oldest, at = key, last
			}
		}
		delete(l.devices, oldest)
	}
	for i := 0; i < count; i++ {
		l.devices[device] = append(l.devices[device], now)
	}
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
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._/:", r)) {
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
