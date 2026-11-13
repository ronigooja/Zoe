package memoryapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
)

func TestClientProtocolAndLogging(t *testing.T) {
	oldLevel, oldOutput := logging.CurrentLevel(), log.Writer()
	t.Cleanup(func() { logging.SetLevel(oldLevel); log.SetOutput(oldOutput) })
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("PRIVATE_TOKEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, level := range []logging.Level{logging.Info, logging.Debug} {
		var logs bytes.Buffer
		log.SetOutput(&logs)
		logging.SetLevel(level)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer PRIVATE_TOKEN" {
				t.Error("missing auth")
			}
			if r.URL.Path == "/v1/memory/search" {
				_, _ = w.Write([]byte(`{"items":[{"id":"id","text":"PRIVATE_RESPONSE","score":0.8}]}`))
			} else {
				_, _ = w.Write([]byte(`{"operations":[]}`))
			}
		}))
		client, err := New(server.URL, path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		items, err := client.Search(context.Background(), "person", "PRIVATE_QUERY")
		if err != nil || len(items) != 1 || items[0].Text != "PRIVATE_RESPONSE" {
			t.Fatalf("items=%v err=%v", items, err)
		}
		if err := client.Write(context.Background(), "person", "PRIVATE_SENTENCE", "PRIVATE_RESTATEMENT"); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if strings.Contains(logs.String(), "PRIVATE_TOKEN") {
			t.Fatal("token leaked")
		}
		for _, value := range []string{"PRIVATE_QUERY", "PRIVATE_RESPONSE", "PRIVATE_SENTENCE", "PRIVATE_RESTATEMENT", "component=memory_search", "component=memory_write"} {
			if strings.Contains(logs.String(), value) != (level == logging.Debug) {
				t.Errorf("level=%v missing/unexpected %s", level, value)
			}
		}
	}
}

func TestClientFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"status", "timeout", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "timeout" {
					time.Sleep(50 * time.Millisecond)
					return
				}
				w.WriteHeader(503)
				if mode == "status" {
					_, _ = w.Write([]byte(`{"error_kind":"busy"}`))
				} else {
					_, _ = w.Write([]byte(`{"error_kind":"PRIVATE_CONTENT"}`))
				}
			}))
			defer server.Close()
			client, _ := New(server.URL, path, 20*time.Millisecond)
			_, err := client.Search(context.Background(), "person", "query")
			var failure *Error
			want := map[string]string{"status": "busy", "timeout": "deadline_exceeded", "unknown": "http_status"}[mode]
			if !errors.As(err, &failure) || failure.Kind != want {
				t.Fatalf("error=%v want=%s", err, want)
			}
		})
	}
	var client *Client
	if err := client.Write(context.Background(), "person", "sentence", "restatement"); err == nil {
		t.Fatal("disabled accepted")
	}
}
