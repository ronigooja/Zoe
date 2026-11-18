package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/prompts"
)

func text(s string) *string { return &s }

func validChoice(action Action) Choice {
	choice := Choice{Action: action}
	switch action {
	case ActionAppend, ActionContinue, ActionCancel, ActionRedisplay, ActionAnswer, ActionReport:
		choice.TaskID = text("stable-task-2")
	}
	if action == ActionAnswer {
		choice.QuestionID = text("stable-question-2")
	}
	if action == ActionReport || action == ActionClarify {
		choice.Reply = text("  完整回复🙂\n")
	}
	return choice
}

func encoded(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func facts() Input {
	return Input{Request: "full retained request", Tasks: []Candidate{
		{TaskID: "stable-task-1", Goal: "duplicate goal", Status: "active", PendingQuestions: []Question{{QuestionID: "stable-question-1", Text: "Which?"}}},
		{TaskID: "stable-task-2", Goal: "duplicate goal", Status: "completed", PendingQuestions: []Question{{QuestionID: "stable-question-2", Text: "Which?"}}, Result: text("full result")},
	}}
}

func TestChooseAllActionsStableIDs(t *testing.T) {
	for _, action := range []Action{ActionNew, ActionAppend, ActionAnswer, ActionContinue, ActionReport, ActionCancel, ActionRedisplay, ActionClarify} {
		t.Run(string(action), func(t *testing.T) {
			want := validChoice(action)
			s := choiceServer(t, encoded(want), nil)
			defer s.Close()
			got, err := configured(t, s.URL, "").Choose(context.Background(), facts())
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("choice=%+v err=%v", got, err)
			}
		})
	}
}

func TestParseChoiceExactNullability(t *testing.T) {
	for _, action := range []Action{ActionNew, ActionAppend, ActionAnswer, ActionContinue, ActionReport, ActionCancel, ActionRedisplay, ActionClarify} {
		want := validChoice(action)
		for _, field := range []string{"task_id", "question_id", "reply"} {
			var fields map[string]any
			_ = json.Unmarshal([]byte(encoded(want)), &fields)
			if fields[field] == nil {
				fields[field] = "unexpected"
			} else {
				fields[field] = nil
			}
			if _, err := parseChoice(encoded(fields)); err == nil {
				t.Fatalf("accepted wrong nullability %s/%s", action, field)
			}
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(encoded(want)), &fields)
		for _, field := range []string{"action", "task_id", "question_id", "reply"} {
			copy := make(map[string]any)
			for key, value := range fields {
				if key != field {
					copy[key] = value
				}
			}
			if _, err := parseChoice(encoded(copy)); err == nil {
				t.Fatalf("accepted missing %s/%s", action, field)
			}
		}
	}
}

func TestParseChoiceRejectsMalformed(t *testing.T) {
	valid := encoded(validChoice(ActionNew))
	for _, output := range []string{
		"", "not JSON", "[]", "null", valid + " {}", valid + " trailing", strings.TrimSuffix(valid, "}") + `,"action":"new"}`,
		`{"action":"new","task_id":null,"question_id":null,"reply":null,"extra":null}`,
		`{"Action":"new","task_id":null,"question_id":null,"reply":null}`,
		`{"action":null,"task_id":null,"question_id":null,"reply":null}`,
		`{"action":"NEW","task_id":null,"question_id":null,"reply":null}`,
		`{"action":"attach","task_id":"stable-task-2","question_id":null,"reply":null}`,
		`{"action":"none","task_id":null,"question_id":null,"reply":null}`,
		`{"action":"report","task_id":"stable-task-2","question_id":null,"reply":" \n"}`,
		`{"action":"cancel","task_id":"","question_id":null,"reply":null}`,
		`{"action":"answer","task_id":"stable-task-2","question_id":3,"reply":null}`,
		`{"action":"clarify","task_id":null,"question_id":null,"reply":{}}`,
	} {
		if _, err := parseChoice(output); err == nil {
			t.Fatalf("accepted %s", output)
		}
	}
}

func TestChooseIdentityAndQuestionMembership(t *testing.T) {
	for _, tc := range []struct {
		task, question string
		want           error
	}{
		{"duplicate goal", "stable-question-2", ErrNoMatch},
		{"t2", "stable-question-2", ErrNoMatch},
		{"STABLE-task-2", "stable-question-2", ErrNoMatch},
		{"stable-task-2 ", "stable-question-2", ErrNoMatch},
		{"stable-task-2", "stable-question-1", ErrNoQuestion},
		{"stable-task-2", "Which?", ErrNoQuestion},
	} {
		choice := validChoice(ActionAnswer)
		choice.TaskID, choice.QuestionID = text(tc.task), text(tc.question)
		s := choiceServer(t, encoded(choice), nil)
		got, err := configured(t, s.URL, "").Choose(context.Background(), facts())
		s.Close()
		if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, Choice{}) {
			t.Fatalf("choice=%+v err=%v", got, err)
		}
	}
}

