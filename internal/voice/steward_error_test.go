package voice

import (
	"context"
	"errors"
	"testing"

	"github.com/ronigooja/Zoe/internal/steward"
)

func TestStewardFailureCreatesNoTaskAndReportsError(t *testing.T) {
	for name, stewardErr := range map[string]error{"timeout": context.DeadlineExceeded, "error": errors.New("upstream failed")} {
		t.Run(name, func(t *testing.T) {
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "task"}, Steward: &fakeSteward{err: stewardErr}})
			commitAndHandoff(t, ts, key, "steward-fail-"+name, "please handle this")
			var tasks, replies int
			var text, intent, decision string
			if err := store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT count(*), coalesce(max(text),''), coalesce(max(intent_kind),'') FROM voice_fast_replies`).Scan(&replies, &text, &intent); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT steward_decision FROM voice_turns WHERE turn_id=?`, "steward-fail-"+name).Scan(&decision); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || replies != 1 || text != stewardErrorSpeech || intent != "task_control" || decision != string(steward.Error) {
				t.Fatalf("tasks=%d replies=%d text=%q intent=%q decision=%q", tasks, replies, text, intent, decision)
			}
		})
	}
}

func TestStewardSuccessStillCreatesTask(t *testing.T) {
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "task"}, Steward: &fakeSteward{decision: steward.Dispatch}})
	receipt := commitAndHandoff(t, ts, key, "steward-ok", "please handle this")
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, receipt.ResourceID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestStewardFailureRepeatedHandoffRepliesOnce(t *testing.T) {
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "task"}, Steward: &fakeSteward{err: context.DeadlineExceeded}})
	commitAndHandoff(t, ts, key, "steward-repeat", "please handle this")
	commitAndHandoff(t, ts, key, "steward-repeat", "please handle this")
	var replies, tasks int
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies)
	_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
	if replies != 1 || tasks != 0 {
		t.Fatalf("replies=%d tasks=%d", replies, tasks)
	}
}
