package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

const transcriptAt = "2026-11-06T12:34:56.123456789+02:00"

const ingestTestToken = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCA"

func transcriptHTTPServer(t *testing.T) (*httptest.Server, *storage.Store, string) {
	t.Helper()
	ts, store, _ := handoffHTTPServerWithConfig(t, Config{IngestEnabled: true})
	if err := control.RegisterRunnerCredentialIfMissing(context.Background(), store, ingestTestToken, "voice-gateway", []string{"voice:turn:ingest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id=id,gateway_media_generation=1,lifecycle_runner_id='voice-gateway'`); err != nil {
		t.Fatal(err)
	}
	return ts, store, ingestTestToken
}

func transcriptTurn(seq int64, role, text string) TranscriptTurn {
	return TranscriptTurn{Seq: seq, Role: role, Text: text, At: transcriptAt}
}

func postTranscript(t *testing.T, ts *httptest.Server, key, session string, turns ...TranscriptTurn) (int, map[string]int) {
	t.Helper()
	status, result, _ := postTranscriptGeneration(t, ts, key, session, 1, turns...)
	return status, result
}

func postTranscriptGeneration(t *testing.T, ts *httptest.Server, key, session string, mediaGeneration int64, turns ...TranscriptTurn) (int, map[string]int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"gateway_session_id": session, "media_generation": mediaGeneration, "turns": turns})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/voice/turns", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]int
	if resp.StatusCode == http.StatusOK && json.Unmarshal(raw, &result) != nil {
		t.Fatalf("invalid response: %s", raw)
	}
	var failure contracts.ErrorEnvelope
	if resp.StatusCode >= 400 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && json.Unmarshal(raw, &failure) != nil {
		t.Fatalf("invalid error envelope: %s", raw)
	}
	return resp.StatusCode, result, failure.Error.Code
}

func TestDelegationDurableOnTranscriptDuplicateAndTurnReplay(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET generation=7 WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	text := "  original 世界\n"
	turn := markedTranscript(8, strings.Repeat("😀", 256), text)
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(8, "user", text)); status != 200 {
		t.Fatal(status)
	}
	for range 2 {
		if status, result := postTranscript(t, ts, key, "handoff-session", turn); status != 200 || result["accepted"] != 0 || result["duplicates"] != 1 {
			t.Fatalf("status=%d counts=%v", status, result)
		}
	}
	var storedID, content, keyID, confirmed string
	var gen, rev, seq int64
	var hash []byte
	if err := store.DB().QueryRow(`SELECT turn_id,content,command_id,confirmed_at,generation,revision,transcript_seq,content_hash FROM voice_turns`).Scan(&storedID, &content, &keyID, &confirmed, &gen, &rev, &seq, &hash); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(text))
	if storedID != strings.Repeat("😀", 256) || content != text || keyID != delegationKey("handoff-session", 7, storedID, 1) || confirmed == "" || gen != 7 || rev != 1 || seq != 8 || !bytes.Equal(hash, want[:]) {
		t.Fatalf("incorrect bound delegation: %q %d %d %d", content, gen, rev, seq)
	}
	if n := writeJobCount(t, store, `SELECT count(*) FROM voice_turns`); n != 1 {
		t.Fatal(n)
	}
}

