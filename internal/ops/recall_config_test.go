package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnswerConfigurePreservesLegacyEnv(t *testing.T) {
	recallOwnedKeyFixture(t)
	root := t.TempDir()
	d := Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}
	legacy := "ZOE_SUMMARY_TIMEOUT_MS=garbage\nZOE_SUMMARY_API_KEY_FILE=/retired\n"
	if err := os.WriteFile(d.StackEnv, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	c := RecallModelConfig{BaseURL: "https://answer.example/v1", Model: "answer-model", SecretPath: filepath.Join(root, "secrets", "answer-api-key"), TimeoutMS: 1000}
	if err := SaveRecallModel(context.Background(), d, "answer", c, []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	got, err := d.AnswerConfig()
	if err != nil || got != c {
		t.Fatalf("config=%+v err=%v", got, err)
	}
	data, err := os.ReadFile(d.StackEnv)
	if err != nil || !strings.Contains(string(data), legacy) {
		t.Fatal("legacy settings changed")
	}
	if _, err := d.RecallModelConfig("summary"); err == nil {
		t.Fatal("retired summary model accepted")
	}
	if err := WriteRecallModelEnvAtomic(d.StackEnv, "answer", c, true); err != nil {
		t.Fatal(err)
	}
	got, err = d.AnswerConfig()
	if err != nil || got.BaseURL != "" {
		t.Fatal("answer disable failed")
	}
}

func TestAnswerTimeoutRanges(t *testing.T) {
	for _, ms := range []int{0, 500, 30000, 499, 30001, -1} {
		c := RecallModelConfig{BaseURL: "https://answer.example/v1", Model: "m", SecretPath: "/key", TimeoutMS: ms}
		_, err := c.Normalized("answer")
		valid := ms == 0 || (ms >= 500 && ms <= 30000)
		if (err == nil) != valid {
			t.Errorf("timeout=%d err=%v", ms, err)
		}
	}
}
