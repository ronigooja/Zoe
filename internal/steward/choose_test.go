package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func choiceReply(action, request string) string {
	fields := map[string]string{"action": action, "task": request}
	if action == "report" {
		fields["reply"] = "The task is active."
	}
	data, _ := json.Marshal(fields)
	return string(data)
}

const choiceNow = "2026-11-12T12:34:56Z"

// Old Choose rejects the reply field, cannot report without a task, and omits now.
// These HTTP-backed cases exercise the model wire contract, not a parser stub.
func TestChooseReportReplyBoundsAndNoTask(t *testing.T) {
	for _, request := range []string{"", "Original request"} {
		for _, text := range []string{"  No such task was found. \n", " \t" + strings.Repeat("🙂é", 350) + "\n"} {
			data, _ := json.Marshal(map[string]string{"action": "report", "task": request, "reply": text})
			s := rosterServer(t, string(data), nil)
			got, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, []Candidate{{Request: "Original request"}})
			s.Close()
			want := []rune(strings.TrimSpace(text))
			if len(want) > 600 {
				want = want[:600]
			}
			if err != nil || got != (Choice{Action: ActionReport, Request: request, Reply: string(want)}) || !utf8.ValidString(got.Reply) {
				t.Fatalf("choice=%+v err=%v", got, err)
			}
		}
	}
}

// Old Choose rejects cancel as an unknown action; exact-match and empty-reply
// success cases, ErrNoMatch checks, and the instruction assertion regress it.
func TestChooseNonReportReply(t *testing.T) {
	for _, action := range []string{"new", "none", "attach", "answer", "cancel"} {
		for _, reply := range []string{"", "state", " "} {
			request := ""
			if action == "attach" || action == "answer" || action == "cancel" {
				request = "Original request"
			}
			data, _ := json.Marshal(map[string]string{"action": action, "task": request, "reply": reply})
			s := rosterServer(t, string(data), nil)
			_, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, []Candidate{{Request: "Original request", Question: "Which?"}})
			s.Close()
			if (err == nil) != (reply == "") {
				t.Fatalf("action=%s reply=%q err=%v", action, reply, err)
			}
		}
	}
}

func TestChooseValidReplies(t *testing.T) {
	candidates := []Candidate{{Request: " Original request 🙂\n", Question: "Which?"}}
	for _, action := range []Action{ActionNew, ActionNone, ActionAttach, ActionAnswer, ActionCancel, ActionReport} {
		t.Run(string(action), func(t *testing.T) {
			request := candidates[0].Request
			if action == ActionNew || action == ActionNone {
				request = ""
			}
			s := rosterServer(t, choiceReply(string(action), request), nil)
			defer s.Close()
			got, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, candidates)
			want := Choice{Action: action, Request: request}
			if action == ActionReport {
				want.Reply = "The task is active."
			}
			if err != nil || got != want {
				t.Fatalf("choice=%+v err=%v", got, err)
			}
		})
	}
}

func TestChooseExactMatchAndNoMatch(t *testing.T) {
	request := " Original Request\n"
	for _, action := range []Action{ActionAttach, ActionAnswer, ActionCancel, ActionReport} {
		for _, quote := range []string{"Original Request\n", " Original Request", " original request\n", "unknown", ""} {
			if action == ActionReport && quote == "" {
				continue // An unmatched report intentionally has no task.
			}
			t.Run(string(action)+"/"+quote, func(t *testing.T) {
				s := rosterServer(t, choiceReply(string(action), quote), nil)
				defer s.Close()
				got, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, []Candidate{{Request: request, Question: "Which?"}})
				if !errors.Is(err, ErrNoMatch) || got.Action != action || got.Request != quote {
					t.Fatalf("choice=%+v err=%v", got, err)
				}
			})
		}
	}
}

