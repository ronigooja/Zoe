package memory

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestIngestRequestsRetryPublicationOnStateChange(t *testing.T) {
	db, repo, now := ingestFixture(t)
	if _, err := db.Exec(`INSERT INTO voice_transcript_turns VALUES('session',1,'user','text',?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	fail := true
	client := evidenceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/memory/config" {
			fmt.Fprint(w, `{"config_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			return
		}
		if fail {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error_kind":"backend_unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	published := 0
	repo.OnRetryChange = func() { published++ }
	ctx := context.Background()
	if err := repo.batchStepWithJitter(ctx, client, time.Hour, now, func() float64 { return 0.5 }); err == nil {
		t.Fatal("expected a transient failure")
	}
	if published != 1 {
		t.Fatalf("publication requests after recorded failure = %d, want 1", published)
	}
	// Not due yet: no model call and no state change, so no new publication request.
	if err := repo.batchStepWithJitter(ctx, client, time.Hour, now.Add(time.Second), func() float64 { return 0.5 }); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("publication requested without a state change: %d", published)
	}
}
