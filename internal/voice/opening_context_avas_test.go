package voice

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestAVASOpeningContextRealNonTerminalTasks(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 2000})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, owner := range []string{"user", "other"} {
		if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES(?,?,?,?,?,?)`, "device-"+owner, "default", owner, owner, "approved", now); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range []string{"active", "queued", "waiting", "awaiting_user", "paused", "complete", "failed", "canceled", "active"} {
		owner := "user"
		if i == 8 {
			owner = "other"
		}
		id := fmt.Sprintf("task-%d", i)
		if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+id, "default", owner, "key-"+id, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "default", "任务"+fmt.Sprint(i), "目标", "voice", "default", status, 1, now, now, "device-"+owner, "cmd-"+id, owner); err != nil {
			t.Fatal(err)
		}
	}
	opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"})
	want := []openingContextItem{}
	for _, i := range []int{0, 1, 2, 3} {
		want = append(want, openingContextItem{Role: "user", Text: "进行中的任务:任务" + fmt.Sprint(i)})
	}
	if opening == nil || !reflect.DeepEqual(opening.Items, want) {
		t.Fatalf("opening=%+v want=%+v", opening, want)
	}
	for _, item := range opening.Items {
		if strings.Contains(item.Text, "active task:") {
			t.Fatalf("English task label: %s", item.Text)
		}
	}
}

func TestAVASOpeningContextSkipsStaleWaitingTasksAndDuplicates(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 2000})
	nowT := time.Now().UTC()
	fresh := nowT.Format(time.RFC3339Nano)
	stale := nowT.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-user','default','user','user','approved',?)`, fresh); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ status, title, updated string }{
		{"awaiting_user", "旧的等待任务", stale},
		{"waiting", "旧的等待任务二", stale},
		{"awaiting_user", "刚更新的等待任务", fresh},
		{"active", "重复任务", fresh},
		{"active", "重复任务", fresh},
		{"queued", "排队任务", fresh},
	}
	for i, r := range rows {
		id := fmt.Sprintf("stale-%d", i)
		if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+id, "default", "user", "key-"+id, fresh); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "default", r.title, "目标", "voice", "default", r.status, 1, r.updated, r.updated, "device-user", "cmd-"+id, "user"); err != nil {
			t.Fatal(err)
		}
	}
	opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"})
	if opening == nil {
		t.Fatal("no opening context")
	}
	got := map[string]int{}
	for _, item := range opening.Items {
		got[item.Text]++
	}
	want := map[string]int{"进行中的任务:刚更新的等待任务": 1, "进行中的任务:重复任务": 1, "进行中的任务:排队任务": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items=%v want=%v", got, want)
	}
}
