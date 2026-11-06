package voice

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/steward"
	"net/http"
	"testing"
)

func TestStewardSpokenConfirmationUsesExistingAppend(t *testing.T) {
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: &fakeSteward{decision: steward.Ask}})
	first := commitAndHandoff(t, ts, key, "intent-question", "please handle this")
	answer := commitAndHandoff(t, ts, key, "intent-answer", "yes, proceed")
	hash := sha256.Sum256([]byte("yes, proceed"))
	code, replayed, raw := handoffCommand(t, ts.Client(), ts.URL, key, "intent-answer-replay", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "intent-answer", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
	if code != 200 || replayed.ResourceID != first.ResourceID {
		t.Fatalf("replayed confirmation: status=%d %s", code, raw)
	}
	if first.ResourceID != answer.ResourceID {
		t.Fatalf("confirmation created a new task: %s != %s", first.ResourceID, answer.ResourceID)
	}
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, first.ResourceID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("confirmation did not resume task: %s", status)
	}
	var tasks, entries int
	_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
	_ = store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE content='yes, proceed'`).Scan(&entries)
	if tasks != 1 || entries != 1 {
		t.Fatalf("tasks=%d confirmation entries=%d", tasks, entries)
	}
	var runID string
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
		req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var claim contracts.JobClaim
		err = json.NewDecoder(resp.Body).Decode(&claim)
		resp.Body.Close()
		if resp.StatusCode != 200 || err != nil {
			t.Fatalf("claim status=%d err=%v", resp.StatusCode, err)
		}
		if i == 0 {
			runID = claim.RunID
		}
		if claim.TaskID != first.ResourceID || claim.RunID != runID {
			t.Fatalf("duplicate dispatch: %+v", claim)
		}
	}
	var runs int
	_ = store.DB().QueryRow(`SELECT count(*) FROM runs WHERE task_id=?`, first.ResourceID).Scan(&runs)
	if runs != 1 {
		t.Fatalf("dispatched %d runs", runs)
	}
}
