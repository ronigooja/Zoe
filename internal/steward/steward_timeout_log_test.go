package steward

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/triage"
)

func TestStewardTimeoutLogBudgets(t *testing.T) {
	for _, tc := range []struct {
		name             string
		parent, injected time.Duration
		want             int64
	}{
		{"configured", time.Second, 0, 200},
		{"parent", 80 * time.Millisecond, 0, 80},
		{"http", time.Second, 50 * time.Millisecond, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(400 * time.Millisecond):
				}
			}))
			defer server.Close()
			client := configured(t, server.URL, "200")
			if tc.injected > 0 {
				client.SetHTTPClient(&http.Client{Timeout: tc.injected})
			}
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			_, err := client.Decide(ctx, "private utterance")
			if !errors.Is(err, context.DeadlineExceeded) || triage.ErrorKind(err) != "deadline_exceeded" {
				t.Fatalf("timeout identity lost: %v", err)
			}
			for _, prefix := range []string{"steward_call ", "triage_call "} {
				found := false
				for _, line := range strings.Split(logs.String(), "\n") {
					if !strings.Contains(line, prefix) {
						continue
					}
					found = true
					fields := map[string]string{}
					for _, field := range strings.Fields(line) {
						if k, v, ok := strings.Cut(field, "="); ok {
							fields[k] = v
						}
					}
					effective, e1 := strconv.ParseInt(fields["effective_ms"], 10, 64)
					elapsed, e2 := strconv.ParseInt(fields["elapsed_ms"], 10, 64)
					if fields["configured_ms"] != "200" || fields["error_kind"] != "deadline_exceeded" || e1 != nil || e2 != nil || effective > tc.want || effective < tc.want-10 || elapsed < tc.want-10 || elapsed > tc.want+150 {
						t.Errorf("incorrect timeout diagnostics: %s", line)
					}
				}
				if !found {
					t.Errorf("missing %s record", prefix)
				}
			}
			if strings.Contains(logs.String(), "private") || strings.Contains(logs.String(), server.URL) {
				t.Fatal("sensitive data in logs")
			}
		})
	}
}

func TestStewardQuestionAlwaysPlainChinese(t *testing.T) {
	for _, input := range []string{"handle this", "处理这个", ""} {
		if got := Question(input); got != "要我现在去处理这个请求吗？" {
			t.Fatalf("question=%q", got)
		}
	}
}
