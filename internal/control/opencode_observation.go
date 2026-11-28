package control

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

type openCodeObservation struct {
	Boot       string `json:"boot"`
	Generation uint64 `json:"generation"`
	RunID      string `json:"run_id"`
	AttemptID  string `json:"attempt_id"`
	Fence      uint64 `json:"fence"`
	CallID     string `json:"call_id"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
}

var openCodeObservationID = regexp.MustCompile(`^(?:[a-f0-9]{32}|[a-f0-9]{64})$`)

func (s *Server) handleOpenCodeObservation(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !HasRunnerScope(p, "runner:control:read") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	session := r.PathValue("session_id")
	if session == "" {
		http.Error(w, "assignment mismatch", http.StatusConflict)
		return
	}
	var in openCodeObservation
	if r.Method == http.MethodPost {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 56<<10))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
	}
	rows, err := s.store.DB().QueryContext(r.Context(), `SELECT r.id,r.attempt_id,r.fence,r.runner_id FROM runs r
		WHERE r.runtime_session_id=? AND r.harness='opencode' AND r.status IN ('dispatching','active','uncertain')
		AND r.desired_control='run' AND r.stopped_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM runner_observations o WHERE o.run_id=r.id AND o.fence=r.fence AND o.kind='runtime_terminated' AND json_extract(o.body,'$.runtime_session_id')=r.runtime_session_id) LIMIT 2`, session)
	if err != nil {
		http.Error(w, "assignment mismatch", http.StatusConflict)
		return
	}
	defer rows.Close()
	var run, attempt, runner string
	var fence uint64
	if !rows.Next() || rows.Scan(&run, &attempt, &fence, &runner) != nil || rows.Next() || rows.Err() != nil || runner != p.RunnerID || attempt == "" || fence == 0 {
		http.Error(w, "assignment mismatch", http.StatusConflict)
		return
	}
	if r.Method == http.MethodGet {
		generation := modelmonitor.Generation()
		if modelObservationBoot == "" {
			generation = 0
		}
		writeJSON(w, http.StatusOK, map[string]any{"boot": modelObservationBoot, "generation": generation, "run_id": run, "attempt_id": attempt, "fence": fence})
		return
	}
	if in.RunID != run || in.AttemptID != attempt || in.Fence != fence {
		http.Error(w, "assignment mismatch", http.StatusConflict)
		return
	}
	if in.Boot == "" || in.Boot != modelObservationBoot || in.Generation == 0 || !openCodeObservationID.MatchString(in.CallID) || len(in.Text) > 16384 || !utf8.ValidString(in.Text) || (in.Type != "request" && in.Type != "response") {
		http.Error(w, "invalid request", http.StatusBadRequest)
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
	payload, _ := json.Marshal(map[string]any{"text": in.Text})
	modelmonitor.PublishFor(in.Generation, modelmonitor.Event{Component: "opencode_context", CallID: in.CallID, Time: time.Now().UTC(), Type: in.Type, Payload: payload, Truncated: in.Truncated})
	w.WriteHeader(http.StatusNoContent)
}
