package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/voice"
)

func TestMaintenanceMenuListsVoiceInstruction(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("0\n"), &out, &errOut, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "19) Configure voice instruction") {
		t.Fatal("voice instruction menu item missing")
	}
}

func TestMaintenanceMenuVoiceInstructionShowsEffectiveDefaultAndCustom(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("19\ncustom rule\n19\n\n0\n"), &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Current voice instruction (default):\n" + voice.DefaultInstructionText, "Current voice instruction (custom):\ncustom rule"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatal("effective instruction not shown")
		}
	}
	if !strings.Contains(out.String(), "Sent verbatim as instructions in each gateway create-session request; no prefix.") {
		t.Fatal("create-session delivery help missing")
	}
}

func TestMaintenanceMenuVoiceInstructionWritesAndRestores(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	path := filepath.Join(root, "zoe-stack.env")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("19\ncustom instruction $HOME\n0\n"), &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "19) Configure voice instruction") {
		t.Fatal("voice instruction menu item missing")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "ZOE_VOICE_INSTRUCTION_TEXT=") {
		t.Fatalf("menu did not write instruction: %v", err)
	}
	for _, text := range []string{"off", ""} {
		out.Reset()
		if err := menuWithRoot(strings.NewReader("19\n"+text+"\n0\n"), &out, &errOut, root); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if text == "off" && !strings.Contains(string(data), `ZOE_VOICE_INSTRUCTION_TEXT="off"`) {
			t.Fatal("off not saved")
		}
		if text == "" && strings.Contains(string(data), "ZOE_VOICE_INSTRUCTION_TEXT=") {
			t.Fatal("default did not remove key")
		}
	}
	if !strings.Contains(out.String(), "off (disabled)") || !strings.Contains(out.String(), "Apply with: zoe update") {
		t.Fatal("menu did not show effective disabled state or apply guidance")
	}
}

func TestMaintenanceMenuVoiceInstructionRejectsInvalid(t *testing.T) {
	menuManager(t)
	for _, text := range []string{string([]byte{255}), strings.Repeat("x", 6001)} {
		root := t.TempDir()
		var out, errOut bytes.Buffer
		if err := menuWithRoot(strings.NewReader("19\n"+text+"\n0\n"), &out, &errOut, root); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut.String(), "nothing saved") {
			t.Fatal("invalid input not rejected")
		}
		if _, err := os.Stat(filepath.Join(root, "zoe-stack.env")); !os.IsNotExist(err) {
			t.Fatal("invalid input saved configuration")
		}
	}
}
