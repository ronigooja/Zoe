package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestParseCommandSupportsNestedOperations(t *testing.T) {
	name, args, err := parseCommand([]string{"provider", "test", "--json"})
	if err != nil || name != "provider test" || len(args) != 1 || args[0] != "--json" {
		t.Fatalf("parseCommand() = %q, %#v, %v", name, args, err)
	}
}

func TestMenuDispatchesSelectedOperationAndExits(t *testing.T) {
	old := operations["status"]
	t.Cleanup(func() { operations["status"] = old })
	var called bool
	operations["status"] = func(args []string, _ io.Reader, out, _ io.Writer) error {
		called = true
		if len(args) != 0 {
			t.Errorf("menu passed unexpected args: %#v", args)
		}
		_, _ = io.WriteString(out, "ok")
		return nil
	}
	var out, errOut bytes.Buffer
	if err := menu(strings.NewReader("1\n\n0\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(out.String(), "ok") || errOut.Len() != 0 {
		t.Fatalf("menu output/call mismatch: called=%v out=%q err=%q", called, out.String(), errOut.String())
	}
}

func TestMenuSharesInputAcrossPromptsAndActions(t *testing.T) {
	old := operations["setup"]
	t.Cleanup(func() { operations["setup"] = old })
	calls := 0
	operations["setup"] = func(_ []string, in io.Reader, out, _ io.Writer) error {
		calls++
		url, err := inputLine(in)
		if err != nil {
			return err
		}
		model, err := inputLine(in)
		if err != nil {
			return err
		}
		token, err := readToken(in, out)
		if err != nil {
			return err
		}
		if url != "https://provider/v1" || model != "model" || string(token) != "private" {
			t.Errorf("prompt input lost or reordered")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	if err := menu(strings.NewReader("2\nhttps://provider/v1\nmodel\nprivate\n\n2\nhttps://provider/v1\nmodel\nprivate\n\n0\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || errOut.Len() != 0 {
		t.Fatalf("calls=%d diagnostics=%s", calls, &errOut)
	}
	if strings.Contains(out.String(), "private") || strings.Contains(out.String(), "hidden") {
		t.Fatal("prompt reveals token or claims hidden input")
	}
	if !strings.Contains(out.String(), "API token: ") {
		t.Fatalf("token prompt missing: %q", out.String())
	}
}

func TestMenuUsesGroupedTwoDigitLabels(t *testing.T) {
	old := operations["key rotate"]
	t.Cleanup(func() { operations["key rotate"] = old })
	called := false
	operations["key rotate"] = func(_ []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		called = true
		return nil
	}
	var out, errOut bytes.Buffer
	if err := menu(strings.NewReader("21\n\n0\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Overview:", "1) Show status", "First-time setup", "System:", "3) Install host", "4) Update host", "5) Uninstall host", "Models:", "14) Configure Memory extraction model", "10) Configure Compact model", "11) Test Compact model", "12) Configure Steward model", "13) Test Steward model", "24) Configure UI_Controller model", "25) Test UI_Controller model", "Voice:", "19) Configure voice instruction", "Access:", "20) Create instance key", "21) Rotate instance key", "22) Show client policy", "23) Set client policy"} {
		if !strings.Contains(out.String(), label) {
			t.Fatalf("menu missing %q: %s", label, out.String())
		}
	}
	if !called || errOut.Len() != 0 {
		t.Fatalf("two-digit dispatch failed: called=%v err=%q", called, errOut.String())
	}
}

func TestReadTokenUsesSharedLineReaderForFileInput(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("secret-value\nnext-prompt\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out bytes.Buffer
	in := sharedInput(file)
	token, err := readToken(in, &out)
	if err != nil || string(token) != "secret-value" {
		t.Fatal("token input failed")
	}
	if next, err := inputLine(in); err != nil || next != "next-prompt" {
		t.Fatal("secret reader consumed the next prompt")
	}
	if out.String() != "API token: " {
		t.Fatal("unexpected token prompt or secret written to output")
	}
}

func TestReadSecretInputPreservesEOF(t *testing.T) {
	if _, err := readSecretInput(strings.NewReader(""), io.Discard); !errors.Is(err, io.EOF) {
		t.Fatal("secret reader must preserve cancellation by EOF")
	}
	if _, err := readToken(strings.NewReader(""), io.Discard); !errors.Is(err, io.EOF) {
		t.Fatal("token reader must preserve cancellation by EOF")
	}
}

func TestMenuSecretAllowsEmptyWithoutDuplicateTokenPrompt(t *testing.T) {
	var out bytes.Buffer
	m := configureMenuIO(t.TempDir(), strings.NewReader("\n"), &out, io.Discard)
	secret, err := m.PromptSecret(&out)
	if err != nil || len(secret) != 0 || strings.Contains(out.String(), "API token:") {
		t.Fatalf("empty menu secret rejected or duplicate prompt: %v %s", err, &out)
	}
	if _, err := readToken(strings.NewReader("\n"), io.Discard); err == nil {
		t.Fatal("scripted token input must still require a token")
	}
}
