package memory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

type batchErrorModel struct{ err error }

func (m batchErrorModel) Complete(context.Context, string, string) (string, error) {
	return "", m.err
}

func TestBatchDebugModelCalls(t *testing.T) {
	_, repo, _, now := batchFixture(t)
	buf := captureBatchLogs(t)
	logging.SetLevel(logging.Debug)
	output := `[{"subject":"user","key":"preference","content":"PRIVATE_BATCH_OUTPUT"}]`
	var sent string
	model := batchFakeModel(t, 200, output, func(instructions, input string) {
		sent = input
		if instructions != MemoryExtractionInstruction {
			t.Error("unexpected instructions")
		}
	})
	if ok, err := repo.RunBatch(context.Background(), "default", "owner", now, time.Hour, model); err != nil || !ok {
		t.Fatalf("batch: %v %v", ok, err)
	}
	for _, field := range []string{"instructions=" + strconv.Quote(MemoryExtractionInstruction), "input=" + strconv.Quote(sent), "output=" + strconv.Quote(output), "model_call_input", "model_call_output", "model_call_result", "component=memory_batch", "PRIVATE_BATCH_OUTPUT"} {
		if !strings.Contains(buf.String(), field) {
			t.Errorf("missing debug field (%d characters)", len(field))
		}
	}
	for _, forbidden := range []string{"Authorization", "Bearer", "http://127.0.0.1", "owner\""} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("forbidden debug metadata %q", forbidden)
		}
	}
}

func captureBatchLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previousLevel := logging.CurrentLevel()
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { logging.SetLevel(previousLevel) })
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

func TestBatchErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		stage, override, kind string
		err                   error
		status                int
	}{
		{"read_boundary", "config", "config", errors.New("private configuration"), 0},
		{"read_boundary", "", "db", sql.ErrConnDone, 0},
		{"load_input", "", "db", sql.ErrConnDone, 0},
		{"load_input", "", "input", &time.ParseError{Value: "private input"}, 0},
		{"load_input", "input", "input", errors.New("private input"), 0},
		{"call_model", "", "model_http", fmt.Errorf("wrapped: %w", &modelclient.HTTPError{StatusCode: 503}), 503},
		{"call_model", "", "model_transport", errors.New("private URL or key"), 0},
		{"call_model", "", "model_timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), 0},
		{"call_model", "", "output_invalid", errors.New("invalid model response"), 0},
		{"call_model", "", "config", errors.New("model client is not configured"), 0},
		{"parse_output", "", "output_invalid", errors.New("private output"), 0},
		{"write", "", "output_too_large", errBatchOutputTooLarge, 0},
		{"write", "", "write_conflict", fmt.Errorf("wrapped: %w", ErrVersionConflict), 0},
		{"write", "lease_lost", "lease_lost", ErrVersionConflict, 0},
		{"write", "", "db", sql.ErrConnDone, 0},
		{"commit", "", "db", sql.ErrConnDone, 0},
		{"unknown", "", "other", errors.New("private error"), 0},
	} {
		t.Run(tc.stage+"/"+tc.kind, func(t *testing.T) {
			kind, status := batchErrorKind(tc.err, tc.stage, tc.override)
			if kind != tc.kind || status != tc.status {
				t.Fatalf("got %s/%d, want %s/%d", kind, status, tc.kind, tc.status)
			}
		})
	}
}

