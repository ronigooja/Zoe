package voice

import (
	"context"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func TestStopStateReplyTx(t *testing.T) {
	for _, tc := range []struct {
		status, text string
	}{
		{"active", "Stop requested; the task is stopping."},
		{"canceled", "The task has stopped."},
		{"complete", "The task has already completed."},
		{"failed", "The task has already failed."},
	} {
		t.Run(tc.status, func(t *testing.T) {
			db, svc, now := rosterFixture(t)
			seedRosterTask(t, db, now, rosterSeed{id: "task", status: tc.status})
			rosterExec(t, db, `INSERT INTO voice_turns(id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('turn','call',1,'turn',1,'stop',X'01',?,'command')`, now.Format(time.RFC3339Nano))
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			cmd := control.CommandContext{Principal: contracts.Principal{SubjectID: "owner"}, AcceptedAt: now}
			p := delegationIdentity{SessionID: "call", Generation: 1, TurnID: "turn", Revision: 1}
			if err := svc.stopStateReplyTx(ctx, tx, cmd, p, "task"); err != nil {
				t.Fatal(err)
			}
			var text, intent, status string
			if err := tx.QueryRow(`SELECT text,intent_kind,status FROM voice_fast_replies WHERE session_id='call' AND generation=1 AND turn_id='turn'`).Scan(&text, &intent, &status); err != nil {
				t.Fatal(err)
			}
			if text != tc.text || intent != "task_control" || status != "pending" {
				t.Fatalf("reply=%q intent=%q status=%q", text, intent, status)
			}
			var handled int
			if err := tx.QueryRow(`SELECT handoff_handled FROM voice_turns WHERE id='turn'`).Scan(&handled); err != nil || handled != 1 {
				t.Fatalf("handled=%d err=%v", handled, err)
			}
		})
	}
}
