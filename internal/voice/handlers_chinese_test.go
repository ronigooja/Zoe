package voice

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/steward"
)

func TestHandlersPlainChineseSpeech(t *testing.T) {
	for _, tc := range []struct {
		name, category, content, want string
		ask                           bool
	}{
		{"recall", "recall", "what did we discuss", "我查一下我们之前的对话。", false},
		{"memory_read", "memory_read", "what do you remember", "我查一下保存的笔记。", false},
		{"steward", "task", "handle this", "要我现在去处理这个请求吗？", true},
		{"confirmation", "task", "delete the project", "要我现在去处理这个请求吗？", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Classifier: &fakeClassifier{category: tc.category}}
			if tc.ask {
				cfg.Steward = &fakeSteward{decision: steward.Ask}
			}
			ts, store, key := handoffHTTPServerWithConfig(t, cfg)
			commitAndHandoff(t, ts, key, tc.name, tc.content)
			var got string
			if err := store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id=?`, tc.name).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want || strings.Contains(got, "Read this to the user") || strings.HasPrefix(got, recallPrefix) || regexp.MustCompile(`[A-Za-z]`).MatchString(got) {
				t.Fatalf("upstream speech=%q, want plain Chinese %q", got, tc.want)
			}
		})
	}
}

func TestHandlersMemorySaveHasNoPrefix(t *testing.T) {
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: &fakeClassifier{category: "memory_write"}})
	commitAndHandoff(t, ts, key, "save", "remember my favorite drink")
	var got string
	if err := store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id='save'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	// The memory package owns the sentence; the handler must not add instructions.
	if got != memorySaveUnavailable || strings.Contains(got, "Read this to the user") || strings.HasPrefix(got, recallPrefix) {
		t.Fatalf("memory save speech was prefixed: %q", got)
	}
}