func TestDelegationBatchConflictsAndStorageFailureRollBackBothTables(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if status, _ := postTranscript(t, ts, key, "handoff-session", markedTranscript(0, "existing", "first")); status != 200 {
		t.Fatal(status)
	}
	cases := []struct {
		turn   TranscriptTurn
		status int
		code   string
	}{
		{markedTranscript(0, "other", "different"), 409, "transcript_conflict"},
		{markedTranscript(2, "existing", "different"), 409, "turn_revision_conflict"},
		{markedTranscript(2, "reject", "force failure"), 500, "internal"},
	}
	if _, err := store.DB().Exec(`CREATE TRIGGER reject_delegation BEFORE INSERT ON voice_turns WHEN NEW.turn_id='reject' BEGIN SELECT RAISE(ABORT,'test storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if status, _, code := postTranscriptGeneration(t, ts, key, "handoff-session", 1, markedTranscript(1, "new", "must roll back"), tc.turn); status != tc.status || code != tc.code {
			t.Fatalf("status=%d code=%q want=%d %q", status, code, tc.status, tc.code)
		}
		if n := writeJobCount(t, store, `SELECT (SELECT count(*) FROM voice_turns)+(SELECT count(*) FROM voice_transcript_turns)`); n != 2 {
			t.Fatalf("partial commit: %d rows", n)
		}
	}
}

func TestDelegationIngestValidation(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	for _, field := range []string{`null`, `42`, `""`, `"` + strings.Repeat("😀", 257) + `"`} {
		turn := transcriptTurn(1, "user", "valid text")
		turn.DelegationID = json.RawMessage(field)
		if status, _, code := postTranscriptGeneration(t, ts, key, "handoff-session", 1, turn); status != 400 || code != "invalid_request" {
			t.Fatalf("status=%d code=%s", status, code)
		}
	}
	assistant := markedTranscript(2, "invalid", "assistant text")
	assistant.Role = "assistant"
	for _, turn := range []TranscriptTurn{assistant, markedTranscript(3, "invalid", "\n\t ")} {
		if status, _ := postTranscript(t, ts, key, "handoff-session", turn); status != 400 {
			t.Fatal(status)
		}
	}
	if n := writeJobCount(t, store, `SELECT count(*) FROM voice_transcript_turns`); n != 0 {
		t.Fatal(n)
	}
}

func TestLateDelegationUsesOriginalBindingAndRejectsDifferentRunner(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	now := time.Now().UTC()
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended',ended_at=? WHERE id='handoff-session'; INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at,gateway_session_id,gateway_media_generation,lifecycle_runner_id) VALUES('replacement','default','owner','instance','next',X'01',7,'active',?,?, 'handoff-session',2,'voice-gateway')`, now.Add(-9*time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, key, "handoff-session", markedTranscript(1, "late", "original call")); status != 200 {
		t.Fatal(status)
	}
	if n := writeJobCount(t, store, `SELECT count(*) FROM voice_turns WHERE session_id='handoff-session' AND generation=1 AND turn_id='late'`); n != 1 {
		t.Fatal(n)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET ended_at=? WHERE id='handoff-session'`, now.Add(-11*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if status, _, code := postTranscriptGeneration(t, ts, key, "handoff-session", 1, markedTranscript(2, "too late", "never execute")); status != 409 || code != "transcript_window_closed" {
		t.Fatalf("status=%d code=%s", status, code)
	}
	const other = "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDA"
	if err := control.RegisterRunnerCredentialIfMissing(context.Background(), store, other, "different-gateway", []string{"voice:turn:ingest"}); err != nil {
		t.Fatal(err)
	}
	if status, _, code := postTranscriptGeneration(t, ts, other, "handoff-session", 2, markedTranscript(2, "foreign", "never execute")); status != 403 || code != "forbidden" {
		t.Fatalf("status=%d code=%s", status, code)
	}
	if n := writeJobCount(t, store, `SELECT count(*) FROM voice_turns`); n != 1 {
		t.Fatal(n)
	}
}

func TestTranscriptEndpointStoresBothRolesVerbatimInSeqOrder(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	want := []TranscriptTurn{transcriptTurn(1, "user", "  hello\n世界\t\x00  "), transcriptTurn(2, "assistant", "\n  answer 😀\r\n")}
	status, result := postTranscript(t, ts, key, "handoff-session", want[1], want[0])
	if status != 200 || result["accepted"] != 2 || result["duplicates"] != 0 {
		t.Fatalf("store status=%d result=%v", status, result)
	}
	got, err := ReadTranscriptTurns(context.Background(), store.DB(), "default", "handoff-session", 0, math.MaxInt64)
	if err != nil || len(got) != 2 {
		t.Fatalf("read=%v err=%v", got, err)
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || got[i].Role != want[i].Role || got[i].Text != want[i].Text || got[i].At != "2026-11-06T10:34:56.123456789Z" || got[i].ReceivedAt == "" {
			t.Fatalf("turn[%d]=%+v want=%+v", i, got[i], want[i])
		}
	}
}

func TestTranscriptEndpointIsIdempotentForIdenticalRetry(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	turn := transcriptTurn(1, "user", "original")
	status, result := postTranscript(t, ts, key, "handoff-session", turn)
	if status != 200 || result["accepted"] != 1 {
		t.Fatalf("first status=%d result=%v", status, result)
	}
	var before string
	if err := store.DB().QueryRow(`SELECT received_at FROM voice_transcript_turns`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	status, result = postTranscript(t, ts, key, "handoff-session", turn, transcriptTurn(2, "assistant", "reply"))
	if status != 200 || result["accepted"] != 1 || result["duplicates"] != 1 {
		t.Fatalf("retry status=%d result=%v", status, result)
	}
	var after string
	if err := store.DB().QueryRow(`SELECT received_at FROM voice_transcript_turns WHERE seq=1`).Scan(&after); err != nil || after != before {
		t.Fatalf("retry rewrote received_at: %q -> %q err=%v", before, after, err)
	}
}

func TestTranscriptEndpointConflictingRetryReturns409AndAppliesNothing(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "original")); status != 200 {
		t.Fatalf("seed status=%d", status)
	}
	for _, turns := range [][]TranscriptTurn{
		{transcriptTurn(2, "assistant", "must roll back"), transcriptTurn(1, "user", "changed")},
		{transcriptTurn(3, "assistant", "first"), transcriptTurn(3, "assistant", "conflicting within batch")},
	} {
		if status, _ := postTranscript(t, ts, key, "handoff-session", turns...); status != 409 {
			t.Fatalf("conflict status=%d", status)
		}
		var count int
		if err := store.DB().QueryRow(`SELECT count(*) FROM voice_transcript_turns`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("partial batch applied count=%d err=%v", count, err)
		}
	}
}

func TestTranscriptEndpointAcceptsOutOfOrderBatches(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	for _, seq := range []int64{9, 3, 7, 0} {
		if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(seq, "user", "text")); status != 200 {
			t.Fatalf("seq=%d status=%d", seq, status)
		}
	}
	rows, err := store.DB().Query(`SELECT seq FROM voice_transcript_turns ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, want := range []int64{0, 3, 7, 9} {
		var got int64
		if !rows.Next() || rows.Scan(&got) != nil || got != want {
			t.Fatalf("seq=%d want=%d", got, want)
		}
	}
}

func TestTranscriptEndpointRejectsUnknownSessionOtherWorkspaceAndBadRole(t *testing.T) {
	for _, tc := range []struct {
		name, session, role string
		status              int
	}{
		{"unknown", "missing", "user", 404}, {"other workspace", "other", "user", 403}, {"bad role", "handoff-session", "system", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store, key := transcriptHTTPServer(t)
			if _, err := store.DB().Exec(`INSERT INTO workspaces(id,name,created_at) VALUES('other','other',?); INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('other','other','owner','instance','other',X'01',1,'ended',?,?)`, transcriptAt, transcriptAt, transcriptAt); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='other',gateway_media_generation=1 WHERE id='other'`); err != nil {
				t.Fatal(err)
			}
			if status, _ := postTranscript(t, ts, key, tc.session, transcriptTurn(1, tc.role, "text")); status != tc.status {
				t.Fatalf("status=%d want=%d", status, tc.status)
			}
			var n int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_transcript_turns`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rejected request stored %d turns err=%v", n, err)
			}
		})
	}
}