func TestChooseFullInputNoTruncationOrAliases(t *testing.T) {
	input := Input{Request: strings.Repeat("request🙂\n", 2000), DisplayRequested: true}
	for i := 0; i < 12; i++ {
		input.Tasks = append(input.Tasks, Candidate{TaskID: "real-" + strconv.Itoa(i), Goal: strings.Repeat("goal🙂", 500), Status: "arbitrary-authoritative-state", Result: text(strings.Repeat("result", 500)), Additions: []string{"first", "second", "third", "fourth", "fifth", "sixth"}})
	}
	before := encoded(input)
	want, _, err := prepareInput(input)
	if err != nil {
		t.Fatal(err)
	}
	s := choiceServer(t, encoded(Choice{Action: ActionContinue, TaskID: text("real-11")}), func(body []byte) {
		var outer struct{ Input, Instructions string }
		if err := json.Unmarshal(body, &outer); err != nil {
			t.Error(err)
			return
		}
		if outer.Input != encoded(want) || outer.Instructions != testStewardPrompt(t) {
			t.Error("wire input differs from complete canonical facts")
		}
	})
	defer s.Close()
	if _, err := configured(t, s.URL, "").Choose(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if encoded(input) != before {
		t.Fatal("mutated caller facts")
	}
}

func TestChooseEmptyCandidatesDoesNotDecideOrFallback(t *testing.T) {
	for _, action := range []Action{ActionNew, ActionClarify, ActionReport} {
		s := choiceServer(t, encoded(validChoice(action)), func(body []byte) {
			var outer struct{ Input string }
			_ = json.Unmarshal(body, &outer)
			if outer.Input != `{"request":"unresolved request","display_requested":false,"tasks":[]}` {
				t.Error("wrong empty input")
			}
		})
		got, err := configured(t, s.URL, "").Choose(context.Background(), Input{Request: "unresolved request"})
		s.Close()
		if action == ActionReport {
			if !errors.Is(err, ErrNoMatch) || got.Action != "" {
				t.Fatalf("invented report/fallback: %+v %v", got, err)
			}
		} else if err != nil || got.Action != action {
			t.Fatalf("code overrode model: %+v %v", got, err)
		}
	}
}

func TestPrepareInputRejectsMalformedFacts(t *testing.T) {
	for _, mutate := range []func(*Input){
		func(i *Input) { i.Request = " " },
		func(i *Input) { i.Tasks[0].TaskID = "" },
		func(i *Input) { i.Tasks[0].Goal = "" },
		func(i *Input) { i.Tasks[0].Status = "" },
		func(i *Input) { i.Tasks[1].TaskID = i.Tasks[0].TaskID },
		func(i *Input) { i.Tasks[0].PendingQuestions[0].QuestionID = "" },
		func(i *Input) { i.Tasks[0].PendingQuestions[0].Text = "" },
		func(i *Input) {
			i.Tasks[0].PendingQuestions = append(i.Tasks[0].PendingQuestions, i.Tasks[0].PendingQuestions[0])
		},
	} {
		input := facts()
		mutate(&input)
		if _, _, err := prepareInput(input); err == nil {
			t.Fatal("accepted malformed facts")
		}
	}
	input := facts()
	input.Tasks[1].PendingQuestions[0].QuestionID = input.Tasks[0].PendingQuestions[0].QuestionID
	if _, _, err := prepareInput(input); err != nil {
		t.Fatal("question IDs may be scoped per task", err)
	}
}

func TestChooseInstructionsDefinitionOnly(t *testing.T) {
	for _, forbidden := range []string{"for example", "e.g.", "such as", "newest listed task", "keyword rules", "restatement", "screen_display"} {
		if strings.Contains(testStewardPrompt(t), forbidden) {
			t.Fatalf("legacy/example instruction %q", forbidden)
		}
	}
	for _, required := range []string{"new:", "append:", "answer:", "continue:", "report:", "cancel:", "redisplay:", "clarify:", "empty task list does not imply new", "Ambiguity requires clarify", "Replies follow the request's language"} {
		if !strings.Contains(testStewardPrompt(t), required) {
			t.Fatalf("missing definition %q", required)
		}
	}
}

func TestChooseFailuresAndPrivateDiagnostics(t *testing.T) {
	previousLevel := logging.CurrentLevel()
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { logging.SetLevel(previousLevel) })
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	s := choiceServer(t, "private-model-output", nil)
	c := configured(t, s.URL, "200")
	got, err := c.Choose(context.Background(), Input{Request: "private-request"})
	if err == nil || got.Action != "" {
		t.Fatal("invalid output became work")
	}
	s.Close()
	got, err = c.Choose(context.Background(), Input{Request: "private-request"})
	if err == nil || got.Action != "" {
		t.Fatal("transport failure became work")
	}
	var disabled *Client
	if _, err := disabled.Choose(context.Background(), Input{Request: "request"}); err == nil {
		t.Fatal("disabled client accepted")
	}
	for _, private := range []string{"private-request", "private-model-output", s.URL} {
		if strings.Contains(logs.String(), private) || strings.Contains(err.Error(), private) {
			t.Fatal("private diagnostics")
		}
	}
	for _, field := range []string{"configured_ms=200", "effective_ms=", "elapsed_ms=", "error_kind="} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %s", field)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.SetHTTPClient(&http.Client{})
	if _, err := c.Choose(ctx, Input{Request: "request"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func testStewardPrompt(t *testing.T) string {
	t.Helper()
	p, err := prompts.Load("../../deploy/prompts")
	if err != nil {
		t.Fatal(err)
	}
	return p.Steward()
}
