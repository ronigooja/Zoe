package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceMenuListsAnswerItems(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("0\n"), &out, &errOut, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"16) Configure Memory answer model", "17) Test Memory answer model", "18) Configure voice gateway", "25) Set client policy"} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("missing %s", label)
		}
	}
	if strings.Contains(out.String(), "Call summary") {
		t.Fatal("retired summary menu item")
	}
}

func TestSummaryCommandsAreRemoved(t *testing.T) {
	for _, command := range []string{"configure", "test"} {
		if err := run([]string{"summary", command}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Fatal("retired summary command accepted")
		}
	}
}

func TestMaintenanceMenuAnswerFlowWritesEnvAndSecretAndTests(t *testing.T) {
	containerKeyFixture(t)
	menuManager(t)
	root := t.TempDir()
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer menu-private" {
			t.Error("wrong key")
		}
		var request struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if !strings.Contains(request.Input, "When is the project meeting?") || !strings.Contains(request.Instructions, "memory_items and conversation record") {
			t.Error("runtime answer request missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"status":"answer","text":"In the January call, the meeting was set for Monday."}`})
	}))
	defer s.Close()
	input := strings.NewReader("16\nhttp://remote.invalid\n" + s.URL + "\n\nfast-answer\nwrong\nlow\nwrong\ndisabled\n\nmenu-private\ny\n17\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(input, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ZOE_ANSWER_MODEL="fast-answer"`, `ZOE_ANSWER_REASONING_EFFORT="low"`, `ZOE_ANSWER_THINKING="disabled"`, s.URL} {
		if !strings.Contains(string(env), want) {
			t.Errorf("missing %s in env", want)
		}
	}
	keyPath := filepath.Join(root, "secrets", "answer-api-key")
	key, err := os.ReadFile(keyPath)
	if err != nil || string(key) != "menu-private\n" {
		t.Fatal("secret not saved")
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("secret permissions")
	}
	if calls != 2 || strings.Count(out.String(), "PASS style=responses") != 2 {
		t.Fatalf("calls=%d out=%s err=%s", calls, &out, &errOut)
	}
	if strings.Contains(out.String()+errOut.String(), "menu-private") {
		t.Fatal("key leaked")
	}
	if strings.Count(errOut.String(), "Nothing was saved.") != 4 || !strings.Contains(out.String(), "Saved.") {
		t.Fatalf("validation output=%s", &errOut)
	}
}

func TestAnswerTestUsesRuntimeClient(t *testing.T) {
	containerKeyFixture(t)
	menuManager(t)
	for _, kind := range []string{"answer"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			calls := 0
			invalid := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Input        string `json:"input"`
					Instructions string `json:"instructions"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				output := `{"status":"answer","text":"In the January call, the meeting was set for Monday."}`
				if !strings.Contains(body.Input, "The project meeting is on Monday.") || !strings.Contains(body.Instructions, "Never invent") {
					t.Error("answer runtime not used")
				}
				if invalid {
					output = "not JSON"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
			}))
			defer s.Close()
			var out, errOut bytes.Buffer
			if err := run([]string{kind, "configure", "--root", root, "--base-url", s.URL, "--model", "runtime-model", "--effort", "low", "--thinking", "disabled", "--key-stdin"}, strings.NewReader("probe-private"), &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{kind, "test", "--root", root}, nil, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			invalid = true
			if err := run([]string{kind, "test", "--root", root}, nil, &out, &errOut); err == nil {
				t.Fatal("probe bypassed runtime JSON validation")
			}
			if calls != 2 || !strings.Contains(out.String(), "PASS style=responses") || !strings.Contains(out.String(), "FAIL style=responses") || !strings.Contains(out.String(), "latency=") {
				t.Fatalf("calls=%d output=%s", calls, &out)
			}
			if strings.Contains(out.String()+errOut.String(), "probe-private") {
				t.Fatal("probe leaked key")
			}
		})
	}
}

func TestStatusShowsAnswerWithoutSecrets(t *testing.T) {
	containerKeyFixture(t)
	menuManager(t)
	root := t.TempDir()
	for _, kind := range []string{"answer"} {
		if err := run([]string{kind, "configure", "--root", root, "--base-url", "https://" + kind + ".example/v1", "--model", kind + "-model", "--effort", "medium", "--thinking", "disabled", "--key-stdin"}, strings.NewReader(kind+"-private"), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Answer: enabled style=auto host=answer.example model=answer-model effort=medium thinking=disabled timeout-ms=8000 key-file-present=true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out.String()+errOut.String(), "summary-private") || strings.Contains(out.String()+errOut.String(), "answer-private") {
		t.Fatal("status leaked key")
	}
}
