package voice

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

func reopenUIFixture(t *testing.T, r *deliveryRig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restart.db")
	if _, err := r.store.DB().Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	controller := r.svc.uiController
	r.svc = newService(db, Config{Now: r.now, UIController: controller, UIControllerInstruction: testPrompts(t).UIController()})
}

func addUIRetryJob(t *testing.T, db *sql.DB, now time.Time, id string) {
	t.Helper()
	at := now.UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO ui_requests(id,owner_subject_id,source,request,created_at,updated_at) VALUES(?,'owner','register','request',?,?)`, id, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ui_jobs(id,owner_subject_id,kind,dedupe_key,ui_request_id,next_attempt_at,created_at,updated_at) VALUES(?,'owner','task_entry',?,?, ?,?,?)`, "job-"+id, id, id, at, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestUIRetryRestartAndInvalidOutput(t *testing.T) {
	for _, code := range []int{500, 401, 200} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := uiTargetRig(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(code)
				if code == 200 {
					fmt.Fprint(w, `{"output_text":"invalid"}`)
				}
			}))
			defer server.Close()
			client, err := modelclient.FromEnv("UI", func(k string) string {
				switch k {
				case "UI_BASE_URL":
					return server.URL
				case "UI_MODEL", "UI_API_KEY_FILE":
					return "fixture"
				}
				return ""
			}, func(string) ([]byte, error) { return []byte("fixture"), nil }, 1000, 1, 30000)
			if err != nil {
				t.Fatal(err)
			}
			r.svc.uiController = client
			old := uiRetryJitter
			uiRetryJitter = func() float64 { return 0 }
			defer func() { uiRetryJitter = old }()
			addUIRetryJob(t, r.store.DB(), r.now(), "first")
			r.svc.dispatchUIJobs(context.Background())
			reopenUIFixture(t, r)
			db := r.svc.db
			if code == 500 {
				r.svc.dispatchUIJobs(context.Background())
				if calls.Load() != 1 {
					t.Fatal("retry called before due after reopen")
				}
				r.advance(800 * time.Millisecond)
				r.svc.dispatchUIJobs(context.Background())
				var attempts int
				if err := db.QueryRow(`SELECT attempts FROM ui_jobs WHERE id='job-first'`).Scan(&attempts); err != nil || attempts != 2 || calls.Load() != 2 {
					t.Fatalf("attempts=%d calls=%d err=%v", attempts, calls.Load(), err)
				}
			} else if code == 401 {
				addUIRetryJob(t, db, r.now(), "second")
				r.svc.dispatchUIJobs(context.Background())
				var jobs, requests int
				if err := db.QueryRow(`SELECT count(*) FROM ui_jobs WHERE status='failed' AND last_error='model_error'`).Scan(&jobs); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM ui_requests WHERE status='failed'`).Scan(&requests); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 || jobs != 2 || requests != 2 {
					t.Fatalf("calls=%d jobs=%d requests=%d", calls.Load(), jobs, requests)
				}
			} else {
				var status string
				var attempts, records int
				if err := db.QueryRow(`SELECT status,attempts FROM ui_jobs`).Scan(&status, &attempts); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM model_retry_state WHERE consumer='ui_controller'`).Scan(&records); err != nil {
					t.Fatal(err)
				}
				if status != "failed" || attempts != 1 || records != 0 || calls.Load() != 1 {
					t.Fatalf("status=%s attempts=%d records=%d calls=%d", status, attempts, records, calls.Load())
				}
			}
		})
	}
}

func TestUIRetryPolicy(t *testing.T) {
	for _, status := range []int{500, 401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			client, err := modelclient.FromEnv("UI", func(k string) string {
				switch k {
				case "UI_BASE_URL":
					return server.URL
				case "UI_MODEL", "UI_API_KEY_FILE":
					return "fixture"
				}
				return ""
			}, func(string) ([]byte, error) { return []byte("fixture"), nil }, 1000, 1, 30000)
			if err != nil {
				t.Fatal(err)
			}
			r.svc.uiController = client
			old := uiRetryJitter
			uiRetryJitter = func() float64 { return 0 }
			defer func() { uiRetryJitter = old }()
			uiFlowRegister(t, r)
			if status == 500 {
				for attempt, delay := range []time.Duration{800 * time.Millisecond, 1600 * time.Millisecond} {
					var next string
					if err := r.store.DB().QueryRow(`SELECT next_attempt_at FROM ui_jobs`).Scan(&next); err != nil {
						t.Fatal(err)
					}
					when, err := time.Parse(time.RFC3339Nano, next)
					if err != nil || !when.Equal(r.now().Add(delay)) {
						t.Fatalf("attempt %d next=%s err=%v", attempt+1, next, err)
					}
					r.svc.dispatchUIJobs(context.Background())
					if calls.Load() != int32(attempt+1) {
						t.Fatal("called before due")
					}
					r.advance(delay)
					r.svc.dispatchUIJobs(context.Background())
				}
				if calls.Load() != 3 {
					t.Fatalf("calls=%d", calls.Load())
				}
			} else {
				store := backoff.Store{DB: r.store.DB()}
				due, err := store.Due(context.Background(), "ui_controller", "", r.now())
				if err != nil || due || calls.Load() != 1 {
					t.Fatalf("due=%v calls=%d err=%v", due, calls.Load(), err)
				}
			}
			var state string
			if err := r.store.DB().QueryRow(`SELECT status FROM ui_jobs`).Scan(&state); err != nil || state != "failed" {
				t.Fatalf("status=%s err=%v", state, err)
			}
		})
	}
}
