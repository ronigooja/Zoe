package answer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"
)

func candidates() []Candidate {
	return []Candidate{{ID: "s-1", Topic: "Meeting", StartTime: "2026-11-06T10:00:00Z", EndTime: "2026-11-06T10:01:00Z", Summary: "The meeting is on Monday."}}
}
func testClient(t *testing.T, output string, inspect func(*http.Request)) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inspect != nil {
			inspect(r)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
	}))
	t.Cleanup(s.Close)
	c, err := FromEnv(func(name string) string {
		return map[string]string{"ZOE_ANSWER_BASE_URL": s.URL, "ZOE_ANSWER_MODEL": "fast", "ZOE_ANSWER_API_KEY_FILE": "/key"}[name]
	}, func(string) ([]byte, error) { return []byte("secret"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnswerAnswersFromSummariesWithoutTranscript(t *testing.T) {
	c := testClient(t, `{"status":"answer","text":"In the call this morning, the meeting was set for Monday."}`, func(r *http.Request) {
		var body struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !strings.Contains(body.Input, "The meeting is on Monday.") || !strings.Contains(body.Input, "2026-11-06T10:00:00Z") || strings.Contains(body.Input, "excerpts") {
			t.Errorf("unexpected evidence: %s", body.Input)
		}
		if !strings.Contains(body.Instructions, "Answer from summaries alone whenever possible") || !strings.Contains(body.Instructions, "exact wording or detail absent") {
			t.Error("summary-first contract missing")
		}
	})
	r, err := c.Answer(context.Background(), "When is the meeting?", candidates(), nil)
	if err != nil || r.Status != "answer" || r.Text == "" {
		t.Fatalf("result=%v err=%v", r, err)
	}
}

func TestAnswerRequestsTranscriptOnlyWithKnownSummaryIDs(t *testing.T) {
	c := testClient(t, `{"status":"need_transcript","summary_ids":["s-1"]}`, nil)
	r, err := c.Answer(context.Background(), "What were my exact words?", candidates(), nil)
	if err != nil || len(r.SummaryIDs) != 1 || r.SummaryIDs[0] != "s-1" {
		t.Fatalf("result=%v err=%v", r, err)
	}
}

func TestAnswerRejectsUnknownStatusAndForeignSummaryIDs(t *testing.T) {
	for _, output := range []string{`{"status":"maybe"}`, `{"status":"need_transcript","summary_ids":["foreign"]}`, `{"status":"need_transcript","summary_ids":[]}`, `{"status":"need_transcript","summary_ids":["s-1","s-1"]}`, `{"status":"not_found","text":"invented"}`, `{"status":"not_found","extra":1}`} {
		t.Run(output, func(t *testing.T) {
			c := testClient(t, output, nil)
			if _, err := c.Answer(context.Background(), "What was said?", candidates(), nil); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestAnswerRejectsEmptyText(t *testing.T) {
	for _, output := range []string{`{"status":"answer"}`, `{"status":"answer","text":"   "}`, `{"status":"answer","text":""}`} {
		c := testClient(t, output, nil)
		if _, err := c.Answer(context.Background(), "When?", candidates(), nil); err == nil {
			t.Fatal("empty answer accepted")
		}
	}
}

func TestAnswerPromptContainsNoExampleSentences(t *testing.T) {
	if strings.ContainsAny(instructions, "\"'“”‘’") {
		t.Fatal("instruction contains quotation marks")
	}
	for _, r := range instructions {
		if unicode.Is(unicode.Han, r) {
			t.Fatal("instruction contains CJK")
		}
	}
	if strings.Contains(strings.ToLower(instructions), "example") {
		t.Fatal("instruction includes examples")
	}
}
