package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"net/http"
	"testing"
)

type reviewPushCallback func(context.Context, control.PushRequest) control.PushResponse

func (f reviewPushCallback) Send(c context.Context, r control.PushRequest) control.PushResponse {
	return f(c, r)
}
func TestReviewPushRevocationStopsAlreadySelectedUnsentDeliveries(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	s, h := reviewServer(t, control.Config{}, func(srv *control.Server) {
		if err := srv.RegisterCommand("review.notification", func(ctx context.Context, tx *sql.Tx, c control.CommandContext, _ contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
			return contracts.CommandResult{}, []contracts.Event{{Type: "run.runtime_terminated", AggregateType: "run", AggregateID: "run-push", AggregateVersion: 1, Payload: json.RawMessage(`{"task_id":"task-push","task_status":"awaiting_user"}`)}}, nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	d := reviewInitializeInstance(t, h, code, token)
	if _, err := s.DB().Exec(`INSERT INTO tasks(id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id) VALUES('task-push','Synthetic','Synthetic','user','unused','awaiting_user',1,'2026-10-21T00:00:00Z','2026-10-21T00:00:00Z',?,'cmd-push')`, d.Device.ID); err != nil {
		t.Fatal(err)
	}
	r := reviewRequest(h, http.MethodPost, "/v1/commands", token, "register", contracts.CommandRequest{Kind: "device.push_token.register", Payload: json.RawMessage(`{"platform":"ios","token":"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"}`)})
	if r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	for _, id := range []string{"notify1", "notify2"} {
		r = reviewRequest(h, http.MethodPost, "/v1/commands", token, id, contracts.CommandRequest{Kind: "review.notification", Payload: json.RawMessage(`{}`)})
		if r.Code != http.StatusOK {
			t.Fatal(r.Body.String())
		}
	}
	var tokenID string
	if err := s.DB().QueryRow(`SELECT id FROM device_push_tokens`).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}
	sends := 0
	sender := reviewPushCallback(func(context.Context, control.PushRequest) control.PushResponse {
		sends++
		if sends == 1 {
			b, _ := json.Marshal(map[string]string{"token_id": tokenID})
			revoked := reviewRequest(h, http.MethodPost, "/v1/commands", token, "revoke", contracts.CommandRequest{Kind: "device.push_token.revoke", Payload: b})
			if revoked.Code != http.StatusOK {
				t.Fatal(revoked.Body.String())
			}
		}
		return control.PushResponse{StatusCode: http.StatusOK}
	})
	if err := control.DispatchPushDeliveries(context.Background(), s, sender, 10); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("sent %d pushes: dispatcher transmitted a queued delivery after token revocation committed", sends)
	}
}
