package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

type unreadableRecallKeyInfo struct{ os.FileInfo }

func (info unreadableRecallKeyInfo) Sys() any {
	st := *info.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid, st.Gid = 0, 0
	return &st
}

func TestStatusWarnsForUnreadableSummaryAndAnswerKeys(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	d := ops.Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}
	old := ops.ModelKeyStat
	ops.ModelKeyStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		return unreadableRecallKeyInfo{info}, nil
	}
	t.Cleanup(func() { ops.ModelKeyStat = old })
	for _, kind := range []string{"summary", "answer"} {
		path := filepath.Join(root, kind+"-key")
		if err := os.WriteFile(path, []byte(kind+"-private"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ops.WriteRecallModelEnvAtomic(d.StackEnv, kind, ops.RecallModelConfig{BaseURL: "https://gateway.example", Model: "m", SecretPath: path}, false); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"summary", "answer"} {
		if !strings.Contains(out.String(), kind+"-key") || !strings.Contains(out.String(), "WARNING:") {
			t.Errorf("missing %s warning", kind)
		}
	}
	if strings.Contains(out.String()+errOut.String(), "-private") {
		t.Fatal("secret leaked")
	}
}
