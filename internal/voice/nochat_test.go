package voice

import "testing"

func TestDelegationChatIsUnknownAndCreatesSlowTask(t *testing.T) {
	classifier := &fakeClassifier{category: "chat"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier})
	receipt := commitAndHandoff(t, ts, key, "removed-chat", "please handle this")
	var category string
	if err := store.DB().QueryRow(`SELECT triage_category FROM voice_turns WHERE turn_id='removed-chat'`).Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != "unclassified" {
		t.Errorf("chat reply must be persisted as unknown: category=%q", category)
	}
	var tasks, replies int
	if err := store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || replies != 0 {
		t.Fatalf("unknown reply must take slow task path: tasks=%d fast_replies=%d", tasks, replies)
	}
}
