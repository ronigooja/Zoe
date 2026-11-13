package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

func captureRegisterLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previousLevel := logging.CurrentLevel()
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { logging.SetLevel(previousLevel) })
	var out bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &out
}

func assertRegisterLogPrivacy(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"PRIVATE_SENTENCE", "PRIVATE_COMPACT", "PRIVATE_MODEL_OUTPUT", "PRIVATE_OWNER_ID", "PRIVATE_MEMORY_KEY", "PRIVATE_RESTATEMENT"} {
		if strings.Contains(text, secret) {
			t.Errorf("log contains private fixture marker %q", secret)
		}
	}
}

func TestRegisterRouteLogs(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		fail               bool
	}{
		{"success", `{"route":"task","restatement":"PRIVATE_RESTATEMENT"}`, "status=ok route=task elapsed_ms=[0-9]+ error_kind=none", false},
		{"model_error", "PRIVATE_MODEL_OUTPUT", "status=error route=none elapsed_ms=[0-9]+ error_kind=model_http", true},
		{"invalid_output", "PRIVATE_MODEL_OUTPUT", "status=error route=none elapsed_ms=[0-9]+ error_kind=output_invalid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegisterFixture(t)
			f.owner.OwnerSubjectID = "PRIVATE_OWNER_ID"
			f.session("private-session", "default", f.owner.OwnerSubjectID)
			f.turn("private-session", 0, "user", "PRIVATE_SENTENCE", f.now.Add(-2*time.Hour))
			f.output = "PRIVATE_COMPACT"
			f.rebuild(f.now)
			f.output, f.fail = tc.output, tc.fail
			logs := captureRegisterLogs(t)
			_, err := f.r.Route(context.Background(), f.owner, "PRIVATE_SENTENCE", f.now)
			if (err == nil) != (tc.name == "success") {
				t.Fatalf("unexpected route error: %v", err)
			}
			if !regexp.MustCompile(`register_call `+tc.want+`\n`).MatchString(logs.String()) || strings.Count(logs.String(), "register_call ") != 1 {
				t.Fatalf("unexpected call log: %s", logs.String())
			}
			assertRegisterLogPrivacy(t, logs.String())
		})
	}
}

