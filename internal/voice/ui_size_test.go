package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func TestUIScreenCommand(t *testing.T) {
	ts, store, key := registerDisabledHTTPServer(t, Config{})
	for _, payload := range []string{`{}`, `{"width":319,"height":300}`, `{"width":8193,"height":300}`, `{"width":400,"height":239}`, `{"width":400,"height":8193}`, `{"width":400.0,"height":300}`, `{"width":"400","height":300}`, `{"width":400,"height":null}`, `{"width":400,"height":300,"extra":1}`} {
		status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "invalid-"+payload, "ui.screen", json.RawMessage(payload))
		if status != http.StatusBadRequest {
			t.Fatalf("payload=%s status=%d response=%s", payload, status, raw)
		}
	}
	payload := map[string]int{"width": 1440, "height": 900}
	status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "screen-one", "ui.screen", payload)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("status=%d response=%s", status, raw)
	}
	payload["width"] = 1280
	status, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, "screen-two", "ui.screen", payload)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("status=%d response=%s", status, raw)
	}
	payload["width"] = 1440
	status, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, "screen-one", "ui.screen", payload)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("replay status=%d response=%s", status, raw)
	}
	var count, width, height, events int
	if err := store.DB().QueryRow(`SELECT count(*),width,height FROM ui_screens`).Scan(&count, &width, &height); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if count != 1 || width != 1280 || height != 900 || events != 0 {
		t.Fatalf("count=%d width=%d height=%d events=%d", count, width, height, events)
	}
}

func TestUIScreenScope(t *testing.T) {
	r := newDeliveryRig(t)
	tx, err := r.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, _, err = r.svc.commandUIScreen(context.Background(), tx, control.CommandContext{Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}}, contracts.CommandRequest{Payload: json.RawMessage(`{"width":1440,"height":900}`)})
	if api, ok := err.(*control.APIError); !ok || api.Status != http.StatusForbidden {
		t.Fatalf("scope=%v", err)
	}
}

func TestUIJobSize(t *testing.T) {
	r := newDeliveryRig(t)
	uiFlowServer(t, r)
	var mu sync.Mutex
	output := strings.Replace(uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false), `"container":"window"`, `"container":"window","size":{"width":500,"height":400}`, 1)
	r.svc.uiController = uiFlowModel(t, &output, &mu)
	r.seedTask(t, "task", "owner")
	r.exec(t, `UPDATE tasks SET ui_display=1 WHERE id='task'`)
	for i, entry := range []string{"one", "two"} {
		r.seedEntry(t, "task", entry, "result")
		r.seedEvent(t, int64(i*2+1), "task", "run", entry)
		r.exec(t, `UPDATE event_counters SET current_cursor=? WHERE workspace_id='default'`, i*2+1)
		r.svc.consumeUITaskEvents(context.Background())
		r.svc.dispatchUIJobs(context.Background())
		var width, height int
		if err := r.store.DB().QueryRow(`SELECT width,height FROM ui_surfaces`).Scan(&width, &height); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := r.store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='ui.surface' ORDER BY cursor DESC LIMIT 1`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if width != 500 || height != 400 || !strings.Contains(raw, `"size":{"width":500,"height":400}`) {
			t.Fatalf("size=%dx%d event=%s", width, height, raw)
		}
		mu.Lock()
		output = uiFlowOutput(`{"version":"v0.9.1","updateDataModel":{"surfaceId":"screen","value":{}}}`, false)
		mu.Unlock()
	}
	var request string
	if err := r.store.DB().QueryRow(`SELECT id FROM ui_requests LIMIT 1`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	j := uiJob{w: "default", o: "owner", request: request}
	input, err := r.svc.uiInput(context.Background(), j)
	if err != nil || strings.Contains(string(input), `"screen":`) || !strings.Contains(string(input), `"size":{"width":500,"height":400}`) {
		t.Fatalf("input=%s err=%v", input, err)
	}
	r.exec(t, `INSERT INTO ui_screens(workspace_id,owner_subject_id,width,height,updated_at) VALUES('default','other',800,600,?)`, r.now().Format(time.RFC3339Nano))
	input, err = r.svc.uiInput(context.Background(), j)
	if err != nil || strings.Contains(string(input), `"screen":`) {
		t.Fatalf("foreign screen input=%s err=%v", input, err)
	}
	r.exec(t, `INSERT INTO ui_screens(workspace_id,owner_subject_id,width,height,updated_at) VALUES('default','owner',1440,900,?)`, r.now().Format(time.RFC3339Nano))
	input, err = r.svc.uiInput(context.Background(), j)
	if err != nil || !strings.Contains(string(input), `"screen":{"width":1440,"height":900}`) {
		t.Fatalf("input=%s err=%v", input, err)
	}
}

func TestUIJobSizeExceedsScreen(t *testing.T) {
	r := newDeliveryRig(t)
	uiFlowServer(t, r)
	r.exec(t, `INSERT INTO ui_screens(workspace_id,owner_subject_id,width,height,updated_at) VALUES('default','owner',400,300,?)`, r.now().Format(time.RFC3339Nano))
	var mu sync.Mutex
	output := strings.Replace(uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false), `"container":"window"`, `"container":"window","size":{"width":500,"height":400}`, 1)
	r.svc.uiController = uiFlowModel(t, &output, &mu)
	uiFlowRegister(t, r)
	var status, code string
	if err := r.store.DB().QueryRow(`SELECT status,last_error FROM ui_jobs`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "validation_failed" {
		t.Fatalf("status=%s code=%s", status, code)
	}
}
