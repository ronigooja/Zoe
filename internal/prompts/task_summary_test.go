package prompts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskSummaryPromptRequired(t *testing.T) {
	dir := fixture(t)
	set, err := Load(dir)
	if err != nil || set.TaskSummary() != "value" {
		t.Fatalf("load error=%v", err)
	}
	path, size, ok := set.Metadata("task_summary")
	if !ok || size != 6 || path != filepath.Join(dir, "task_summary.txt") {
		t.Fatal("missing task summary metadata")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || err.Error() != "prompt_missing: task_summary" {
		t.Fatalf("missing prompt error=%v", err)
	}
}