func TestTranscriptEndpointRejectsOversizeTurnAndBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		turns []TranscriptTurn
	}{
		{"turn", []TranscriptTurn{transcriptTurn(1, "user", strings.Repeat("x", (64<<10)+1))}},
		{"batch", func() []TranscriptTurn {
			var turns []TranscriptTurn
			for i := 0; i < 9; i++ {
				turns = append(turns, transcriptTurn(int64(i), "assistant", strings.Repeat("x", 64<<10)))
			}
			return turns
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store, key := transcriptHTTPServer(t)
			if status, _ := postTranscript(t, ts, key, "handoff-session", tc.turns...); status != 413 {
				t.Fatalf("oversize status=%d", status)
			}
			var n int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_transcript_turns`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("count=%d err=%v", n, err)
			}
		})
	}
}

func TestTranscriptEndpointRequiresAuthentication(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	for _, credential := range []string{"", "invalid", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"} {
		if status, _ := postTranscript(t, ts, credential, "handoff-session", transcriptTurn(1, "user", "private")); status != 401 {
			t.Fatalf("unauthenticated status=%d", status)
		}
	}
	// Prove the authenticated production route exists, not just global middleware.
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "private")); status != 200 {
		t.Fatalf("authenticated status=%d", status)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM voice_transcript_turns`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestIngestEndpointRequiresVoiceIngestScope(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	other := "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD A"
	other = strings.ReplaceAll(other, " ", "")
	if err := control.RegisterRunnerCredentialIfMissing(context.Background(), store, other, "execution", []string{"runner:claim"}); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, other, "handoff-session", transcriptTurn(1, "user", "no")); status != 403 {
		t.Fatalf("missing scope status=%d", status)
	}
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "yes")); status != 200 {
		t.Fatalf("ingest status=%d", status)
	}
	if _, err := store.DB().Exec(`UPDATE runner_credentials SET revoked_at=? WHERE runner_id='voice-gateway'`, transcriptAt); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(2, "user", "revoked")); status != 401 {
		t.Fatalf("revoked status=%d", status)
	}
}

