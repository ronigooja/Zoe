package uictl

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/prompts"
)

func TestShippedExamples(t *testing.T) {
	p, err := prompts.Load("../../deploy/prompts")
	if err != nil {
		t.Fatal(err)
	}
	sections, err := prompts.UISections(p.UIController())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{Show, TaskQuestion, ChatInput} {
		examples := regexp.MustCompile(`(?s)<ui-meta>.*?</a2ui-json>`).FindAllString(sections[string(mode)], -1)
		if len(examples) != 1 {
			t.Fatalf("%s: expected one complete example", mode)
		}
		o, err := Parse(examples[0], nil, mode)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if _, err := Apply(o); err != nil {
			t.Fatal(err)
		}
		if mode != Show {
			withFeedback := strings.Replace(examples[0], `"container":`, `"feedback":"ignored","container":`, 1)
			parsed, err := Parse(withFeedback, nil, mode)
			if err != nil || parsed.Feedback != "" {
				t.Fatal("input feedback not cleared", err)
			}
			withMode := strings.Replace(examples[0], `"container":`, `"task_question":true,"container":`, 1)
			if _, err := Parse(withMode, nil, mode); err == nil {
				t.Fatal("model selected input semantics")
			}
			bad := strings.Replace(examples[0], `"value":{"path":`, `"value":{"not_path":`, 1)
			if _, err := Parse(bad, nil, mode); err == nil {
				t.Fatal("unbound input accepted")
			}
		}
	}
}

func TestBlockEdgeSpacingIsIgnored(t *testing.T) {
	valid := fixture(create + "," + root)
	for _, edge := range []string{"\u200a", "\u200b", "\u00a0", "\n"} {
		text := strings.Replace(valid, "</ui-meta>", edge+"</ui-meta>", 1)
		text = strings.Replace(text, "</a2ui-json>", edge+"</a2ui-json>", 1)
		if _, err := Parse(text, nil, Show); err != nil {
			t.Fatalf("edge %q rejected: %v", edge, err)
		}
	}
	inside := strings.Replace(valid, `"container":`, "\u200b"+`"container":`, 1)
	if _, err := Parse(inside, nil, Show); err == nil {
		t.Fatal("character inside the JSON was accepted")
	}
}

func TestModelCannotSelectInputMode(t *testing.T) {
	valid := fixture(create + "," + root)
	for _, key := range []string{"awaiting_input", "voice_input", "task_question", "chat_input"} {
		text := strings.Replace(valid, `"container":`, `"`+key+`":false,"container":`, 1)
		if _, err := Parse(text, nil, Show); err == nil {
			t.Fatalf("accepted model field %s", key)
		}
	}
}
