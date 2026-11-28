package control

import (
	"errors"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
	"github.com/ronigooja/Zoe/internal/modelmonitor/diagnostics"
)

// A generation is meaningful only within the process which issued it.
var modelObservationBoot = func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}()

type modelObservation struct {
	Boot       string `json:"boot"`
	Generation uint64 `json:"generation"`
	Attempt    string `json:"attempt_id"`
	Fence      uint64 `json:"fence"`
	CallID     string `json:"call_id"`
	Type       string `json:"type"`
	Source     string `json:"source"`
	Text       string `json:"text,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

var modelObservationID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var modelObservationTool = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
var modelObservationCredential = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization)\s*[:=]\s*["']?)[^\s"',;]+|\b(?:sk|ghp|github_pat)_[A-Za-z0-9_-]{8,}\b|\bsk-[A-Za-z0-9_-]{8,}\b`)

func (s *Server) handleModelObservation(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
		diagnostics.Record("ingress", "forbidden", "", "", 403)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in modelObservation
	if r.Method == http.MethodGet {
		in.Attempt = r.URL.Query().Get("attempt_id")
		in.Fence, _ = strconv.ParseUint(r.URL.Query().Get("fence"), 10, 64)
	} else {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 20<<10))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			reason := "invalid_json"
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				reason = "body_too_large"
			}
			diagnostics.Record("ingress", reason, r.PathValue("run_id"), "", 400)
			http.Error(w, "invalid request", 400)
			return
		}
	}
	if in.Attempt == "" || in.Fence == 0 {
		diagnostics.Record("ingress", "invalid_assignment", r.PathValue("run_id"), in.Type, 400)
		http.Error(w, "invalid request", 400)
		return
	}
	var runner, attempt string
	var fence uint64
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT runner_id,attempt_id,fence FROM runs WHERE id=? AND status IN ('dispatching','active','uncertain')`, r.PathValue("run_id")).Scan(&runner, &attempt, &fence)
	if err != nil || runner != p.RunnerID || attempt != in.Attempt || fence != in.Fence {
		diagnostics.Record("ingress", "assignment_mismatch", r.PathValue("run_id"), in.Type, 409)
		http.Error(w, "assignment mismatch", 409)
		return
	}
	if r.Method == http.MethodGet {
		generation := modelmonitor.Generation()
		if modelObservationBoot == "" {
			generation = 0
		}
		writeJSON(w, http.StatusOK, map[string]any{"boot": modelObservationBoot, "generation": generation})
		return
	}
	if in.Boot == "" || in.Boot != modelObservationBoot || in.Generation == 0 || !modelObservationID.MatchString(in.CallID) || len(in.Text) > 16384 || (in.Source != "worker" && in.Source != "worker_rotation") || (in.Type != "request" && in.Type != "response" && in.Type != "tool_call" && in.Type != "gap") || (in.Source == "worker_rotation" && in.Type == "tool_call") || (in.Type == "tool_call" && !modelObservationTool.MatchString(in.Text)) || (in.Type == "gap" && (in.Text != "" || in.Truncated)) {
		diagnostics.Record("ingress", "invalid_observation", r.PathValue("run_id"), in.Type, 400)
		http.Error(w, "invalid request", 400)
		return
	}
	if in.Type == "gap" {
		modelmonitor.GapFor(in.Generation)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, secret := range []string{s.cfg.RunnerToken, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")} {
		if secret != "" {
			in.Text = strings.ReplaceAll(in.Text, secret, "[redacted]")
		}
	}
	in.Text = modelObservationCredential.ReplaceAllString(in.Text, "[redacted]")
	if len(in.Text) > 8192 {
		in.Text = in.Text[:8192]
		for !utf8.ValidString(in.Text) {
			in.Text = in.Text[:len(in.Text)-1]
		}
		in.Truncated = true
	}
	payload, _ := json.Marshal(map[string]any{"origin": "opencode_observed", "not_provider_request": true, "text": in.Text})
	modelmonitor.PublishFor(in.Generation, modelmonitor.Event{Component: in.Source, CallID: in.CallID, Time: time.Now().UTC(), Type: in.Type, Payload: payload, Truncated: in.Truncated})
	w.WriteHeader(http.StatusNoContent)
}
