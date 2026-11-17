package voice

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestUIValidationRetries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       int
		validSecond bool
		want        string
		attempts    int
	}{
		{"default", 0, false, "failed", 1}, {"recover", 1, true, "done", 2}, {"exhausted", 1, false, "failed", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			r.svc.recall.UIControllerMaxAttempts = tc.extra
			var mu sync.Mutex
			output := "{}"
			r.svc.uiController = uiFlowModel(t, &output, &mu)
			uiFlowRegister(t, r)
			if tc.extra > 0 {
				if tc.validSecond {
					mu.Lock()
					output = uiFlowOutput(uiFlowCreate+","+uiFlowRoot, false)
					mu.Unlock()
				}
				r.exec(t, `UPDATE ui_jobs SET next_attempt_at='2000-01-01T00:00:00Z'`)
				r.svc.dispatchUIJobs(context.Background())
			}
			var status, code string
			var attempts int
			if err := r.store.DB().QueryRow(`SELECT status,attempts,last_error FROM ui_jobs`).Scan(&status, &attempts, &code); err != nil {
				t.Fatal(err)
			}
			if status != tc.want || attempts != tc.attempts {
				t.Fatalf("status=%s attempts=%d code=%s", status, attempts, code)
			}
			if status == "failed" && !strings.HasPrefix(code, "validation_failed:") {
				t.Fatal(code)
			}
		})
	}
}

func TestUIInfrastructureRetryLimit(t *testing.T) {
	for _, extra := range []int{0, 8} {
		for _, code := range []string{"model_error", "storage_error", "snapshot_changed"} {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			r.svc.recall.UIControllerMaxAttempts = extra
			r.seedCall(t, "call", "owner", 1)
			r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request) VALUES('turn','default','call',1,'turn',1,'utterance',X'01','2000-01-01T00:00:00Z','command','ui','request')`)
			r.svc.createRegisterUIJobs(context.Background())
			var j uiJob
			if err := r.store.DB().QueryRow(`SELECT id,ui_request_id,workspace_id,owner_subject_id,kind FROM ui_jobs`).Scan(&j.id, &j.request, &j.w, &j.o, &j.kind); err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 3; attempt++ {
				j.attempts = attempt
				r.exec(t, `UPDATE ui_jobs SET attempts=?`, attempt)
				r.svc.failUIJob(context.Background(), j, code, true)
				var status, last string
				if err := r.store.DB().QueryRow(`SELECT status,last_error FROM ui_jobs`).Scan(&status, &last); err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if attempt == 3 {
					want = "failed"
				}
				if status != want || last != code {
					t.Fatal(status, last)
				}
			}
		}
	}
}
