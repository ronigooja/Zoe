package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

func TestStewardMenuConfigurationAndOwnedKey(t *testing.T) {
	containerKeyFixture(t)
	menuManager(t)
	root := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"output_text":"dispatch"}`)) }))
	defer s.Close()
	keyPath := filepath.Join(root, "secrets", "steward-key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	input := strings.NewReader("12\n" + s.URL + "\nconfigured-steward\nnone\ndisabled\n" + keyPath + "\nprivate-steward-fixture\ny\n13\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(input, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("menu error: %s", errOut.String())
	}
	env, err := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ZOE_STEWARD_MODEL=\"configured-steward\"", "ZOE_STEWARD_REASONING_EFFORT=\"none\"", "ZOE_STEWARD_THINKING=\"disabled\""} {
		if !strings.Contains(string(env), value) {
			t.Errorf("missing %s", value)
		}
	}
	if err := ops.ValidateModelKeyReadable(keyPath); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String()+errOut.String()+string(env), "private-steward-fixture") {
		t.Fatal("secret logged")
	}
	if strings.Count(out.String(), "PASS style=") != 2 {
		t.Fatalf("probes missing: %s", out.String())
	}
}
