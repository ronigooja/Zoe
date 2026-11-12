package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/runner"
)

func TestStartupWaitEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{"", 120 * time.Second, false},
		{"0", 0, false},
		{"600", 600 * time.Second, false},
		{" 12 ", 12 * time.Second, false},
		{"-1", 0, true},
		{"601", 0, true},
		{"1.5", 0, true},
		{"garbage", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("ZOE_RUNNER_STARTUP_WAIT_SECONDS", tc.value)
			got, err := startupWaitEnv()
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %v, %v; want %v bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestServeDoesNotClaimBeforeStartupReconcile(t *testing.T) {
	t.Setenv("ZOE_RUNNER_STARTUP_WAIT_SECONDS", "0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runner/v1/claims/current" {
			t.Errorf("work started before successful reconciliation: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	control, err := runner.NewControlClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = serve(context.Background(), &runner.Engine{Control: control}, nil, nil, true, time.Second)
	if err == nil || !strings.Contains(err.Error(), "reconcile current claims") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected immediate reconcile failure, got %v", err)
	}
}
