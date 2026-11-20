package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUISections(t *testing.T) {
	valid := "[common]\nshared\n[show]\nshow only\n[task_question]\nquestion only\n[chat_input]\nchat only"
	for _, bad := range []string{"plain", strings.Replace(valid, "[show]", "[missing]", 1), strings.Replace(valid, "show only", "", 1), valid + "\n[show]\nagain"} {
		if _, err := UISections(bad); err == nil || err.Error() != "prompt_sections_invalid: ui_controller" {
			t.Fatalf("unexpected error %v", err)
		}
	}
	for _, mode := range []string{"show", "task_question", "chat_input"} {
		instruction, err := UIInstruction(valid, mode)
		if err != nil || !strings.HasPrefix(instruction, "shared\n\n") || strings.Contains(instruction, "[") {
			t.Fatal(instruction, err)
		}
		for _, other := range []string{"show only", "question only", "chat only"} {
			expected := map[string]string{"show": "show only", "task_question": "question only", "chat_input": "chat only"}[mode]
			if strings.Contains(instruction, other) != (other == expected) {
				t.Fatal("inactive mode leaked", instruction)
			}
		}
	}
}

func TestLoadRejectsInvalidUISections(t *testing.T) {
	dir := fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "ui_controller.txt"), []byte("[common]\nonly common"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || err.Error() != "prompt_sections_invalid: ui_controller" {
		t.Fatalf("load error=%v", err)
	}
}
