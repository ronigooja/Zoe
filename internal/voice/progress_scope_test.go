package voice

import (
	"context"
	"testing"
	"time"
)

func TestProgressCurrentSessionAndAge(t *testing.T) {
	for _, tc := range []struct {
		name, status, session, want string
		age, bound                  time.Duration
	}{
		{"other_waiting", "waiting", "other", "现在没有在处理的任务。", time.Minute, 0},
		{"current_running", "active", "current", "还在处理,已经10秒了。", 10 * time.Second, 0},
		{"too_old", "waiting", "current", "现在没有在处理的任务。", 7 * time.Hour, 0},
		{"configured_bound", "active", "current", "现在没有在处理的任务。", 2 * time.Minute, time.Minute},
		{"current_complete", "complete", "current", "结果已经给你了,或者已经准备好了。", time.Minute, 0},
		{"current_awaiting", "awaiting_user", "current", "结果已经给你了,或者已经准备好了。", time.Minute, 0},
		{"other_complete", "complete", "other", "现在没有在处理的任务。", time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeliveryRig(t)
			r.seedTask(t, "task", "user")
			r.seedCall(t, "other", "user", 1)
			r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='other'`)
			r.seedCall(t, "current", "user", 1)
			if tc.bound > 0 {
				r.svc.recall.ProgressMaxTaskAge = tc.bound
			}
			at := r.now().Add(-tc.age).Format(time.RFC3339Nano)
			r.exec(t, `UPDATE tasks SET status=?,created_at=?,started_at=? WHERE id='task'`, tc.status, at, at)
			r.exec(t, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES('default',?,1,'turn',1,'task','device-1','cmd-task',?)`, tc.session, at)
			if got := r.svc.progressReply(context.Background(), "default", "user", "current"); got != tc.want {
				t.Fatalf("delivery text=%q want=%q", got, tc.want)
			}
			tx, err := r.store.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			got, err := r.svc.progressReplyTx(context.Background(), tx, "default", "user", "current", "turn", r.now())
			if err != nil || got != tc.want {
				t.Fatalf("admission text=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestResultDeliveryCompletedTask(t *testing.T) {
	r := newDeliveryRig(t)
	r.setup(t, "Completed result.")
	r.exec(t, `UPDATE tasks SET status='complete',finished_at=?`, r.now().Format(time.RFC3339Nano))
	r.seedCall(t, "current", "user-1", 1)
	r.run(t)
	if status, _, attempts := r.row(t, "entry-1"); status != "delivered" || attempts != 1 || r.posts.Load() != 1 {
		t.Fatalf("status=%s attempts=%d posts=%d", status, attempts, r.posts.Load())
	}
}