func TestIngestTokenCannotCallOtherEndpoints(t *testing.T) {
	ts, _, key := transcriptHTTPServer(t)
	for _, path := range []string{"/v1/snapshot", "/v1/commands", "/runner/v1/claims/current", "/runner/v1/claims"} {
		method := http.MethodPost
		if path == "/v1/snapshot" || path == "/runner/v1/claims/current" {
			method = http.MethodGet
		}
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 && resp.StatusCode != 403 {
			t.Fatalf("credential reached %s status=%d", path, resp.StatusCode)
		}
	}
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "yes")); status != 200 {
		t.Fatalf("ingest status=%d", status)
	}
}

func TestIngestTokenIsDisabledWhenNoTokenFileConfigured(t *testing.T) {
	ts, store, _ := registerDisabledHTTPServer(t, Config{})
	if err := control.RegisterRunnerCredentialIfMissing(context.Background(), store, ingestTestToken, "voice-gateway", []string{"voice:turn:ingest"}); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, ingestTestToken, "handoff-session", transcriptTurn(1, "user", "no")); status != 404 {
		t.Fatalf("disabled status=%d", status)
	}
	// The additive authority schema is independent of enabled route state.
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM voice_transcript_turns`).Scan(&n); err != nil {
		t.Fatal(err)
	}
}

func TestIngestResolvesLocalSessionFromGatewaySessionID(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='opaque-gateway',gateway_media_generation=1`); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, key, "opaque-gateway", transcriptTurn(1, "assistant", "resolved")); status != 200 {
		t.Fatalf("resolve status=%d", status)
	}
	var session, workspace string
	if err := store.DB().QueryRow(`SELECT session_id,workspace_id FROM voice_transcript_turns`).Scan(&session, &workspace); err != nil || session != "handoff-session" || workspace != "default" {
		t.Fatalf("session=%s workspace=%s err=%v", session, workspace, err)
	}
}

