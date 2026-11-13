package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/runner"
)

type claimTestTransport func(*http.Request) (*http.Response, error)

func (f claimTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLogClaimFailure(t *testing.T) {
	previousWriter, previousLevel := log.Writer(), logging.CurrentLevel()
	defer log.SetOutput(previousWriter)
	defer logging.SetLevel(previousLevel)
	logging.SetLevel(logging.Debug)
	for _, tc := range []struct {
		name, kind string
		err        error
		status     int
		body       string
		cancel     bool
	}{
		{name: "refused", kind: "connection_refused", err: syscall.ECONNREFUSED},
		{name: "timeout", kind: "timeout", err: context.DeadlineExceeded},
		{name: "eof", kind: "eof", err: io.EOF},
		{name: "unexpected_eof", kind: "unexpected_eof", err: io.ErrUnexpectedEOF},
		{name: "reset", kind: "connection_reset", err: syscall.ECONNRESET},
		{name: "pipe", kind: "broken_pipe", err: syscall.EPIPE},
		{name: "http_408", kind: "http_408", status: 408},
		{name: "http_429", kind: "http_429", status: 429},
		{name: "http_502", kind: "http_502", status: 502},
		{name: "http_503", kind: "http_503", status: 503},
		{name: "http_504", kind: "http_504", status: 504},
		{name: "unauthorized", status: 401},
		{name: "forbidden", status: 403},
		{name: "other_http", status: 500},
		{name: "malformed", status: 200, body: "not JSON secret-response-body"},
		{name: "empty_response", status: 200},
		{name: "truncated_response", status: 200, body: `{"run_id":`},
		{name: "unknown", err: errors.New("secret-response-body https://example.invalid/secret-path")},
		{name: "shutdown_canceled", err: context.Canceled, cancel: true},
		{name: "shutdown_refused", kind: "connection_refused", err: syscall.ECONNREFUSED, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := runner.NewControlClient("https://example.invalid/secret-path", "secret-runner-token", &http.Client{
				Transport: claimTestTransport(func(r *http.Request) (*http.Response, error) {
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Secret": {"secret-header"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Claim(context.Background())
			if err == nil {
				t.Fatal("expected Claim failure")
			}
			if got := runner.TransientErrorKind(err); got != tc.kind {
				t.Fatalf("kind=%q want=%q", got, tc.kind)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			var logs bytes.Buffer
			log.SetOutput(&logs)
			logClaimFailure(ctx, err)
			if tc.cancel {
				if logs.Len() != 0 {
					t.Fatal("shutdown logged a Claim failure")
				}
				return
			}
			level, kind := "error", "unclassified"
			if tc.kind != "" {
				level, kind = "warn", tc.kind
			}
			want := "level=" + level + " runner iteration failed; retrying after idle interval: error_kind=" + kind
			if !strings.Contains(logs.String(), want) || strings.Count(logs.String(), "\n") != 1 {
				t.Fatalf("unexpected log: %q", logs.String())
			}
			for _, secret := range []string{"https://", "example.invalid", "secret-path", "secret-response-body", "secret-runner-token", "secret-header"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("Claim failure log leaked sensitive data")
				}
			}
		})
	}
}
