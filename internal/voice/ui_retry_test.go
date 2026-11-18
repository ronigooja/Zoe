package voice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

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