func TestIngestRejectsUnknownGatewaySessionAndStaleGeneration(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if status, _ := postTranscript(t, ts, key, "unknown", transcriptTurn(1, "user", "no")); status != 404 {
		t.Fatalf("unknown status=%d", status)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_media_generation=2`); err != nil {
		t.Fatal(err)
	}
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "no")); status != 409 {
		t.Fatalf("stale status=%d", status)
	}
}

func TestIngestAcceptsLateTurnsWithinTenMinutesAfterEndAndRejectsLater(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	for _, tc := range []struct {
		age    time.Duration
		status int
	}{{9 * time.Minute, 200}, {11 * time.Minute, 409}} {
		if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended',ended_at=?`, time.Now().Add(-tc.age).UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "assistant", "late")); status != tc.status {
			t.Fatalf("age=%v status=%d want=%d", tc.age, status, tc.status)
		}
	}
}

func TestTranscriptRangeReadReturnsOnlyRequestedSeqs(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "a"), transcriptTurn(3, "assistant", "b"), transcriptTurn(5, "user", "c")); status != 200 {
		t.Fatalf("status=%d", status)
	}
	got, err := ReadTranscriptTurns(context.Background(), store.DB(), "default", "handoff-session", 2, 5)
	if err != nil || len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 5 {
		t.Fatalf("range=%v err=%v", got, err)
	}
	for _, ids := range [][2]string{{"other", "handoff-session"}, {"default", "missing"}} {
		got, err = ReadTranscriptTurns(context.Background(), store.DB(), ids[0], ids[1], 0, 10)
		if err != nil || len(got) != 0 {
			t.Fatalf("scope leak=%v err=%v", got, err)
		}
	}
}

func TestTranscriptTableIsIncludedInDestruction(t *testing.T) {
	ts, store, key := transcriptHTTPServer(t)
	if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(1, "user", "retained forever")); status != 200 {
		t.Fatalf("status=%d", status)
	}
	path := filepath.Join(t.TempDir(), "source.sqlite")
	if _, err := store.DB().Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual generic SQLite backup and root-based destroy flows,
	// with the existing host-operation mock rig (no real services are changed).
	code := `import pathlib, sqlite3, sys, tempfile
tempfile.tempdir=str(pathlib.Path(tempfile.gettempdir()).resolve())
from tools.data_lifecycle.common import sqlite_snapshot
from tools.test_instance_lifecycle_cli import LifecycleCLI
fixture=LifecycleCLI()
fixture.setUp()
try:
 manifest=fixture.manifest()
 target=pathlib.Path(manifest['database'])
 target.unlink()
 sqlite_snapshot(pathlib.Path(sys.argv[1]),target)
 target.chmod(0o600)
 with sqlite3.connect(target) as db:
  for table in ('voice_transcript_turns',):
   assert db.execute('SELECT count(*) FROM '+table).fetchone()[0]==1, table+' missing from backup'
  db.execute("UPDATE zoe_instance_state SET instance_id='instance-fixture'")
 runner,calls=fixture.executor(manifest)
 from tools import zoe_destroy
 from unittest import mock
 # Host lock/mount operations are mocked, as are systemd and external cleanup.
 # Darwin flock conflicts with SQLite reads; this test checks table coverage,
 # not Linux locking or real destruction acceptance.
 with mock.patch.object(zoe_destroy,'mounted_paths',return_value=set()), mock.patch.object(zoe_destroy.fcntl,'flock'):
  result=zoe_destroy.destroy(manifest,fixture.root/'receipt',True,manifest['instance_id'],runner)
 assert result['executed'] and not target.exists(), 'database survived destruction'
finally:
 fixture.doCleanups()
`
	cmd := exec.Command("python3", "-c", code, path)
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backup/destruction: %v\n%s", err, output)
	}
}
