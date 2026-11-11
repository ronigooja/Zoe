package voice

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTaskFactsExactScopeAndNewestLimit(t *testing.T) {
	db, _, now := rosterFixture(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("history-%02d", i)
		session := "call"
		if i%2 == 0 {
			session = "other-call"
		}
		seedRosterTask(t, db, now, rosterSeed{id: id, status: "complete", session: session, created: now.Add(time.Duration(i) * time.Minute), result: id})
		rosterExec(t, db, `UPDATE tasks SET title='Original request' WHERE id=?`, id)
	}
	for _, seed := range []rosterSeed{{id: "owner-isolation", owner: "other-owner", status: "active"}, {id: "workspace-isolation", workspace: "other-workspace", session: "workspace-call", status: "active"}, {id: "partial", status: "active"}, {id: "case", status: "active"}} {
		seedRosterTask(t, db, now, seed)
		title := "Original request"
		if seed.id == "partial" {
			title += " extra"
		}
		if seed.id == "case" {
			title = "original request"
		}
		rosterExec(t, db, `UPDATE tasks SET title=? WHERE id=?`, title, seed.id)
	}
	facts, err := taskFacts(context.Background(), db, "default", "owner", []string{"Original request", "Original request", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 10 {
		t.Fatalf("got %d facts", len(facts))
	}
	for i, fact := range facts {
		if fact.Request != "Original request" || fact.Result != fmt.Sprintf("history-%02d", 11-i) {
			t.Fatalf("fact %d: %+v", i, fact)
		}
		if fact.FinishedAt != "" {
			t.Fatal("NULL finish must be empty")
		}
	}
	for _, requests := range [][]string{nil, {"Original"}, {"original request "}, {"missing"}} {
		got, err := taskFacts(context.Background(), db, "default", "owner", requests)
		if err != nil || len(got) != 0 {
			t.Fatalf("unexpected match: %+v %v", got, err)
		}
	}
}

func TestTaskFactsFieldsAdditionsAndBounds(t *testing.T) {
	db, _, now := rosterFixture(t)
	finished := now.Add(time.Hour)
	seeds := []rosterSeed{
		{id: "waiting", status: "waiting", interaction: "awaiting_message", question: "Which city?", result: strings.Repeat("界", 2100), created: now},
		{id: "confirmation", status: "awaiting_user", interaction: "explicit_confirmation", question: "Allow?", created: now},
		{id: "active", status: "active", interaction: "awaiting_message", question: "hidden", created: now},
		{id: "none", status: "waiting", question: "hidden", created: now},
		{id: "complete", status: "complete", interaction: "explicit_confirmation", question: "hidden", result: "Full result", created: now, finished: &finished},
	}
	var requests []string
	for _, seed := range seeds {
		seedRosterTask(t, db, now, seed)
		requests = append(requests, "Title "+seed.id)
	}
	for i := -1; i <= 7; i++ {
		content := fmt.Sprintf("addition-%d", i)
		if i == 7 {
			content = strings.Repeat("文", 550)
		}
		rosterExec(t, db, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,'waiting','default','user_message',?,'default-owner',?)`, fmt.Sprintf("entry-%02d", i), content, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}
	rosterExec(t, db, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES('not-user','waiting','default','assistant_message','hidden','default-owner',?)`, finished.Format(time.RFC3339Nano))
	for i, stamp := range []time.Time{now.Add(-time.Second), now} {
		rosterExec(t, db, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,'active','default','user_message','initial','default-owner',?)`, fmt.Sprintf("initial-%d", i), stamp.Format(time.RFC3339Nano))
	}
	facts, err := taskFacts(context.Background(), db, "default", "owner", requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 5 {
		t.Fatalf("facts=%+v", facts)
	}
	wantAdditions := []string{"addition-3", "addition-4", "addition-5", "addition-6", strings.Repeat("文", 500)}
	if !reflect.DeepEqual(facts[0].Additions, wantAdditions) || len([]rune(facts[0].Result)) != 2000 {
		t.Fatalf("bounds/order: %+v", facts[0])
	}
	for i, question := range []string{"Which city?", "Allow?", "", "", ""} {
		if facts[i].Question != question {
			t.Errorf("question %d=%q", i, facts[i].Question)
		}
	}
	if facts[4].FinishedAt != finished.Format(time.RFC3339Nano) || facts[4].Result != "Full result" {
		t.Fatal("missing finished/result")
	}
	if len(facts[2].Additions) != 0 {
		t.Fatal("included entries at or before creation")
	}
	converted := toProgressFacts(facts)
	if converted[0].Request != facts[0].Request || !reflect.DeepEqual(converted[0].Additions, wantAdditions) {
		t.Fatal("conversion lost facts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := taskFacts(ctx, db, "default", "owner", requests); err == nil {
		t.Fatal("ignored query error")
	}
}
