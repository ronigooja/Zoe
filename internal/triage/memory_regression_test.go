package triage

import (
	"strings"
	"testing"
)

func TestMemorySplitRegression(t *testing.T) {
	for _, word := range []string{"memory_read", "memory_write"} {
		if !strings.Contains(instructions, word+": ") {
			t.Errorf("missing definition: %s", word)
		}
		if got, err := parseCategory(word); err != nil || string(got) != word {
			t.Errorf("new category rejected: %s", word)
		}
	}
	if got, err := parseCategory("memory"); err == nil || got != "" {
		t.Error("legacy memory accepted")
	}
}
