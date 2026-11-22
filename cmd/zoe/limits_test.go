package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestLimits(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile("../../deploy/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "limits.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}
