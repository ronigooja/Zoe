package voice

import (
	"bytes"
	"crypto/sha256"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/steward"
)

func TestClassificationStewardTimeoutLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer upstream.Close()
	env := map[string]string{"ZOE_STEWARD_BASE_URL": upstream.URL, "ZOE_STEWARD_MODEL": "test", "ZOE_STEWARD_API_KEY_FILE": "/test/key", "ZOE_STEWARD_TIMEOUT_MS": "200"}
	client, err := steward.FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fixture-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	client.SetHTTPClient(&http.Client{Timeout: 50 * time.Millisecond})
	_, store, _ := handoffHTTPServerWithConfig(t, Config{})
	const content = "private utterance"
	hash := sha256.Sum256([]byte(content))
	_, err = store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('timeout-log','default','handoff-session',1,'timeout-turn',1,?,?,?,'timeout-commit')`, content, hash[:], time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(store.DB(), Config{Classifier: &fakeClassifier{category: "task"}, Steward: client})
	defer svc.Stop()
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	svc.classifyCommittedTurn("timeout-log")
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "voice_classification ") {
			continue
		}
		found = true
		fields := map[string]string{}
		for _, field := range strings.Fields(line) {
			if k, v, ok := strings.Cut(field, "="); ok {
				fields[k] = v
			}
		}
		elapsed, parseErr := strconv.Atoi(fields["elapsed_ms"])
		if fields["configured_ms"] != "200" || fields["effective_ms"] != "50" || fields["error_kind"] != "deadline_exceeded" || fields["reason"] != "steward_timeout" || parseErr != nil || elapsed < 40 || elapsed > 200 || fields["elapsed_ms"] != fields["steward_ms"] {
			t.Errorf("incorrect classification diagnostics: %s", line)
		}
	}
	if !found {
		t.Fatal("missing classification log")
	}
	if strings.Contains(logs.String(), content) || strings.Contains(logs.String(), upstream.URL) || strings.Contains(logs.String(), "fixture-key") {
		t.Fatal("sensitive diagnostics")
	}
}
