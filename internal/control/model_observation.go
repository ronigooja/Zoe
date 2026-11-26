package control

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
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
	Text       string `json:"text,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

func (s *Server) handleModelObservation(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
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
		if d.Decode(&in) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
	}
	if in.Attempt == "" || in.Fence == 0 {
		http.Error(w, "invalid request", 400)
		return
	}
	var runner, attempt string
	var fence uint64
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT runner_id,attempt_id,fence FROM runs WHERE id=? AND status IN ('dispatching','active','uncertain')`, r.PathValue("run_id")).Scan(&runner, &attempt, &fence)
	if err != nil || runner != p.RunnerID || attempt != in.Attempt || fence != in.Fence {
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
	if in.Boot == "" || in.Boot != modelObservationBoot || in.Generation == 0 || in.CallID == "" || len(in.CallID) > 128 || len(in.Text) > 16384 || (in.Type != "request" && in.Type != "response" && in.Type != "tool_call") {
		http.Error(w, "invalid request", 400)
		return
	}
	payload, _ := json.Marshal(map[string]any{"origin": "opencode_observed", "not_provider_request": true, "text": in.Text})
	modelmonitor.PublishFor(in.Generation, modelmonitor.Event{Component: "worker", CallID: in.CallID, Time: time.Now().UTC(), Type: in.Type, Payload: payload, Truncated: in.Truncated})
	w.WriteHeader(http.StatusNoContent)
}
