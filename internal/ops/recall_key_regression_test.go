package ops

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func recallOwnedKeyFixture(t *testing.T) {
	t.Helper()
	modelKeyReadableFixture(t)
	old := ModelKeyChown
	ModelKeyChown = func(_ string, uid, gid int) error {
		if uid != ContainerUID || gid != ContainerGID {
			t.Fatalf("owner=%d:%d", uid, gid)
		}
		return nil
	}
	t.Cleanup(func() { ModelKeyChown = old })
}

func TestRecallKeyWrittenOwned65532Mode0600(t *testing.T) {
	recallOwnedKeyFixture(t)
	for _, kind := range []string{"summary", "answer"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "secrets", kind+"-api-key")
			calls := 0
			old := ModelKeyChown
			ModelKeyChown = func(path string, uid, gid int) error {
				calls++
				if uid != 65532 || gid != 65532 {
					t.Errorf("owner=%d:%d", uid, gid)
				}
				return nil
			}
			defer func() { ModelKeyChown = old }()
			c := RecallModelConfig{BaseURL: "https://gateway.example/v1", Model: "m", SecretPath: path}
			if err := SaveRecallModel(context.Background(), Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}, kind, c, []byte("fixture")); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 || calls == 0 {
				t.Fatalf("mode=%v ownership calls=%d", info.Mode().Perm(), calls)
			}
		})
	}
}

type unreadableRecallKeyInfo struct{ os.FileInfo }

func (info unreadableRecallKeyInfo) Sys() any {
	st := *info.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid, st.Gid = 0, 0
	return &st
}
func unreadableRecallKeyFixture(t *testing.T) {
	t.Helper()
	old := ModelKeyStat
	ModelKeyStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		return unreadableRecallKeyInfo{info}, nil
	}
	t.Cleanup(func() { ModelKeyStat = old })
}

func TestRecallModelFailsBeforeRequestForUnreadableServerKey(t *testing.T) {
	unreadableRecallKeyFixture(t)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(401) }))
	defer s.Close()
	for _, kind := range []string{"summary", "answer"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := TestRecallModel(context.Background(), kind, RecallModelConfig{BaseURL: s.URL, Model: "m", SecretPath: path}, nil)
			if err == nil || !strings.Contains(err.Error(), "not readable by server user 65532:65532") || calls != 0 {
				t.Fatalf("err=%v requests=%d", err, calls)
			}
		})
	}
}

func TestRecallExistingKeyRejectedWhenUnreadableByServer(t *testing.T) {
	unreadableRecallKeyFixture(t)
	for _, kind := range []string{"summary", "answer"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "key")
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			d := Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}
			err := SaveRecallModel(context.Background(), d, kind, RecallModelConfig{BaseURL: "https://gateway.example", Model: "m", SecretPath: path}, nil)
			if err == nil || !strings.Contains(err.Error(), "not readable by server user") {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(d.StackEnv); !os.IsNotExist(err) {
				t.Fatal("configuration saved despite unreadable key")
			}
		})
	}
}

func TestRecallConfigEffortSetRejectsMinimal(t *testing.T) {
	for _, kind := range []string{"summary", "answer"} {
		for _, effort := range []string{"", "none", "low", "medium", "high", "max", "minimal"} {
			c := RecallModelConfig{BaseURL: "https://gateway.example", Model: "m", SecretPath: "/key", ReasoningEffort: effort}
			_, err := c.Normalized(kind)
			if (err != nil) != (effort == "minimal") {
				t.Errorf("kind=%s effort=%s err=%v", kind, effort, err)
			}
		}
	}
}

func TestUpdateWarnsForUnreadableSummaryAndAnswerKeysBeforeServices(t *testing.T) {
	unreadableRecallKeyFixture(t)
	root := t.TempDir()
	d := Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}
	for _, kind := range []string{"summary", "answer"} {
		path := filepath.Join(root, kind+"-key")
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := WriteRecallModelEnvAtomic(d.StackEnv, kind, RecallModelConfig{BaseURL: "https://gateway.example", Model: "m", SecretPath: path}, false); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	calls := 0
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil })
	err := m.Update(context.Background(), LifecycleOptions{Root: root, Out: &out})
	if err == nil || !strings.Contains(err.Error(), "update stopped before service changes") || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	for _, kind := range []string{"summary", "answer"} {
		if !strings.Contains(out.String(), kind+"-key") {
			t.Errorf("missing %s warning", kind)
		}
	}
}
