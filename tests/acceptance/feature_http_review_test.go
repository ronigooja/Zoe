package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func reviewFeatureServer(t *testing.T) (*storage.Store, http.Handler, string, string, string) {
	t.Helper()
	code, token := reviewToken(t), reviewToken(t)
	store, err := storage.Open(context.Background(), storage.Config{Path: filepath.Join(t.TempDir(), "features.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	runnerToken := reviewToken(t)
	cfg := reviewRuntimeConfig(code, runnerToken)
	cfg.InstanceScopes = append(control.DefaultOwnerScopes(), "instance:admin")
	srv, err := control.NewServer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterInstanceRoutes(srv); err != nil {
		t.Fatal(err)
	}
	if err = memoryapi.RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = voice.Register(srv, store, voice.Config{}); err != nil {
		t.Fatal(err)
	}
	if err = control.RegisterExportRoutes(srv); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterRunnerCredential(context.Background(), runnerToken, "review", []string{"default"}, cfg.RunnerScopes); err != nil {
		t.Fatal(err)
	}
	h := bindReviewHandler(srv.Handler(), store)
	registration := reviewInitializeInstance(t, h, code, token)
	return store, h, token, registration.Device.ID, runnerToken
}
func featureCommand(t *testing.T, h http.Handler, token, key, kind string, payload any) *contracts.CommandReceipt {
	t.Helper()
	raw, _ := json.Marshal(payload)
	w := reviewRequest(h, "POST", "/v1/commands", token, key, contracts.CommandRequest{Kind: kind, Payload: raw})
	if w.Code != 200 {
		t.Fatalf("%s=%d %s", kind, w.Code, w.Body.String())
	}
	var receipt contracts.CommandReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	return &receipt
}

func TestReviewVoiceAtomicTaskSourceOverHTTP(t *testing.T) {
	store, h, token, device, _ := reviewFeatureServer(t)
	var subject string
	if err := store.DB().QueryRow(`SELECT subject_id FROM actors WHERE id=?`, device).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('review-atomic','default',?,?,'atomic-request',X'01',1,'active',?,?)`, subject, device, now, now); err != nil {
		t.Fatal(err)
	}
	create := func(revision int, key string) int {
		featureCommand(t, h, token, fmt.Sprintf("commit-%d", revision), "voice.turn.commit", map[string]any{"session_id": "review-atomic", "generation": 1, "turn_id": "stable", "revision": revision, "content": fmt.Sprintf("confirmed transcript %d", revision), "user_confirmed": true})
		raw, _ := json.Marshal(map[string]any{"title": "forged title", "goal": "forged goal", "initial_message": "forged message", "source": "voice", "source_reference": map[string]any{"kind": "voice_turn", "voice_session_id": "review-atomic", "generation": 1, "turn_id": "stable", "revision": revision}})
		return reviewRequest(h, "POST", "/v1/commands", token, key, contracts.CommandRequest{Kind: "task.create", Payload: raw}).Code
	}
	if status := create(1, "create-first"); status != 200 {
		t.Fatalf("first create=%d", status)
	}
	counts := func() string {
		t.Helper()
		var tasks, work, claims int
		for _, q := range []struct {
			sql  string
			dest *int
		}{{`SELECT count(*) FROM tasks`, &tasks}, {`SELECT count(*) FROM command_work WHERE kind='task.create'`, &work}, {`SELECT count(*) FROM runs`, &claims}} {
			if err := store.DB().QueryRow(q.sql).Scan(q.dest); err != nil {
				t.Fatal(err)
			}
		}
		return fmt.Sprintf("%d/%d/%d", tasks, work, claims)
	}
	before := counts()
	if status := create(2, "create-revised"); status != 409 {
		t.Fatalf("revised create=%d", status)
	}
	if after := counts(); after != before {
		t.Fatalf("duplicate changed task/work/run counts: %s -> %s", before, after)
	}
	var goal, entry string
	if err := store.DB().QueryRow(`SELECT goal FROM tasks`).Scan(&goal); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT content FROM task_entries`).Scan(&entry); err != nil {
		t.Fatal(err)
	}
	if goal != "confirmed transcript 1" || entry != goal {
		t.Fatalf("canonical goal=%q entry=%q", goal, entry)
	}
	var raw []byte
	if err := store.DB().QueryRow(`SELECT payload FROM events WHERE event_type='task.created'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var event contracts.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Task contracts.Task `json:"task"`
	}
	if err := json.Unmarshal(event.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Task.Goal != goal || body.Task.Title != goal {
		t.Fatalf("canonical event task=%+v", body.Task)
	}
}
