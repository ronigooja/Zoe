package voice

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

var openingTestOwner = contracts.Principal{SubjectID: "user-1", DeviceID: "device-1"}

func reserveTestOpening(t *testing.T, r *deliveryRig, session string) *openingContext {
	t.Helper()
	ctx := context.Background()
	register := r.svc.openingRegisterText(ctx, openingTestOwner)
	tx, err := r.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	opening, err := r.svc.buildAVASOpeningContext(ctx, tx, openingTestOwner, session, register, r.now().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return opening
}

func TestAVASOpeningTaskResultShape(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "full text\n with whitespace ")
	r.seedTask(t, "task-2", "user-1")
	r.seedEntry(t, "task-2", "entry-2", "not shown")
	r.seedEvent(t, 2, "task-2", "run-2", "entry-2")
	r.seedTask(t, "task-3", "user-1")
	r.seedEntry(t, "task-3", "entry-3", "not shown either")
	r.seedEvent(t, 3, "task-3", "run-3", "entry-3")
	r.seedTask(t, "task-4", "user-1")
	r.exec(t, `UPDATE tasks SET status='active',title='not an opening item' WHERE id='task-4'`)
	r.exec(t, `UPDATE tasks SET summary_result='short summary' WHERE id='task-2'`)
	r.exec(t, `UPDATE tasks SET summary_result='' WHERE id='task-3'`)
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "session-private", "user-1", 1)
	opening := reserveTestOpening(t, r, "session-private")
	want := []string{"full text\n with whitespace ", "short summary", ""}
	if len(opening.Items) != len(want) {
		t.Fatalf("items=%+v", opening.Items)
	}
	for i, tail := range want {
		if item := opening.Items[i]; item != (openingContextItem{Role: "user", Text: "Task result:\nGoal: goal\nStatus: awaiting_user\nResult: " + tail}) {
			t.Fatalf("item %d=%+v", i, item)
		}
	}
	wire, err := json.Marshal(opening)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"entry-", "task-", "run-", "session-private", resultHash(want[0]), "content_hash", "chunks"} {
		if strings.Contains(string(wire), private) {
			t.Fatalf("model data included %q: %s", private, wire)
		}
	}
}
