package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerMenuReasoningEffortOrderRetryAndCurrent(t *testing.T) {
	fakeServices(t, false)
	root, _ := workerMenuFixture(t, 200)
	var out, errOut bytes.Buffer
	err := menuWorker(ConfigureMenuIO{Root: root, In: strings.NewReader("\n\ninvalid\nnone\n\nn\n"), Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(errOut.String(), "Nothing was saved.") != 1 || strings.Count(out.String(), "Reasoning effort (") != 2 {
		t.Fatalf("retry output=%s errors=%s", &out, &errOut)
	}
	last := -1
	for _, label := range []string{"Base URL [", "Model [", "Reasoning effort (none/low/medium/high/max; empty keeps default)", "Key file (", "API key (", "Test now?"} {
		at := strings.Index(out.String(), label)
		if at <= last {
			t.Fatalf("wrong prompt order at %s", label)
		}
		last = at
	}
	_, cfg, err := menuProviderDeployment(context.Background(), root)
	if err != nil || cfg.ReasoningEffort != "none" {
		t.Fatalf("saved effort=%q err=%v", cfg.ReasoningEffort, err)
	}
	out.Reset()
	err = menuWorker(ConfigureMenuIO{Root: root, In: strings.NewReader("\n\n\n\nn\n"), Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return nil, nil }})
	if err != nil || !strings.Contains(out.String(), "empty keeps default) [none]:") || strings.Contains(out.String(), "Thinking") {
		t.Fatalf("current effort missing or Thinking shown: %v %s", err, &out)
	}
}

func TestWorkerMenuAPIKeyEOFCancelsWithoutSaving(t *testing.T) {
	fakeServices(t, false)
	root, _ := workerMenuFixture(t, 200)
	path := filepath.Join(root, "zoe-runner.env")
	before, _ := os.ReadFile(path)
	var out, errOut bytes.Buffer
	err := menuWorker(ConfigureMenuIO{Root: root, In: strings.NewReader("\n\nhigh\n\n"), Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return nil, io.EOF }})
	if !errors.Is(err, io.EOF) || !strings.Contains(out.String(), "API key (") || !strings.Contains(out.String(), "Cancelled. Nothing was saved.") {
		t.Fatalf("err=%v output=%s", err, &out)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled input changed configuration")
	}
}
