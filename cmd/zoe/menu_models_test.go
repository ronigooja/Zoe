package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

func TestPurposeMenuExactListing(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("0\n"), &out, &errOut, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	const want = `
Zoe maintenance
Overview:
  1) Show status
  2) First-time setup
System:
  3) Install host
  4) Update host
  5) Uninstall host
Models:
  6) Configure Worker model (runs tasks)
  7) Test Worker model
  8) Configure Register model (routes every sentence)
  9) Test Register model
  10) Configure Compact model (hourly conversation compaction)
  11) Test Compact model
  12) Configure Steward model (task routing and replies)
  13) Test Steward model
  14) Configure Memory extraction model (24-hour batch and memory worker)
  15) Test Memory extraction model
  16) Configure Memory answer model (recall and memory questions)
  17) Test Memory answer model
Voice:
  18) Configure voice gateway
  19) Test voice gateway
  20) Show voice gateway status
  21) Configure voice instruction
Access:
  22) Create instance key
  23) Rotate instance key
Client policy:
  24) Show client policy
  25) Set client policy
  0) Exit
Select an option: `
	if out.String() != want || errOut.Len() != 0 {
		t.Fatalf("unexpected menu: %s errors=%s", out.String(), errOut.String())
	}
}

func TestRemovedTriageCommands(t *testing.T) {
	for _, command := range []string{"configure", "test"} {
		err := run([]string{"triage", command}, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "usage: zoe") || strings.Contains(err.Error(), "triage") {
			t.Fatalf("unexpected usage: %v", err)
		}
	}
	for _, kind := range []string{"register", "compact", "steward"} {
		if err := run([]string{kind, "configure"}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Fatalf("%s must be menu-only", kind)
		}
	}
}

func TestIndependentMenuModelsConfigureAndProbe(t *testing.T) {
	for _, tc := range []struct{ kind, configure, test, other string }{{"Register", "8", "9", "COMPACT"}, {"Compact", "10", "11", "REGISTER"}} {
		t.Run(tc.kind, func(t *testing.T) {
			menuManager(t)
			containerKeyFixture(t)
			root := t.TempDir()
			prefix := "ZOE_" + strings.ToUpper(tc.kind)
			legacy := "# operator comment\nZOE_TRIAGE_BASE_URL=http://ignored.invalid\nZOE_TRIAGE_MODEL=old\nZOE_TRIAGE_TIMEOUT_MS=invalid-unused\n"
			other := "ZOE_" + tc.other + "_MODEL=unchanged\nZOE_" + tc.other + "_API_KEY_FILE=/untouched\n"
			path := filepath.Join(root, "zoe-stack.env")
			if err := os.WriteFile(path, []byte(legacy+other), 0640); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer private-fixture" {
					t.Error("wrong model path/auth/method")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != tc.kind+"-model" {
					t.Error("wrong model")
				}
				_, _ = io.WriteString(w, `{"output_text":"OK"}`)
			}))
			defer server.Close()
			old := terminalEcho
			terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
			t.Cleanup(func() { terminalEcho = old })
			input := tc.configure + "\n" + server.URL + "/v1\n" + tc.kind + "-model\nnone\ndisabled\n\nprivate-fixture\ny\n" + tc.test + "\n0\n"
			var out, errOut bytes.Buffer
			if err := menuWithRoot(strings.NewReader(input), &out, &errOut, root); err != nil {
				t.Fatal(err)
			}
			if errOut.Len() != 0 || requests != 2 || strings.Count(out.String(), "PASS style=") != 2 {
				t.Fatalf("requests=%d errors=%s", requests, errOut.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(root, "secrets", strings.ToLower(tc.kind)+"-api-key")
			want := legacy + other + fmt.Sprintf("%s_BASE_URL=%q\n%s_MODEL=%q\n%s_REASONING_EFFORT=\"none\"\n%s_THINKING=\"disabled\"\n%s_API_KEY_FILE=%q\n", prefix, server.URL+"/v1", prefix, tc.kind+"-model", prefix, prefix, prefix, key)
			if string(data) != want {
				t.Fatal("configuration wrote unexpected keys or changed unrelated lines")
			}
			for p, mode := range map[string]os.FileMode{path: 0640, key: 0600} {
				info, err := os.Stat(p)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("incorrect file mode: %s %v", p, err)
				}
			}
			if err := ops.ValidateModelKeyReadable(key); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String()+errOut.String()+string(data), "private-fixture") {
				t.Fatal("secret leaked")
			}
			if !strings.Contains(out.String(), "Apply with: zoe update (restart/redeploy the server)") {
				t.Fatal("missing apply reminder")
			}
		})
	}
}

func TestIndependentMenuModelsMissingKeysNeverFallback(t *testing.T) {
	for _, kind := range []string{"Register", "Compact"} {
		for _, missing := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			t.Run(kind+missing, func(t *testing.T) {
				menuManager(t)
				root := t.TempDir()
				prefix := "ZOE_" + strings.ToUpper(kind)
				env := "ZOE_STEWARD_BASE_URL=https://never-use.invalid\nZOE_STEWARD_MODEL=never-use\nZOE_STEWARD_API_KEY_FILE=/never-use\n"
				for _, item := range []struct{ key, value string }{{"BASE_URL", "https://never-use.invalid"}, {"MODEL", "m"}, {"API_KEY_FILE", "/never-use"}} {
					if item.key != missing {
						env += prefix + "_" + item.key + "=" + item.value + "\n"
					}
				}
				if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte(env), 0600); err != nil {
					t.Fatal(err)
				}
				choice := "9"
				if kind == "Compact" {
					choice = "11"
				}
				var out, errOut bytes.Buffer
				if err := menuWithRoot(strings.NewReader(choice+"\n0\n"), &out, &errOut, root); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(errOut.String(), prefix+"_"+missing) || !strings.Contains(errOut.String(), "server will not start without it") || strings.Contains(out.String(), "PASS style=") {
					t.Fatalf("missing required-key error: %s", errOut.String())
				}
			})
		}
	}
}

func TestIndependentMenuModelsProbeFailureMakesOneCall(t *testing.T) {
	for _, kind := range []string{"Register", "Compact"} {
		for _, status := range []int{401, 404, 500} {
			t.Run(fmt.Sprintf("%s%d", kind, status), func(t *testing.T) {
				menuManager(t)
				containerKeyFixture(t)
				root := t.TempDir()
				requests := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(status) }))
				defer s.Close()
				key := filepath.Join(root, "key")
				if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				prefix := "ZOE_" + strings.ToUpper(kind)
				env := prefix + "_BASE_URL=" + s.URL + "\n" + prefix + "_MODEL=m\n" + prefix + "_API_KEY_FILE=" + key + "\n"
				if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte(env), 0600); err != nil {
					t.Fatal(err)
				}
				var out, errOut bytes.Buffer
				choice := "9"
				if kind == "Compact" {
					choice = "11"
				}
				if err := menuWithRoot(strings.NewReader(choice+"\n0\n"), &out, &errOut, root); err != nil {
					t.Fatal(err)
				}
				if requests != 1 || errOut.Len() == 0 || strings.Contains(out.String(), "PASS style=") {
					t.Fatalf("requests=%d errors=%s", requests, errOut.String())
				}
			})
		}
	}
}
