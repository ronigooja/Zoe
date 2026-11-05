package summarize

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_SUMMARY_BASE_URL": server.URL, "ZOE_SUMMARY_MODEL": "gpt-6-summary", "ZOE_SUMMARY_API_KEY_FILE": "/key"}[name]
	}, func(string) ([]byte, error) { return []byte("secret"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func respond(w http.ResponseWriter, text string) {
	_ = json.NewEncoder(w).Encode(map[string]string{"output_text": text})
}
func turns() []Turn {
	return []Turn{{Seq: 4, Role: "user", Text: "Alex will bring 12 documents on Monday."}, {Seq: 5, Role: "assistant", Text: "The decision is confirmed."}, {Seq: 6, Role: "user", Text: "The budget is 42 dollars."}}
}

func TestSummarizeRequestContainsWholeTranscriptWithSeqNumbers(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var got []Turn
		if err := json.Unmarshal([]byte(body.Input), &got); err != nil {
			t.Error(err)
		}
		want, _ := json.Marshal(turns())
		actual, _ := json.Marshal(got)
		if string(want) != string(actual) {
			t.Errorf("transcript changed: %s", actual)
		}
		if !strings.Contains(body.Instructions, "names, numbers, dates, decisions, and commitments") {
			t.Error("missing preservation instruction")
		}
		respond(w, `[{"topic":"Plan","summary":"Alex committed to the documents and budget.","first_seq":4,"last_seq":6}]`)
	})
	if _, err := c.Summarize(context.Background(), turns()); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizeParsesValidTopicsAndRejectsGapsOverlapsAndOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"valid", `[{"topic":"Documents","summary":"Documents on Monday.","first_seq":4,"last_seq":5},{"topic":"Budget","summary":"42 dollars.","first_seq":6,"last_seq":6}]`, true},
		{"gap", `[{"topic":"a","summary":"b","first_seq":4,"last_seq":4},{"topic":"c","summary":"d","first_seq":6,"last_seq":6}]`, false},
		{"overlap", `[{"topic":"a","summary":"b","first_seq":4,"last_seq":5},{"topic":"c","summary":"d","first_seq":5,"last_seq":6}]`, false},
		{"out_of_range", `[{"topic":"a","summary":"b","first_seq":4,"last_seq":7}]`, false},
		{"missing_start", `[{"topic":"a","summary":"b","first_seq":5,"last_seq":6}]`, false},
		{"missing_end", `[{"topic":"a","summary":"b","first_seq":4,"last_seq":5}]`, false},
		{"reversed", `[{"topic":"a","summary":"b","first_seq":4,"last_seq":3}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, tc.output) })
			result, err := c.Summarize(context.Background(), turns())
			if (err == nil) != tc.valid {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if tc.valid && (len(result) != 2 || result[1].FirstSeq != 6) {
				t.Fatalf("result=%v", result)
			}
		})
	}
}

func TestSummarizeRejectsNonJSONAndEmptyFields(t *testing.T) {
	for _, output := range []string{"not JSON", "null", "[]", `[{"topic":"","summary":"b","first_seq":4,"last_seq":6}]`, `[{"topic":"a","summary":"  ","first_seq":4,"last_seq":6}]`, `[{"topic":"a","summary":"b","first_seq":4,"last_seq":6,"extra":true}]`, `[{"topic":"a","summary":"b","first_seq":4,"last_seq":6}] trailing`} {
		t.Run(output, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, output) })
			if _, err := c.Summarize(context.Background(), turns()); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
}

func TestSummarizeFallsBackToChatCompletions(t *testing.T) {
	responses, chat := 0, 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			responses++
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		chat++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["max_tokens"] != float64(1024) {
			t.Error("chat token limit mismatch")
		}
		io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"content":"[{\"topic\":\"Plan\",\"summary\":\"The plan is agreed.\",\"first_seq\":4,\"last_seq\":6}]"}}]}`)
	})
	for i := 0; i < 2; i++ {
		if _, err := c.Summarize(context.Background(), turns()); err != nil {
			t.Fatal(err)
		}
	}
	if responses != 1 || chat != 2 || c.Style() != "chat" {
		t.Fatalf("responses=%d chat=%d style=%s", responses, chat, c.Style())
	}
}
