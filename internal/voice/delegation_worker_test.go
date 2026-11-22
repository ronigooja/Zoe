package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/storage"
)

func markedTranscript(seq int64, id, content string) TranscriptTurn {
	turn := transcriptTurn(seq, "user", content)
	turn.DelegationID, _ = json.Marshal(id)
	return turn
}

func TestDelegationWorkerOwnsClassificationAndAppliesAfterHangup(t *testing.T) {
	var routes, choices atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.Add(1)
		var body struct{ Input string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Count(body.Input, "unique original utterance") != 1 {
			t.Error("delegation appeared in both raw history and current utterance")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"route":"task","request":"Complete the retained request."}`})
	}))
	defer model.Close()
	modelCfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
	cfg := Config{IngestEnabled: true, RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: modelCfg, CompactModel: modelCfg, CompactCharacters: 1000}}
	cfg.Steward = classificationStewardFunc(func(ctx context.Context, _ steward.Input) (steward.Choice, error) {
		if choices.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return steward.Choice{Action: steward.ActionNew}, nil
		case <-ctx.Done():
			return steward.Choice{}, ctx.Err()
		}
	})
	ts, store, key := registerDisabledHTTPServer(t, cfg)
	bindDelegationTestSession(t, store)
	for _, kind := range []string{"voice.turn.commit", "voice.turn.handoff"} {
		if status, _, _ := handoffCommand(t, ts.Client(), ts.URL, key, kind, kind, map[string]string{}); status == 200 {
			t.Fatalf("old command %s is still available", kind)
		}
	}
	turn := markedTranscript(1, "worker-only", "unique original utterance")
	for range 2 {
		if status, _ := postTranscript(t, ts, ingestTestToken, "handoff-session", turn); status != 200 {
			t.Fatalf("ingest waited for classification or failed: %d", status)
		}
	}
	waitClassification(t, started)
	status, _, body := handoffCommand(t, ts.Client(), ts.URL, key, "bypass", "task.create", map[string]any{
		"source": "voice", "title": "Invalid shortcut", "goal": "Invalid shortcut", "source_reference": map[string]any{
			"kind": "voice_turn", "voice_session_id": "handoff-session", "generation": 1, "turn_id": "worker-only", "revision": 1,
		},
	})
	if status != 403 || !strings.Contains(body, "invalid_source") {
		t.Fatalf("client task.create bypass status=%d: %s", status, body)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended',ended_at=? WHERE id='handoff-session'`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	close(release)
	result := waitDelegation(t, store, "worker-only")
	var owner, device, keyID, goal string
	if err := store.DB().QueryRow(`SELECT owner_subject_id,created_by_device_id,command_id,goal FROM tasks WHERE id=?`, result.ResourceID).Scan(&owner, &device, &keyID, &goal); err != nil {
		t.Fatal(err)
	}
	if owner != "owner" || device != "instance" || keyID != delegationKey("handoff-session", 1, "worker-only", 1) || goal != "Complete the retained request." || routes.Load() != 1 || choices.Load() != 1 {
		t.Fatalf("attestation owner=%s device=%s key=%s goal=%s routes=%d choices=%d", owner, device, keyID, goal, routes.Load(), choices.Load())
	}
}

func TestDelegationApplyRevalidatesPersistedSnapshot(t *testing.T) {
	r := newClassificationRig(t, nil)
	r.seedTurn(t, "stale-persisted")
	r.exec(t, `UPDATE voice_turns SET transcript_seq=1 WHERE id='stale-persisted'`)
	r.svc.classifyTurnFixture("stale-persisted")
	r.assertResult(t, "stale-persisted", "task", "new")
	r.seedTask(t, "intervening-task", "user-1")
	if err := r.svc.applyDelegation(context.Background(), "stale-persisted"); !errors.Is(err, errStaleSelection) {
		t.Fatalf("stale application error=%v", err)
	}
	if n := writeJobCount(t, r.store, `SELECT count(*) FROM tasks`); n != 1 {
		t.Fatalf("stale decision admitted work: tasks=%d", n)
	}
	r.svc.steward = classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
		r.choices.Add(1)
		reply := "Which task?"
		return steward.Choice{Action: steward.ActionClarify, Reply: &reply}, nil
	})
	r.svc.processDelegation(context.Background(), "stale-persisted")
	r.assertResult(t, "stale-persisted", "task", "clarify")
	if n := writeJobCount(t, r.store, `SELECT count(*) FROM voice_fast_replies WHERE text='Which task?'`); n != 1 || r.routes.Load() != 1 || r.choices.Load() != 2 {
		t.Fatalf("refresh produced %d replies, register=%d steward=%d", n, r.routes.Load(), r.choices.Load())
	}
}

