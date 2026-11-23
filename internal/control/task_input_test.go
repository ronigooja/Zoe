package control

import (
	"context"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestTypedTaskInputQuestionBinding(t *testing.T) {
	for _, mode := range []string{"append", "answer", "foreign", "expired", "wrong_digest", "wrong_fence", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			store, server, _ := instanceAuthFixture(t)
			now := time.Now().UTC()
			at := now.Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`INSERT INTO tasks(id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_run_id,interaction_action_digest,interaction_question) VALUES('input-task','title','goal','user','profile','waiting',1,?,?,'instance','cmd','owner','awaiting_message','input-run','digest','Which?')`, at, at); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('input-run','input-task','attempt','active','run',1,1,'runner','instance','cmd','goal','profile','opencode','runtime','model','worker',?)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`INSERT INTO pending_runtime_actions(id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('input-question','input-task','input-run',1,'request','worker_question','Which?','digest',?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), at); err != nil {
				t.Fatal(err)
			}
			principal := contracts.Principal{SubjectID: "owner", DeviceID: "instance"}
			switch mode {
			case "foreign":
				principal.SubjectID = "other"
			case "expired":
				_, _ = store.DB().Exec(`UPDATE pending_runtime_actions SET expires_at=?`, now.Add(-time.Second).Format(time.RFC3339Nano))
			case "wrong_digest":
				_, _ = store.DB().Exec(`UPDATE pending_runtime_actions SET action_digest='other'`)
			case "wrong_fence":
				_, _ = store.DB().Exec(`UPDATE pending_runtime_actions SET fence=2`)
			}
			question := "input-question"
			input := TaskInput{Content: "The entire detailed request. Preserve its context.", QuestionID: &question}
			if mode == "append" {
				input.QuestionID = nil
			}
			tx, err := store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = server.AppendTaskInputTx(context.Background(), tx, CommandContext{Principal: principal, AcceptedAt: now}, "input-task", input)
			valid := mode == "append" || mode == "answer" || mode == "duplicate"
			if !valid {
				_ = tx.Rollback()
				if err == nil {
					t.Fatal("invalid answer accepted")
				}
				return
			}
			if err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var decision, interaction string
			var bindings int
			if err := store.DB().QueryRow(`SELECT decision FROM pending_runtime_actions WHERE id='input-question'`).Scan(&decision); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT interaction_state FROM tasks WHERE id='input-task'`).Scan(&interaction); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT count(*) FROM task_question_answers`).Scan(&bindings); err != nil {
				t.Fatal(err)
			}
			if mode == "append" {
				if decision != "" || interaction != "awaiting_message" || bindings != 0 {
					t.Fatalf("addition answered question: %s %s %d", decision, interaction, bindings)
				}
			} else if decision != "answer" || bindings != 1 || interaction != "none" {
				t.Fatalf("answer not bound: %s %s %d", decision, interaction, bindings)
			}
			if mode == "duplicate" {
				tx, err := store.DB().BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, _, err := server.AppendTaskInputTx(context.Background(), tx, CommandContext{Principal: principal, AcceptedAt: now}, "input-task", input); err == nil {
					t.Fatal("answered question accepted twice")
				}
			}
		})
	}
}
