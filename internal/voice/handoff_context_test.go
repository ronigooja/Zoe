package voice

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestHandoffStableIdentityAndQuestionFence(t *testing.T) {
	db, _, now := rosterFixture(t)
	candidateSeed(t, db, now, rosterSeed{id: "first", status: "complete"}, "shared complete goal")
	candidateSeed(t, db, now, rosterSeed{id: "second", status: "complete", owner: "other-owner"}, "shared complete goal")
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}
	prior, _, err := readPriorTaskContext(context.Background(), tx, p, "first", "turn")
	if err != nil || prior.TaskID != "first" || prior.Goal != "shared complete goal" {
		t.Fatalf("prior=%+v err=%v", prior, err)
	}
	if _, _, err := readPriorTaskContext(context.Background(), tx, p, "second", "turn"); err == nil {
		t.Fatal("foreign duplicate goal selected")
	}
	if err := validatePendingQuestionTx(context.Background(), tx, p, "first", "invented-question", now, "turn"); err == nil {
		t.Fatal("stale/invented question accepted")
	}
	if liveTaskStatus("complete") || !liveTaskStatus("active") {
		t.Fatal("execution eligibility changed")
	}
}