func TestOversizedDelegationIsHandledWithOneErrorLog(t *testing.T) {
	for _, action := range []string{"new", "append"} {
		t.Run(action, func(t *testing.T) {
			r := newClassificationRig(t, nil)
			const id = "oversized-turn"
			r.seedTurn(t, id)
			request := "private request " + strings.Repeat("界", 6662) // 20,002 bytes
			var target string
			var version int
			if action == "append" {
				target, version = "existing-task", 1
				r.seedTask(t, target, "user-1")
			}
			ctx := context.Background()
			snapshot, err := stewardSnapshot(ctx, r.store.DB(), "default", "user-1")
			if err != nil {
				t.Fatal(err)
			}
			r.exec(t, `UPDATE voice_turns SET transcript_seq=1,exits='task',request=?,steward_action=?,steward_snapshot=?,steward_task_id=?,steward_task_version=? WHERE id=?`, request, action, snapshot, target, version, id)
			r.svc.uiEventServer, err = control.NewServer(r.store, control.Config{})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			oldOutput, oldFlags, oldLevel := log.Writer(), log.Flags(), logging.CurrentLevel()
			log.SetOutput(&logs)
			log.SetFlags(0)
			logging.SetLevel(logging.Info)
			t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags); logging.SetLevel(oldLevel) })
			for range 2 {
				r.svc.processDelegations(ctx)
			}
			if got, want := logs.String(), "level=error voice_delegation_task_text_too_large id=oversized-turn request_bytes=20002 limit_bytes=20000\n"; got != want {
				t.Fatalf("log = %q, want %q", got, want)
			}
			if n := writeJobCount(t, r.store, `SELECT count(*) FROM voice_turns WHERE id='oversized-turn' AND handoff_handled=1`); n != 1 {
				t.Fatal("oversized turn was left pending")
			}
			var retained string
			if err := r.store.DB().QueryRow(`SELECT request FROM voice_turns WHERE id=?`, id).Scan(&retained); err != nil || retained != request {
				t.Fatalf("retained request changed: bytes=%d err=%v", len(retained), err)
			}
			if n := writeJobCount(t, r.store, `SELECT (SELECT count(*) FROM tasks WHERE id!='existing-task') + (SELECT count(*) FROM task_entries) + (SELECT count(*) FROM voice_fast_replies)`); n != 0 {
				t.Fatalf("oversized delegation wrote %d tasks, entries or replies", n)
			}
		})
	}
}

func TestDelegationRecallQueuesRetainedRequest(t *testing.T) {
	r := newClassificationRig(t, nil)
	r.seedTurn(t, "recall-turn")
	r.exec(t, `UPDATE voice_turns SET transcript_seq=1,exits='recall',request='Find the earlier travel decision.' WHERE id='recall-turn'`)
	server, err := control.NewServer(r.store, control.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.svc.uiEventServer = server
	for range 2 {
		r.svc.processDelegation(context.Background(), "recall-turn")
	}
	if n := writeJobCount(t, r.store, `SELECT count(*) FROM voice_recall_jobs j JOIN voice_fast_replies f ON f.id=j.holding_id WHERE j.owner_id='user-1' AND j.question='Find the earlier travel decision.' AND f.intent_kind='recall_holding'`); n != 1 || r.routes.Load() != 0 || r.choices.Load() != 0 {
		t.Fatalf("recall jobs=%d register=%d steward=%d", n, r.routes.Load(), r.choices.Load())
	}
}

func bindDelegationTestSession(t *testing.T, store *storage.Store) {
	t.Helper()
	if err := control.RegisterRunnerCredentialIfMissing(context.Background(), store, ingestTestToken, "voice-gateway", []string{"default"}, []string{"voice:turn:ingest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id=id,gateway_media_generation=1,lifecycle_runner_id='voice-gateway' WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
}

func delegateTurn(t *testing.T, ts *httptest.Server, store *storage.Store, id, content string) contracts.CommandResult {
	t.Helper()
	bindDelegationTestSession(t, store)
	var seq sql.NullInt64
	if err := store.DB().QueryRow(`SELECT transcript_seq FROM voice_turns WHERE session_id='handoff-session' AND turn_id=?`, id).Scan(&seq); errors.Is(err, sql.ErrNoRows) {
		if err := store.DB().QueryRow(`SELECT coalesce(max(seq),-1)+1 FROM voice_transcript_turns WHERE session_id='handoff-session'`).Scan(&seq); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, ingestTestToken, "handoff-session", markedTranscript(seq.Int64, id, content)); status != http.StatusOK {
		t.Fatalf("delegate %q: status=%d", id, status)
	}
	return waitDelegation(t, store, id)
}

func waitDelegation(t *testing.T, store *storage.Store, id string) contracts.CommandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var handled bool
		var task string
		err := store.DB().QueryRowContext(ctx, `SELECT v.handoff_handled,coalesce(l.task_id,i.task_id,'') FROM voice_turns v LEFT JOIN voice_task_links l ON l.session_id=v.session_id AND l.generation=v.generation AND l.turn_id=v.turn_id LEFT JOIN voice_task_inputs i ON i.session_id=v.session_id AND i.generation=v.generation AND i.turn_id=v.turn_id WHERE v.session_id='handoff-session' AND v.turn_id=?`, id).Scan(&handled, &task)
		if err != nil {
			t.Fatal(err)
		}
		if handled {
			if task != "" {
				return contracts.CommandResult{ResourceType: "task", ResourceID: task}
			}
			return contracts.CommandResult{ResourceType: "voice_turn", ResourceID: id}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("delegation %q did not finish", id)
	return contracts.CommandResult{}
}
