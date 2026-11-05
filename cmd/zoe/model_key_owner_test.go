package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusWarnsWhenModelKeyFileIsNotReadableByServerUser(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("never-print-this-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=https://gateway.example\nZOE_TRIAGE_MODEL=m\nZOE_TRIAGE_API_KEY_FILE="+key+"\nZOE_MEM0_LLM_BASE_URL=https://gateway.example\nZOE_MEM0_LLM_API_KEY_FILE="+key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeServices(t, false)
	var out, errOut bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"WARNING:", key, "not readable by server user 65532:65532", "sudo chown", "chmod 0600"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got+errOut.String(), "never-print-this-key") {
		t.Fatal("key leaked")
	}
}
