package control

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/prompts"
)

func TestTaskDisplayInstructionOnlyOnDisplayClaims(t *testing.T) {
	store, _, _ := instanceAuthFixture(t)
	ctx := context.Background()
	if _, err := store.DB().Exec(`ALTER TABLE tasks ADD COLUMN ui_display INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id) VALUES('prompt-task','default','title','unchanged goal','user','worker','queued',1,'2026-01-01','2026-01-01','instance','prompt-command')`); err != nil {
		t.Fatal(err)
	}
	p, err := prompts.Load("../../deploy/prompts")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	claim := contracts.JobClaim{TaskID: "prompt-task", Goal: "unchanged goal"}
	if err := addTaskDisplayInstruction(ctx, tx, &claim, p.TaskUIDisplay()); err != nil || len(claim.Instructions) != 0 {
		t.Fatalf("non-display claim: %v", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET ui_display=1 WHERE id='prompt-task'`); err != nil {
		t.Fatal(err)
	}
	if err := addTaskDisplayInstruction(ctx, tx, &claim, p.TaskUIDisplay()); err != nil || len(claim.Instructions) != 1 || claim.Instructions[0] != p.TaskUIDisplay() || claim.Goal != "unchanged goal" {
		t.Fatalf("display claim: %v", err)
	}
}
