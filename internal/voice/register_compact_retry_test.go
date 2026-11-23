package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
)

func TestCompactRetryPersistenceAndAuth(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-3*time.Hour))
	if !f.rebuild(f.now) {
		t.Fatal("initial compact")
	}
	previous := f.layers(f.now).Compact
	f.turn("handoff-session", 1, "user", "new", f.now.Add(-2*time.Hour))
	status := 503
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": "replacement"})
	}))
	defer server.Close()
	model := RegisterModelConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"}
	register, err := NewRegister(f.db, RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: model, CompactModel: model, CompactCharacters: 1000})
	if err != nil {
		t.Fatal(err)
	}
	store := backoff.Store{DB: f.db}
	register.retry = &store
	register.retryJitter = func() float64 { return 0 }
	now := f.now
	for i := 1; i <= 9; i++ {
		_, err = register.RebuildCompactForOwner(context.Background(), f.owner, now)
		if err == nil {
			t.Fatalf("failure %d succeeded", i)
		}
		states, err := store.List(context.Background())
		if err != nil || len(states) != 1 {
			t.Fatalf("states=%v err=%v", states, err)
		}
		want := []time.Duration{24 * time.Second, 48 * time.Second, 96 * time.Second, 192 * time.Second, 384 * time.Second, 768 * time.Second, 1536 * time.Second, 2880 * time.Second, 2880 * time.Second}[i-1]
		if states[0].Failures != i || !states[0].NextAttemptAt.Equal(now.Add(want)) {
			t.Fatalf("failure %d state=%+v", i, states[0])
		}
		before := calls
		if ok, err := register.RebuildCompactForOwner(context.Background(), f.owner, now.Add(time.Second)); err != nil || ok || calls != before {
			t.Fatalf("early retry %v %v", ok, err)
		}
		if f.layers(f.now).Compact != previous {
			t.Fatal("previous compact changed")
		}
		if i == 1 {
			path := filepath.Join(t.TempDir(), "retry.db")
			if _, err := f.db.Exec(`VACUUM INTO ?`, path); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			register.db = db
			store.DB = db
			if ok, err := register.RebuildCompactForOwner(context.Background(), f.owner, now.Add(time.Second)); err != nil || ok || calls != before {
				t.Fatalf("reopened early retry %v %v", ok, err)
			}
		}
		now = states[0].NextAttemptAt
	}
	status = 401
	if _, err := register.RebuildCompactForOwner(context.Background(), f.owner, now); err == nil {
		t.Fatal("expected auth")
	}
	before := calls
	if ok, err := register.RebuildCompactForOwner(context.Background(), f.owner, now.Add(24*time.Hour)); err != nil || ok || calls != before {
		t.Fatal("auth not stopped")
	}
	if err := store.ClearAuth(context.Background(), "compact"); err != nil {
		t.Fatal(err)
	}
	status = 200
	if ok, err := register.RebuildCompactForOwner(context.Background(), f.owner, now); err != nil || !ok {
		t.Fatalf("recovery %v %v", ok, err)
	}
	states, err := store.List(context.Background())
	if err != nil || len(states) != 0 {
		t.Fatalf("not cleared %v %v", states, err)
	}
}

func TestCompactRetryUnreadable(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-3*time.Hour))
	badDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer badDB.Close()
	store := backoff.Store{DB: badDB}
	f.r.retry = &store
	if !f.rebuild(f.now) {
		t.Fatal("unreadable retry state blocked rebuild")
	}
}

func TestCompactRetryClearedOnlyAfterFencedWrite(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-3*time.Hour))
	if !f.rebuild(f.now) {
		t.Fatal("initial compact missing")
	}
	previous := f.layers(f.now).Compact
	f.turn("handoff-session", 1, "user", "new", f.now.Add(-2*time.Hour))
	store := backoff.Store{DB: f.db}
	f.r.retry = &store
	published := 0
	f.r.requestPublish = func() { published++ }
	scope := f.owner.OwnerSubjectID
	if err := store.Record(context.Background(), "compact", scope, backoff.Transient, f.now.Add(-time.Minute), backoff.Background, nil); err != nil {
		t.Fatal(err)
	}
	before, err := store.List(context.Background())
	if err != nil || len(before) != 1 {
		t.Fatalf("initial retry state=%v err=%v", before, err)
	}
	f.onCall = func() {
		f.exec(`UPDATE voice_context_registers SET version=version+1 WHERE owner_subject_id=?`, f.owner.OwnerSubjectID)
	}
	if f.rebuild(f.now) {
		t.Fatal("rejected fence reported success")
	}
	if f.layers(f.now).Compact != previous {
		t.Fatal("rejected fence changed compact")
	}
	after, err := store.List(context.Background())
	if err != nil || len(after) != 1 || after[0] != before[0] || published != 0 {
		t.Fatalf("retry state changed after fence: before=%v after=%v published=%d err=%v", before, after, published, err)
	}
	f.onCall = nil
	if !f.rebuild(f.now) {
		t.Fatal("successful rebuild failed")
	}
	after, err = store.List(context.Background())
	if err != nil || len(after) != 0 || published != 1 {
		t.Fatalf("retry state not cleared: after=%v published=%d err=%v", after, published, err)
	}
}
