package triage

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTriageTimeoutLogConfiguredVersusInjected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer server.Close()
	client := clientFromTestEnv(t, server, map[string]string{"ZOE_TRIAGE_TIMEOUT_MS": "200"})
	client.SetHTTPClient(&http.Client{Timeout: 50 * time.Millisecond})
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	_, err := client.Classify(context.Background(), "private utterance")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not a timeout: %v", err)
	}
	for _, field := range []string{"triage_call ", "configured_ms=200", "effective_ms=50", "elapsed_ms=", "error_kind=deadline_exceeded"} {
		if !strings.Contains(logs.String(), field) {
			t.Errorf("missing %s in timeout record", field)
		}
	}
	if strings.Contains(logs.String(), "private") || strings.Contains(logs.String(), server.URL) {
		t.Fatal("sensitive diagnostics")
	}
}

func TestSanitizedClassifierErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		status           int
	}{
		{"status", "private response body", "http_status", http.StatusForbidden},
		{"decode", "private malformed response", "decode", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := clientFromTestEnv(t, server, nil)
			_, err := client.Classify(context.Background(), "private utterance")
			if ErrorKind(err) != tc.kind {
				t.Fatalf("kind=%s want=%s", ErrorKind(err), tc.kind)
			}
		})
	}
}
