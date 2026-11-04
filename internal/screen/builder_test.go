package screen

import (
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"strings"
	"testing"
	"time"
)

func TestBuildDocumentsForEmptyManyDecisionAndResult(t *testing.T) {
	task := contracts.Task{ID: "t1", Title: "Task", Status: "awaiting_user", InteractionState: "explicit_confirmation", InteractionRunID: "r1", InteractionActionDigest: "digest", InteractionExpectedVersion: 2}
	result := contracts.Task{ID: "t2", Title: "Result", Status: "complete", Result: "summary"}
	for _, tasks := range [][]contracts.Task{{}, {task}, {result}, {task, result}} {
		got, err := Build(tasks, nil, 7, time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != 7 || len(got.Surfaces) == 0 {
			t.Fatalf("bad result: %#v", got)
		}
	}
}
func TestBuildDeterministicAndRevisionMonotonic(t *testing.T) {
	a := contracts.Task{ID: "b", Title: "B", Status: "queued"}
	b := contracts.Task{ID: "a", Title: "A", Status: "queued"}
	x, _ := Build([]contracts.Task{a, b}, nil, 2, time.Unix(1, 0))
	y, _ := Build([]contracts.Task{b, a}, nil, 3, time.Unix(1, 0))
	if x.Surfaces[0].Document.Root.Children[0].Props["items"].([]any)[0].(map[string]any)["id"] != "a" {
		t.Fatal("unstable ordering")
	}
	if y.Revision <= x.Revision {
		t.Fatal("revision not monotonic")
	}
}

func TestBuildDoesNotExposeSecretsOrRawErrors(t *testing.T) {
	got, err := Build([]contracts.Task{{ID: "secret", Title: "safe", Status: "failed", Error: "provider secret raw failure", Result: "summary"}}, nil, 1, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "provider secret raw failure") || strings.Contains(string(b), "instance-key") {
		t.Fatalf("unsafe screen output: %s", b)
	}
}

func TestBuildStatusCountsOnlyActiveTasks(t *testing.T) {
	tasks := []contracts.Task{
		{ID: "active-1", Status: "queued"},
		{ID: "active-2", Status: "awaiting_user"},
		{ID: "complete", Status: "complete"},
		{ID: "failed", Status: "failed"},
	}
	got, err := Build(tasks, nil, 1, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	status := got.Surfaces[0].Document.Root.Children[1].Props["description"]
	if status != "2 active task(s)" {
		t.Fatalf("status=%q, want 2 active task(s)", status)
	}
}
