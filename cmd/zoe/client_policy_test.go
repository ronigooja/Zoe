package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestClientPolicySetWarn(t *testing.T) {
	var out, errOut bytes.Buffer
	args := []string{"--offline", "--database", ":memory:", "--level", "warn"}
	if err := clientPolicySetOperation(args, nil, &out, &errOut); err != nil {
		t.Fatalf("set warn: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "Log level: warn") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestClientPolicyRejectsInvalidLevel(t *testing.T) {
	for _, level := range []string{"error", "warn", "info", "debug"} {
		if !validPolicyLevel(level) {
			t.Fatalf("valid level rejected: %s", level)
		}
	}
	var out, errOut bytes.Buffer
	err := clientPolicySetOperation([]string{"--level", "verbose"}, nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "error, warn, info, or debug") {
		t.Fatalf("invalid level error=%v", err)
	}
}
