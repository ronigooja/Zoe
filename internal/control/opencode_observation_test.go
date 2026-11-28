package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

func TestOpenCodeObservationBindingAndOutput(t *testing.T) {
	s, closeStore := taskEntriesTestServer(t)
	defer closeStore()
	_, err := s.store.DB().Exec(`INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at,runtime_session_id) VALUES('entry-run','entries-task','attempt','active','run',1,1,'runner','entries-device','cmd','goal','worker','opencode','runtime','model','worker','2026-10-24T12:00:00Z','session')`)
	if err != nil {
		t.Fatal(err)
	}
	sub := modelmonitor.SubscribeWithOpenCodeContext()
	defer sub.Close()
	request := func(method, session, runner string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", bytes.NewReader(body))
		r.SetPathValue("session_id", session)
		r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: runner, Scopes: []string{"runner:control:read"}}))
		w := httptest.NewRecorder()
		s.handleOpenCodeObservation(w, r)
		return w
	}
	w := request("GET", "session", "runner", nil)
	if w.Code != 200 {
		t.Fatalf("GET: %d", w.Code)
	}
	var in openCodeObservation
	if err := json.Unmarshal(w.Body.Bytes(), &in); err != nil {
		t.Fatal(err)
	}
	in.CallID, in.Type, in.Text = strings.Repeat("a", 32), "response", "Bearer supersecret "+strings.Repeat("界", 3000)
	post := func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(in)
		return request("POST", "session", "runner", b)
	}
	if code := post().Code; code != 204 {
		t.Fatalf("POST: %d", code)
	}
	select {
	case item := <-sub.Events:
		if item.Event.Component != "opencode_context" || !item.Event.Truncated || bytes.Contains(item.Event.Payload, []byte("supersecret")) {
			t.Fatal("invalid output")
		}
		var payload map[string]string
		if json.Unmarshal(item.Event.Payload, &payload) != nil || len(payload) != 1 || len(payload["text"]) > 8192 {
			t.Fatal("invalid payload")
		}
	default:
		t.Fatal("missing output")
	}
	if code := request("GET", "session", "other", nil).Code; code != 409 {
		t.Fatalf("other runner: %d", code)
	}
	if _, err := s.store.DB().Exec(`UPDATE runs SET runtime_session_id='new-session' WHERE id='entry-run'`); err != nil {
		t.Fatal(err)
	}
	if code := post().Code; code != 409 {
		t.Fatalf("stale session: %d", code)
	}
	if _, err := s.store.DB().Exec(`UPDATE runs SET runtime_session_id='session',fence=2 WHERE id='entry-run'`); err != nil {
		t.Fatal(err)
	}
	if code := post().Code; code != 409 {
		t.Fatalf("stale fence: %d", code)
	}
}
