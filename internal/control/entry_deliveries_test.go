package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestRunEntryDeliveriesDurableOrderingAndGates(t *testing.T) {
	s, closeStore := taskEntriesTestServer(t)
	defer closeStore()
	_, err := s.store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('entry-run','default','entries-task','attempt','active','run',1,1,'runner','entries-device','cmd','goal','worker','opencode','runtime','model','worker','2026-10-24T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ id, stamp string }{{"b", "2026-10-24T12:00:00.1Z"}, {"a", "2026-10-24T12:00:00Z"}, {"c", "2026-10-24T12:00:00.100000001Z"}} {
		if _, err = s.store.DB().Exec(`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,'entries-task','default','user_message',?,'entries-device',?)`, e.id, e.id, e.stamp); err != nil {
			t.Fatal(err)
		}
	}
	principal := contracts.RunnerPrincipal{RunnerID: "runner", WorkspaceIDs: []string{"default"}, Scopes: []string{"runner:control:read"}}
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.SetPathValue("run_id", "entry-run")
		r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, principal))
		w := httptest.NewRecorder()
		if method == "GET" {
			s.handleRunControl(w, r)
		} else {
			s.handleEntryDelivery(w, r)
		}
		return w
	}
	pending := func() []string {
		w := request("GET", "/runner/v1/runs/entry-run", nil)
		if w.Code != 200 {
			t.Fatalf("control: %d %s", w.Code, w.Body.String())
		}
		var control contracts.RunControl
		if err := json.Unmarshal(w.Body.Bytes(), &control); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, e := range control.Entries {
			ids = append(ids, e.ID)
		}
		return ids
	}
	if got := pending(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("order=%v", got)
	}
	ack := []byte(`{"attempt_id":"attempt","fence":1,"entry_id":"a"}`)
	for i := 0; i < 2; i++ {
		if w := request("POST", "/runner/v1/runs/entry-run/entry-deliveries", ack); w.Code != 204 {
			t.Fatalf("ack %d: %s", w.Code, w.Body.String())
		}
	}
	if got := pending(); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("after ack=%v", got)
	}
	// A new server instance reads the same durable delivery table.
	s = &Server{store: s.store}
	if got := pending(); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("restart=%v", got)
	}
	for _, state := range []string{"awaiting_message", "explicit_confirmation"} {
		if _, err = s.store.DB().Exec(`UPDATE tasks SET interaction_state=? WHERE id='entries-task'`, state); err != nil {
			t.Fatal(err)
		}
		if got := pending(); len(got) != 0 {
			t.Fatalf("%s delivered %v", state, got)
		}
	}
	if _, err = s.store.DB().Exec(`UPDATE tasks SET interaction_state='none',status='complete' WHERE id='entries-task'`); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 0 {
		t.Fatalf("terminal task=%v", got)
	}
	if _, _, err := appendTaskMessageForTest(t, s, "entries-task", "late", 1); err == nil {
		t.Fatal("terminal append accepted")
	}
}

func TestRunEntryPromptSnapshotsExcludeRedelivery(t *testing.T) {
	s, closeStore := taskEntriesTestServer(t)
	defer closeStore()
	_, err := s.store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('snapshot-run','default','entries-task','attempt','active','run',1,1,'runner','entries-device','cmd','goal','worker','opencode','runtime','model','worker','2026-10-24T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"initial", "rotation", "answer"} {
		if _, err = s.store.DB().Exec(`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,'entries-task','default','user_message',?,'entries-device','2026-10-24T12:00:00Z')`, kind, kind); err != nil {
			t.Fatal(err)
		}
		tx, err := s.store.DB().BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = markRunEntries(context.Background(), tx, "snapshot-run", kind, []contracts.RunEntry{{ID: kind}}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	entries, err := orderedRunEntries(context.Background(), tx, "snapshot-run", true)
	if err != nil || len(entries) != 0 {
		t.Fatalf("redelivered=%v error=%v", entries, err)
	}
}