func TestChooseRejectsInvalidReplies(t *testing.T) {
	for _, reply := range []string{
		"", " \n", "not JSON", `[]`, `null`, `{"action":"new","task":""} extra`,
		`{"action":"new","task":""}{"action":"none","task":""}`,
		`{"action":"new","task":"","extra":""}`, `{"action":"unknown","task":""}`,
		`{"action":"NEW","task":""}`, `{"action":"new"}`, `{"task":""}`,
		`{"action":"new","task":null}`, `{"action":"new","task":4}`,
		`{"Action":"new","task":""}`, `{"action":"new","task":"","task":""}`,
		`{"action":"new","task":"request"}`, `{"action":"none","task":"request"}`,
		`{"action":"cancel"}`, `{"action":"cancel","task":""}`,
		`{"action":"answer","task":"request"}`, `{"action":"new","task":"",}`,
		`{"action":"report","task":"request"}`, `{"action":"report","task":"","reply":""}`,
		`{"action":"report","task":"request","reply":" \n\t"}`,
		`{"action":"report","task":"request","reply":null}`,
		`{"action":"report","task":"request","reply":3}`,
		`{"action":"report","task":"request","reply":"state","reply":"state"}`,
		`{"action":"report","task":"request","reply":"state","id":"1"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			s := rosterServer(t, reply, nil)
			defer s.Close()
			_, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, []Candidate{{Request: "request"}})
			if err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
}

func TestChooseDuplicateRequestsUseNewest(t *testing.T) {
	for _, newestHasQuestion := range []bool{false, true} {
		candidates := []Candidate{{Request: "same", CreatedAt: "newest"}, {Request: "same", CreatedAt: "older", Question: "Older question?"}}
		if newestHasQuestion {
			candidates[0].Question = "Newest question?"
			candidates[1].Question = ""
		}
		s := rosterServer(t, choiceReply("answer", "same"), nil)
		_, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, candidates)
		s.Close()
		if (err == nil) != newestHasQuestion {
			t.Fatalf("newest has question=%v err=%v", newestHasQuestion, err)
		}
	}
}

func TestChooseCancelDuplicateRequestsPreserveNewestFirstInput(t *testing.T) {
	candidates := []Candidate{
		{Request: "same", Status: "active", CreatedAt: "newest"},
		{Request: "same", Status: "done", CreatedAt: "older"},
	}
	s := rosterServer(t, choiceReply("cancel", "same"), func(body []byte) {
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(body, &outer); err != nil {
			t.Error(err)
			return
		}
		var input, instructions string
		if err := json.Unmarshal(outer["input"], &input); err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(outer["instructions"], &instructions); err != nil {
			t.Error(err)
		}
		want := `{"sentence":"stop that work","restatement":"withdraw the same task","now":"2026-11-12T12:34:56Z","tasks":[{"request":"same","status":"active","created_at":"newest","finished_at":"","question":"","result":"","additions":[]},{"request":"same","status":"done","created_at":"older","finished_at":"","question":"","result":"","additions":[]}]}`
		if input != want || instructions != chooseInstructions {
			t.Error("cancel changed the input contract or newest-first order")
		}
	})
	defer s.Close()
	got, err := configured(t, s.URL, "").Choose(context.Background(), "stop that work", "withdraw the same task", choiceNow, candidates)
	if err != nil || got != (Choice{Action: ActionCancel, Request: "same"}) {
		t.Fatalf("choice=%+v err=%v", got, err)
	}
}

func TestChooseInputNoAliasesRuneCapsAndOrder(t *testing.T) {
	candidates := []Candidate{{Request: strings.Repeat("界", 401)}}
	for i := 0; i < 12; i++ {
		candidates = append(candidates, Candidate{
			Request: strconv.Itoa(i) + strings.Repeat("界🙂", 150), Status: "active",
			CreatedAt: "created-" + strconv.Itoa(i), FinishedAt: "finished-" + strconv.Itoa(i),
			Question: strings.Repeat("问🙂", 70), Result: strings.Repeat("果🙂", 170),
			Additions: []string{strings.Repeat("加🙂", 120), "second", "third", "fourth", "fifth", "sixth"},
		})
	}
	// Exactly 400 runes remains usable and must not be shortened to 200.
	candidates[1].Request = strings.Repeat("界🙂", 200)
	sentence, restatement := "private original sentence", "private restatement"
	s := rosterServer(t, choiceReply("attach", candidates[1].Request), func(body []byte) {
		var outer struct{ Input, Instructions string }
		if err := json.Unmarshal(body, &outer); err != nil {
			t.Error(err)
			return
		}
		if outer.Instructions != chooseInstructions {
			t.Error("missing choice instructions")
		}
		for _, forbidden := range []string{`"ref"`, `"id"`, `"alias"`, `"key"`, `"t1"`, `"t10"`} {
			if strings.Contains(outer.Input, forbidden) {
				t.Errorf("model input contains generated reference %s", forbidden)
			}
		}
		var input struct {
			Sentence, Restatement, Now string
			Tasks                      []map[string]json.RawMessage
		}
		if err := json.Unmarshal([]byte(outer.Input), &input); err != nil {
			t.Error(err)
			return
		}
		if input.Sentence != sentence || input.Restatement != restatement || input.Now != choiceNow || len(input.Tasks) != 10 {
			t.Errorf("incorrect sentence/restatement or count=%d", len(input.Tasks))
			return
		}
		for i, task := range input.Tasks {
			if len(task) != 7 {
				t.Errorf("unexpected candidate fields=%d", len(task))
			}
			for field, want := range map[string]string{
				"request": candidates[i+1].Request, "status": "active", "created_at": candidates[i+1].CreatedAt, "finished_at": candidates[i+1].FinishedAt,
				"question": strings.Repeat("问🙂", 60), "result": strings.Repeat("果🙂", 150),
			} {
				var got string
				if err := json.Unmarshal(task[field], &got); err != nil || got != want || !utf8.ValidString(got) {
					t.Errorf("candidate %d field %s not preserved/capped correctly", i, field)
				}
			}
			var additions []string
			if err := json.Unmarshal(task["additions"], &additions); err != nil || len(additions) != 5 || additions[0] != strings.Repeat("加🙂", 100) || !utf8.ValidString(additions[0]) || additions[4] != "fifth" {
				t.Errorf("candidate %d additions not capped correctly", i)
			}
		}
	})
	defer s.Close()
	got, err := configured(t, s.URL, "").Choose(context.Background(), sentence, restatement, choiceNow, candidates)
	if err != nil || got != (Choice{Action: ActionAttach, Request: candidates[1].Request, Dropped: 3}) {
		t.Fatalf("choice=%+v err=%v", got, err)
	}
	if len(candidates[1].Additions) != 6 || utf8.RuneCountInString(candidates[1].Question) != 140 {
		t.Fatal("caller candidates mutated")
	}
}

func TestChooseDroppedCandidatesCannotMatch(t *testing.T) {
	overlong := strings.Repeat("🙂", 401)
	for _, request := range []string{overlong, "excluded", strings.Repeat("🙂", 400)} {
		candidates := []Candidate{{Request: overlong}}
		for i := 0; i < 10; i++ {
			candidates = append(candidates, Candidate{Request: strconv.Itoa(i)})
		}
		candidates = append(candidates, Candidate{Request: "excluded"})
		s := rosterServer(t, choiceReply("attach", request), nil)
		got, err := configured(t, s.URL, "").Choose(context.Background(), "sentence", "none", choiceNow, candidates)
		s.Close()
		if !errors.Is(err, ErrNoMatch) || errors.Is(err, ErrRequestTooLong) != (request == overlong) || got.Dropped != 2 {
			t.Fatalf("dropped=%d err=%v", got.Dropped, err)
		}
	}
}

func TestChooseEmptyCandidatesAndInstructionLanguage(t *testing.T) {
	const none = "none = asks for no work."
	if !strings.Contains(chooseInstructions, none) {
		t.Error("none definition must be the task-only exit, without duplicating register chat")
	}
	for _, removed := range []string{"asks for no new work", "including a sentence that tells running work to continue", "sentences that only ask about state are report", "withdrawals of work are cancel"} {
		if strings.Contains(chooseInstructions, removed) {
			t.Errorf("instruction retains removed none wording %q", removed)
		}
	}
	for _, chatOnly := range []string{"conversation", "thanks", "acknowledgement"} {
		if strings.Contains(chooseInstructions, chatOnly) {
			t.Errorf("Steward duplicates register chat: %s", chatOnly)
		}
	}
	for _, required := range []string{"cancel = the sentence withdraws or stops the work of one listed task", "For attach, answer, cancel, and a matched report", "report", "newest", "current time", "reply language follows the input"} {
		if !strings.Contains(chooseInstructions, required) {
			t.Errorf("instruction missing %q", required)
		}
	}
	for _, r := range chooseInstructions {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			t.Fatal("CJK in choice instructions")
		}
	}
	s := rosterServer(t, choiceReply("none", ""), func(body []byte) {
		var outer struct{ Input string }
		if err := json.Unmarshal(body, &outer); err != nil {
			t.Error(err)
		}
		if outer.Input != `{"sentence":"keep going","restatement":"none","now":"2026-11-12T12:34:56Z","tasks":[]}` {
			t.Error("incorrect empty candidate input")
		}
	})
	defer s.Close()
	if _, err := configured(t, s.URL, "").Choose(context.Background(), "keep going", "none", choiceNow, nil); err != nil {
		t.Fatal(err)
	}
}

func TestChooseLogsExcludePrivateData(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	s := rosterServer(t, "private-model-output", nil)
	defer s.Close()
	_, err := configured(t, s.URL, "").Choose(context.Background(), "private-sentence", "private-restatement", choiceNow, []Candidate{{Request: "private-request", Question: "private-question", Result: "private-result", Additions: []string{"private-addition"}}})
	if err == nil {
		t.Fatal("expected invalid choice error")
	}
	for _, private := range []string{"private-sentence", "private-restatement", "private-request", "private-question", "private-result", "private-addition", "private-model-output"} {
		if strings.Contains(logs.String(), private) || strings.Contains(err.Error(), private) {
			t.Fatal("private data in diagnostic output")
		}
	}
	for _, field := range []string{"steward_call configured_ms=30000", "effective_ms=", "elapsed_ms=", "error_kind="} {
		if !strings.Contains(logs.String(), field) {
			t.Errorf("missing log field %q", field)
		}
	}
}

func TestChooseTimeoutTransportDisabledAndDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := configured(t, s.URL, "200").Choose(ctx, "sentence", "none", choiceNow, nil); err == nil {
		t.Fatal("timeout accepted")
	}
	s.Close()
	if _, err := configured(t, s.URL, "200").Choose(context.Background(), "sentence", "none", choiceNow, nil); err == nil {
		t.Fatal("transport failure accepted")
	}
	var disabled *Client
	if _, err := disabled.Choose(context.Background(), "sentence", "none", choiceNow, nil); err == nil {
		t.Fatal("disabled client accepted")
	}
	c := configured(t, "http://localhost", "200")
	c.SetHTTPClient(&http.Client{Transport: stewardDeadlineTransport{t: t, want: 200 * time.Millisecond}})
	if _, err := c.Choose(context.Background(), "sentence", "none", choiceNow, nil); err == nil {
		t.Fatal("expected transport error")
	}
}
