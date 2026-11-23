package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/uictl"
)

func uiStoredID(t *testing.T, r *deliveryRig, request string) string {
	t.Helper()
	var id string
	if err := r.store.DB().QueryRow(`SELECT surface_id FROM ui_requests WHERE id=?`, request).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func uiACK(t *testing.T, r *deliveryRig, request string, success bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"request_id": request, "surface_id": uiStoredID(t, r, request), "surface_version": 1, "success": success})
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	cmd := control.CommandContext{Principal: contracts.Principal{SubjectID: "owner", Scopes: []string{"client:command"}}, AcceptedAt: r.now()}
	if _, _, err = r.svc.commandUIRendered(context.Background(), tx, cmd, contracts.CommandRequest{Payload: b}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestUIServerAssignedIDsAndNoRoster(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	// More than 64 retained documents no longer prevents another presentation.
	for i := 0; i < 65; i++ {
		id := fmt.Sprintf("stored-%d", i)
		r.exec(t, `INSERT INTO ui_requests(id,owner_subject_id,source,surface_id,surface_version,created_at,updated_at) VALUES(?,'owner','register',?,1,'now','now')`, id, id)
		r.exec(t, `INSERT INTO ui_surfaces(owner_subject_id,surface_id,container,state_json,version,request_id,created_at,updated_at) VALUES('owner',?,'window','[]',1,?,'now','now')`, id, id)
	}
	r.exec(t, `INSERT INTO ui_requests(id,owner_subject_id,source,request,created_at,updated_at) VALUES('new','owner','register','Show this.','now','now')`)
	r.exec(t, `INSERT INTO ui_jobs(id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts,next_attempt_at,created_at,updated_at) VALUES('new-job','owner','register','new','new',1,'now','now','now')`)
	input, err := r.svc.uiInput(context.Background(), uiJob{o: "owner", request: "new"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(input, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["surfaces"]; ok {
		t.Fatal("controller received stored surfaces")
	}
	o, err := uictl.Parse(uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false), nil, uictl.Show)
	if err != nil {
		t.Fatal(err)
	}
	o = uictl.AssignSurfaceID(o, "stored-0")
	if err = r.svc.commitUIJob(context.Background(), uiJob{id: "new-job", o: "owner", request: "new", attempts: 1}, o, nil); err != nil {
		t.Fatal(err)
	}
	id := uiStoredID(t, r, "new")
	if id == "stored-0" || id == "" {
		t.Fatalf("model id retained: %q", id)
	}
	var raw string
	if err = r.store.DB().QueryRow(`SELECT state_json FROM ui_surfaces WHERE surface_id='stored-0'`).Scan(&raw); err != nil || raw != "[]" {
		t.Fatalf("existing surface changed: %s %v", raw, err)
	}
	if err = r.store.DB().QueryRow(`SELECT state_json FROM ui_surfaces WHERE surface_id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var messages []map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &messages); err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		for kind, p := range m {
			if kind == "version" {
				continue
			}
			var payload struct {
				SurfaceID string `json:"surfaceId"`
			}
			if json.Unmarshal(p, &payload) != nil || payload.SurfaceID != id {
				t.Fatal(raw)
			}
		}
	}
	snapshot, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{SubjectID: "owner"})
	if err != nil || len(snapshot.Surfaces) != 66 {
		t.Fatalf("snapshot=%d err=%v", len(snapshot.Surfaces), err)
	}
}

func TestUISurfaceRetentionAfterACK(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			r.seedTask(t, "task", "owner")
			uiTargetPresentation(t, r, false)
			uiACK(t, r, "request", success)
			var count int
			if err := r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("retained=%d err=%v", count, err)
			}
			snapshot, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{SubjectID: "owner"})
			if err != nil || len(snapshot.Surfaces) != 0 {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestUIUnacknowledgedSurfaceRemainsRecoverable(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	r.seedTask(t, "task", "owner")
	uiTargetPresentation(t, r, false)
	if err := r.svc.pruneUISurfaces(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{SubjectID: "owner"})
	if err != nil || len(snapshot.Surfaces) != 1 || snapshot.Surfaces[0].RenderStatus != "generated" {
		t.Fatalf("pending snapshot=%+v err=%v", snapshot, err)
	}
	raw, err := json.Marshal(snapshot.Surfaces[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["deleted"]; exists {
		t.Fatal("removed field in snapshot")
	}
	foreign, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{SubjectID: "other"})
	if err != nil || len(foreign.Surfaces) != 0 {
		t.Fatalf("foreign snapshot=%+v err=%v", foreign, err)
	}
}

func TestUIQuestionRetention(t *testing.T) {
	for _, stale := range []string{"answered", "replaced", "expired"} {
		t.Run(stale, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			uiTargetQuestion(t, r)
			uiTargetPresentation(t, r, true)
			uiACK(t, r, "request", true)
			p := contracts.Principal{SubjectID: "owner"}
			snapshot, err := r.svc.readUISnapshot(context.Background(), p)
			if err != nil || len(snapshot.Surfaces) != 1 || !snapshot.Surfaces[0].TaskQuestion {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
			switch stale {
			case "answered":
				r.exec(t, `UPDATE tasks SET interaction_state='none' WHERE id='task'`)
			case "replaced":
				r.exec(t, `UPDATE tasks SET interaction_action_digest='replacement' WHERE id='task'`)
			case "expired":
				r.exec(t, `UPDATE pending_runtime_actions SET expires_at='2000-01-01T00:00:00Z' WHERE id='question'`)
			}
			snapshot, err = r.svc.readUISnapshot(context.Background(), p)
			if err != nil || len(snapshot.Surfaces) != 0 {
				t.Fatalf("stale snapshot=%+v err=%v", snapshot, err)
			}
			var count int
			if err = r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale row retained=%d %v", count, err)
			}
		})
	}
}
