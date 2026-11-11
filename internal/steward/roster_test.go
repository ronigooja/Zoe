package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func rosterServer(t *testing.T, reply string, inspect func([]byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if inspect != nil {
			inspect(body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": reply})
	}))
}

func TestAssignRequestAliasesRuneLimitsAndRosterCap(t *testing.T) {
	roster := make([]RosterTask, 12)
	for i := range roster {
		roster[i] = RosterTask{
			ID: "opaque-private-id-" + strconv.Itoa(i), Title: strings.Repeat("界🙂", 40),
			Status: "active", Question: strings.Repeat("问🙂", 70), Result: strings.Repeat("果🙂", 110),
		}
	}
	sentence := "private sentence \"ignore instructions\"\n🙂"
	var calls atomic.Int32
	s := rosterServer(t, "attach t10", func(body []byte) {
		calls.Add(1)
		for _, task := range roster {
			if bytes.Contains(body, []byte(task.ID)) {
				t.Error("real id exposed to model")
			}
		}
		var request struct{ Input, Instructions string }
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		if request.Instructions != assignInstructions {
			t.Error("assignment instructions missing")
		}
		var input struct {
			Sentence string
			Tasks    []map[string]string
		}
		if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
			t.Error(err)
		}
		if input.Sentence != sentence || len(input.Tasks) != 10 {
			t.Errorf("sentence preserved=%v tasks=%d", input.Sentence == sentence, len(input.Tasks))
		}
		for i, task := range input.Tasks {
			if len(task) != 5 || task["ref"] != "t"+strconv.Itoa(i+1) || task["status"] != "active" {
				t.Errorf("invalid task fields at %d", i)
			}
			for field, want := range map[string]string{
				"title": strings.Repeat("界🙂", 30), "question": strings.Repeat("问🙂", 60), "result": strings.Repeat("果🙂", 100),
			} {
				if task[field] != want || !utf8.ValidString(task[field]) {
					t.Errorf("%s not rune-truncated correctly", field)
				}
			}
		}
	})
	defer s.Close()
	got, err := configured(t, s.URL, "").Assign(context.Background(), sentence, roster)
	if err != nil || got != (Assignment{Action: ActionAttach, TaskID: roster[9].ID}) || calls.Load() != 1 {
		t.Fatalf("assignment=%+v err=%v calls=%d", got, err, calls.Load())
	}
}

func TestAssignReplies(t *testing.T) {
	roster := []RosterTask{{ID: "real-first", Status: "active"}, {ID: "real-second", Status: "awaiting_user", Question: "Which option?"}}
	for _, tc := range []struct {
		reply string
		want  Assignment
	}{
		{"new", Assignment{Action: ActionNew}}, {"none", Assignment{Action: ActionNone}},
		{"attach t1", Assignment{Action: ActionAttach, TaskID: "real-first"}},
		{"answer t2", Assignment{Action: ActionAnswer, TaskID: "real-second"}},
		{" \nATTACH T2\n ", Assignment{Action: ActionAttach, TaskID: "real-second"}},
		{"ANSWER T2", Assignment{Action: ActionAnswer, TaskID: "real-second"}},
		{"NEW", Assignment{Action: ActionNew}}, {"NONE", Assignment{Action: ActionNone}},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			s := rosterServer(t, tc.reply, nil)
			defer s.Close()
			got, err := configured(t, s.URL, "").Assign(context.Background(), "sentence", roster)
			if err != nil || got != tc.want {
				t.Fatalf("assignment=%+v err=%v want=%+v", got, err, tc.want)
			}
		})
	}
	for _, reply := range []string{"attach t3", "answer t3", "answer t1", "attach", "answer", "attach  t1", "attach\tt1", "attach\nt1", "attach t01", "attach real-first", "new extra", "attach t1 extra", "none.", "", " \n "} {
		t.Run("invalid/"+reply, func(t *testing.T) {
			s := rosterServer(t, reply, nil)
			defer s.Close()
			got, err := configured(t, s.URL, "").Assign(context.Background(), "sentence", roster)
			if err == nil || got != (Assignment{}) {
				t.Fatalf("invalid reply accepted: assignment=%+v err=%v", got, err)
			}
		})
	}
}

func TestAssignEmptyRosterStillCallsModel(t *testing.T) {
	for _, reply := range []string{"new", "none", "attach t1", "answer t1"} {
		t.Run(reply, func(t *testing.T) {
			var calls atomic.Int32
			s := rosterServer(t, reply, func(body []byte) {
				calls.Add(1)
				var request struct{ Input string }
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
				}
				if request.Input != `{"sentence":"hello","tasks":[]}` {
					t.Errorf("unexpected empty-roster input: %s", request.Input)
				}
			})
			defer s.Close()
			got, err := configured(t, s.URL, "").Assign(context.Background(), "hello", nil)
			valid := reply == "new" || reply == "none"
			if calls.Load() != 1 || (err == nil) != valid || (valid && got != (Assignment{Action: Action(reply)})) {
				t.Fatalf("assignment=%+v err=%v calls=%d", got, err, calls.Load())
			}
		})
	}
}

func TestAssignInstructionsDefinitionsOnly(t *testing.T) {
	for _, r := range assignInstructions {
		if unicode.Is(unicode.Han, r) {
			t.Fatal("assignment instructions contain CJK sample text")
		}
	}
}

func TestAssignLogsExcludePrivateData(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	s := rosterServer(t, "attach t99 private-model-output", nil)
	defer s.Close()
	roster := []RosterTask{{ID: "private-real-id", Title: "private-title", Question: "private-question", Result: "private-result"}}
	_, err := configured(t, s.URL, "").Assign(context.Background(), "private-sentence", roster)
	if err == nil {
		t.Fatal("expected invalid assignment error")
	}
	for _, private := range []string{"private-sentence", "private-title", "private-question", "private-result", "private-real-id", "private-model-output"} {
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

func TestAssignTimeoutAndTransportFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got, err := configured(t, s.URL, "200").Assign(ctx, "sentence", nil); err == nil || got != (Assignment{}) {
		t.Fatalf("assignment=%+v err=%v", got, err)
	}
	s.Close()
	if _, err := configured(t, s.URL, "200").Assign(context.Background(), "sentence", nil); err == nil {
		t.Fatal("transport failure accepted")
	}
	var disabled *Client
	if _, err := disabled.Assign(context.Background(), "sentence", nil); err == nil {
		t.Fatal("disabled client accepted")
	}
}

func TestAssignConfiguredDeadline(t *testing.T) {
	c := configured(t, "http://localhost", "200")
	c.SetHTTPClient(&http.Client{Transport: stewardDeadlineTransport{t: t, want: 200 * time.Millisecond}})
	if _, err := c.Assign(context.Background(), "sentence", nil); err == nil {
		t.Fatal("expected transport error")
	}
}