func TestRegisterRebuildLogs(t *testing.T) {
	for _, mode := range []string{"success", "model_error", "invalid_output", "snapshot", "write"} {
		t.Run(mode, func(t *testing.T) {
			f := newRegisterFixture(t)
			f.owner.OwnerSubjectID = "PRIVATE_OWNER_ID"
			f.session("private-session", "default", f.owner.OwnerSubjectID)
			f.turn("private-session", 0, "user", "PRIVATE_SENTENCE", f.now.Add(-2*time.Hour))
			f.output = "PRIVATE_COMPACT"
			stage, kind, httpStatus := "", "", ""
			switch mode {
			case "model_error":
				f.fail = true
				stage, kind, httpStatus = "call_model", "model_http", " http_status=503"
			case "invalid_output":
				f.output = strings.Repeat("PRIVATE_MODEL_OUTPUT", 100)
				stage, kind = "invalid_output", "output_invalid"
			case "snapshot":
				f.exec(`UPDATE voice_transcript_turns SET at='invalid' WHERE session_id='private-session'`)
				stage, kind = "snapshot", "db"
			case "write":
				f.exec(`CREATE TRIGGER reject_compact BEFORE UPDATE OF compact ON voice_context_registers BEGIN SELECT RAISE(ABORT,'PRIVATE_MODEL_OUTPUT'); END`)
				stage, kind = "write", "db"
			}
			logs := captureRegisterLogs(t)
			err := f.r.RebuildCompact(context.Background(), f.now)
			built, failed := 1, 0
			if mode != "success" {
				built, failed = 0, 1
				if err == nil {
					t.Fatal("failure accepted")
				}
				hash := sha256.Sum256([]byte(f.owner.OwnerSubjectID))
				want := fmt.Sprintf("register status=rebuild_failed stage=%s error_kind=%s%s owner_hash=%x", stage, kind, httpStatus, hash[:4])
				if !strings.Contains(logs.String(), want+"\n") {
					t.Fatalf("missing failure metadata: %s", logs.String())
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf(`register status=rebuild owners_built=%d owners_failed=%d elapsed_ms=[0-9]+\n`, built, failed)
			if !regexp.MustCompile(want).MatchString(logs.String()) {
				t.Fatalf("missing rebuild summary: %s", logs.String())
			}
			assertRegisterLogPrivacy(t, logs.String())
			if mode == "success" {
				logs.Reset()
				if err := f.r.RebuildCompact(context.Background(), f.now); err != nil || logs.Len() != 0 {
					t.Fatalf("unchanged source logged or failed: %s %v", logs.String(), err)
				}
			}
		})
	}
}

func TestRegisterStartLogs(t *testing.T) {
	f := newRegisterFixture(t)
	logs := captureRegisterLogs(t)
	// A canceled context makes the worker deterministic without changing startup logging.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.r.Start(ctx)
	f.r.Start(ctx)
	f.r.Stop()
	if strings.Count(logs.String(), "register status=started interval_ms=3600000\n") != 1 {
		t.Fatalf("missing or duplicated startup log: %s", logs.String())
	}
	assertRegisterLogPrivacy(t, logs.String())
}

func TestRegisterDebugFullModelCalls(t *testing.T) {
	f := newRegisterFixture(t)
	f.owner.OwnerSubjectID = "PRIVATE_OWNER_ID"
	f.session("debug-session", "default", f.owner.OwnerSubjectID)
	f.turn("debug-session", 0, "user", "PRIVATE_TRANSCRIPT", f.now.Add(-2*time.Hour))
	f.output = "PRIVATE_COMPACT"
	logs := captureRegisterLogs(t)
	logging.SetLevel(logging.Debug)
	f.rebuild(f.now)
	f.output = `{"route":"task","restatement":"PRIVATE_RESTATEMENT"}`
	sentence := strings.Repeat("s", 20000)
	if _, err := f.r.Route(context.Background(), f.owner, sentence, f.now); err != nil {
		t.Fatal(err)
	}
	for i, input := range f.inputs {
		if !strings.Contains(logs.String(), "input="+strconv.Quote(input)) || !strings.Contains(logs.String(), "instructions="+strconv.Quote(f.instructions[i])) {
			t.Fatal("model input truncated or changed")
		}
	}
	for _, text := range []string{sentence, "output=" + strconv.Quote(f.output), "PRIVATE_COMPACT", "model_call_input", "model_call_output", "model_call_result", "component=register_compact", "component=register"} {
		if !strings.Contains(logs.String(), text) {
			t.Errorf("missing debug text (%d characters)", len(text))
		}
	}
	for _, forbidden := range []string{"PRIVATE_OWNER_ID", "Authorization", "Bearer", "https://unused.invalid", "api_key=", "http://127.0.0.1"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("forbidden debug metadata %q", forbidden)
		}
	}
}

func TestRegisterDebugSingleCall(t *testing.T) {
	f := newRegisterFixture(t)
	f.output = `{"route":"recall","restatement":"Retrieve the user's travel preference."}`
	logs := captureRegisterLogs(t)
	logging.SetLevel(logging.Debug)
	if _, err := f.r.Route(context.Background(), f.owner, "PRIVATE_SENTENCE", f.now); err != nil {
		t.Fatal(err)
	}
	for _, input := range f.inputs {
		if !strings.Contains(logs.String(), "input="+strconv.Quote(input)) {
			t.Fatal("input changed or truncated")
		}
	}
	for _, field := range []string{"attempt=1", "model_call_input", "model_call_output", "model_call_result", "register_call status=ok route=recall"} {
		if !strings.Contains(logs.String(), field) {
			t.Errorf("missing %s", field)
		}
	}
	if f.calls != 1 || strings.Count(logs.String(), "model_call_result ") != 1 {
		t.Fatal("expected exactly one model call and result")
	}
	if strings.Contains(logs.String(), "parent_call_id=") || strings.Contains(logs.String(), "attempt=2") {
		t.Fatal("single model call has followup metadata")
	}
}
