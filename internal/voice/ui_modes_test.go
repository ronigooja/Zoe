package voice

import (
	"context"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/uictl"
)

func TestSingleRouteDisplaySemantics(t *testing.T) {
	for _, route := range []string{"chat", "memory_write", "recall", "task", "task_ui", "ui", "chat_input"} {
		request := `"Complete request, shown on the screen when requested."`
		if route == "chat" {
			request = "null"
		}
		r, err := parseRegisterRoute(`{"route":"` + route + `","request":` + request + `}`)
		if err != nil || r.Exits() != route {
			t.Fatal(r, err)
		}
		if r.Has("task") != (route == "task" || route == "task_ui") || r.Has("ui") != (route == "ui" || route == "task_ui" || route == "chat_input") {
			t.Fatal(r)
		}
	}
	for _, output := range []string{`{"routes":["task"],"request":"work"}`, `{"route":["task"],"request":"work"}`, `{"route":"task","route":"ui","request":"work"}`, `{"route":"chat_input","request":null}`, `{"route":"task_ui","request":"none"}`} {
		if _, err := parseRegisterRoute(output); err == nil {
			t.Fatal("accepted invalid route")
		}
	}
}

func TestChatInputRegisterJob(t *testing.T) {
	r := uiTargetRig(t)
	at := r.now().Format(time.RFC3339Nano)
	r.exec(t, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('input-session','default','owner','device-1','input',X'01',1,'ended',?,?)`, at, at)
	r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request,transcript_seq) VALUES('input-turn','default','input-session',1,'input',1,'type',X'01',?,'input','chat_input','Show an input box on the screen for conversation text.',0)`, at)
	if err := r.svc.applyDelegation(context.Background(), "input-turn"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := r.store.DB().QueryRow(`SELECT id FROM ui_requests WHERE voice_turn_id='input-turn' AND task_id IS NULL`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mode, err := r.svc.uiMode(context.Background(), uiJob{request: id, w: "default", o: "owner"}, nil)
	if err != nil || mode != uictl.ChatInput {
		t.Fatal(mode, err)
	}
}

func uiTestMode(question bool) uictl.Mode {
	if question {
		return uictl.TaskQuestion
	}
	return uictl.Show
}