// These integration assertions fail on the old code: RunBatch emitted neither
// failure metadata nor success logs (the scheduler only logged status=failed).
func TestBatchLogsRealFailuresAndSuccess(t *testing.T) {
	const secret = "sk-transcript-private-0123456789"
	const output = `[{"subject":"user","key":"private","content":"sk-model-output-private"}]`
	for _, tc := range []struct {
		name, kind, stage, sql, output string
		status                         int
		modelErr                       error
		loseLease                      bool
	}{
		{name: "config", kind: "config", stage: "read_boundary"},
		{name: "http", kind: "model_http", stage: "call_model", status: 503, output: output},
		{name: "transport", kind: "model_transport", stage: "call_model", modelErr: errors.New(secret)},
		{name: "timeout", kind: "model_timeout", stage: "call_model", modelErr: context.DeadlineExceeded},
		{name: "invalid", kind: "output_invalid", stage: "parse_output", status: 200, output: secret},
		{name: "too_large", kind: "output_too_large", stage: "write", status: 200, output: `[{"subject":"u","key":"k","content":"` + strings.Repeat("x", 20001) + `"}]`},
		{name: "write", kind: "db", stage: "write", status: 200, output: output, sql: `CREATE TRIGGER fail_batch_log BEFORE UPDATE OF boundary_ns ON memory_batch_boundaries BEGIN SELECT RAISE(ABORT,'private database content'); END`},
		{name: "conflict", kind: "write_conflict", stage: "write", status: 200, output: output, sql: `CREATE TRIGGER conflict_batch_log BEFORE UPDATE OF boundary_ns ON memory_batch_boundaries BEGIN SELECT RAISE(IGNORE); END`},
		{name: "lease", kind: "lease_lost", stage: "write", status: 200, output: output, loseLease: true},
		{name: "input", kind: "input", stage: "load_input", sql: `UPDATE voice_transcript_turns SET at='private-invalid-time' WHERE session_id='a' AND seq=1`},
		{name: "boundary", kind: "db", stage: "read_boundary", sql: `DROP TABLE memory_batch_boundaries`},
		{name: "success", status: 200, output: output},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repo, _, now := batchFixture(t)
			if _, err := db.Exec(`UPDATE voice_transcript_turns SET text=? WHERE session_id='a' AND seq=1`, secret); err != nil {
				t.Fatal(err)
			}
			if err := repo.StageWrite(context.Background(), "default", "owner", "log-test", secret, now); err != nil {
				t.Fatal(err)
			}
			if tc.sql != "" {
				if _, err := db.Exec(tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			chars := 0
			var model BatchModel = batchFakeModel(t, tc.status, tc.output, func(_, input string) {
				chars = utf8.RuneCountInString(input)
				if tc.loseLease {
					if _, err := db.Exec(`UPDATE memory_batch_boundaries SET lease_token='replacement' WHERE principal_id='owner'`); err != nil {
						t.Error(err)
					}
				}
			})
			if tc.modelErr != nil {
				model = batchErrorModel{tc.modelErr}
			}
			buf := captureBatchLogs(t)
			interval := time.Hour
			if tc.name == "config" {
				interval = 0
			}
			ok, err := repo.RunBatch(context.Background(), "default", "owner", now, interval, model)
			if tc.kind == "" {
				if err != nil || !ok {
					t.Fatalf("success=%v err=%v", ok, err)
				}
			} else if err == nil || ok {
				t.Fatalf("expected failure, ok=%v err=%v", ok, err)
			}
			line := buf.String()
			want := []string{"owner_hash=" + hashString("owner")[:8], "elapsed_ms="}
			if tc.kind == "" {
				want = append(want, "status=ok", "claims=1 staged=1 boundary_advanced=true")
			} else {
				want = append(want, "status=failed stage="+tc.stage+" error_kind="+tc.kind)
			}
			if tc.stage != "read_boundary" && tc.stage != "load_input" {
				want = append(want, "input_turns=3 input_tasks=1 input_staged=1", "input_chars=")
			}
			if chars > 0 {
				want = append(want, fmt.Sprintf("input_chars=%d ", chars))
			}
			if tc.kind == "model_http" {
				want = append(want, "http_status=503")
			} else if strings.Contains(line, "http_status=") {
				t.Errorf("unexpected HTTP status: %s", line)
			}
			for _, field := range want {
				if !strings.Contains(line, field) {
					t.Errorf("missing %q: %s", field, line)
				}
			}
			for _, private := range []string{secret, "sk-model-output-private", "private database content", "original request done", "full durable result", "owner ", "owner\n"} {
				if strings.Contains(line, private) {
					t.Error("private data in logs")
				}
			}
			if strings.Count(line, "memory_batch ") != 1 {
				t.Errorf("expected exactly one batch log: %s", line)
			}
		})
	}
}

func TestBatchSchedulerStartAndEmptyRunLogs(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	if _, err := db.Exec(`DELETE FROM voice_transcript_turns; DELETE FROM tasks`); err != nil {
		t.Fatal(err)
	}
	buf := captureBatchLogs(t)
	model := batchFakeModel(t, 200, `[]`, func(string, string) { t.Error("empty run called model") })
	if ok, err := repo.RunBatch(context.Background(), "default", "owner", now, time.Hour, model); err != nil || ok {
		t.Fatalf("empty run: %v %v", ok, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty run logged: %s", buf.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo.runBatches(ctx, model, 24*time.Hour)
	if strings.Count(buf.String(), "memory_batch status=scheduler_started interval_hours=24") != 1 {
		t.Fatalf("scheduler start log: %s", buf.String())
	}
}

func TestBatchOwnerDiscoveryFailureLog(t *testing.T) {
	db, repo, _, now := batchFixture(t)
	if _, err := db.Exec(`DROP TABLE voice_sessions`); err != nil {
		t.Fatal(err)
	}
	buf := captureBatchLogs(t)
	if err := repo.batchStep(context.Background(), nil, time.Hour, now); err == nil {
		t.Fatal("expected owner discovery failure")
	}
	if !strings.Contains(buf.String(), "status=failed stage=load_input error_kind=db") || !strings.Contains(buf.String(), "input_turns=0 input_tasks=0 input_staged=0 input_chars=0") {
		t.Fatalf("missing discovery diagnostic: %s", buf.String())
	}
}
