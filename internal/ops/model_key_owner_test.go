package ops

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func injectModelKeyChown(t *testing.T, fn func(string, int, int) error, root bool) {
	t.Helper()
	oldChown, oldUID := chownFile, effectiveUID
	chownFile = fn
	effectiveUID = func() int {
		if root {
			return 0
		}
		return 1000
	}
	t.Cleanup(func() { chownFile, effectiveUID = oldChown, oldUID })
}

func TestModelKeyFileIsWrittenOwnedByContainerUserMode0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("old"), 0400); err != nil {
		t.Fatal(err)
	}
	calls := 0
	injectModelKeyChown(t, func(tmp string, uid, gid int) error {
		calls++
		if uid != 65532 || gid != 65532 {
			t.Fatalf("owner=%d:%d", uid, gid)
		}
		if filepath.Dir(tmp) != filepath.Dir(p) || tmp == p {
			t.Fatal("not a same-directory temporary file")
		}
		old, err := os.ReadFile(p)
		if err != nil || string(old) != "old" {
			t.Fatalf("replaced before chown: %v", err)
		}
		return nil
	}, true)
	if err := WriteModelSecretAtomic(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0600 || calls != 1 {
		t.Fatalf("info=%v err=%v calls=%d", info, err, calls)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "new\n" {
		t.Fatalf("replace failed: %v", err)
	}
	t.Run("chown failure preserves old file", TestKeyWriteFailsClearlyAndKeepsOldFileWhenChownFails)
}

func TestMemoryKeyFileIsWrittenOwnedByContainerUser(t *testing.T) {
	for _, root := range []bool{true, false} {
		t.Run(map[bool]string{true: "root", false: "unprivileged"}[root], func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "memory-key")
			calls := 0
			injectModelKeyChown(t, func(_ string, uid, gid int) error {
				calls++
				if uid != 65532 || gid != 65532 {
					t.Fatalf("owner=%d:%d", uid, gid)
				}
				if !root {
					return os.ErrPermission
				}
				return nil
			}, root)
			err := writeMemorySecretAtomic(p, []byte("new"))
			if calls != 1 {
				t.Fatalf("ownership was silently skipped: calls=%d", calls)
			}
			if !root {
				if err == nil {
					t.Fatal("accepted unreadable key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("info=%v err=%v", info, err)
			}
		})
	}
}

func TestKeyWriteFailsClearlyAndKeepsOldFileWhenChownFails(t *testing.T) {
	for _, root := range []bool{true, false} {
		t.Run(map[bool]string{true: "root", false: "unprivileged"}[root], func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			injectModelKeyChown(t, func(string, int, int) error { return os.ErrPermission }, root)
			err := WriteModelSecretAtomic(p, []byte("new"))
			if err == nil || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "65532:65532") || !strings.Contains(err.Error(), "run as root") {
				t.Fatalf("unclear failure: %v", err)
			}
			b, readErr := os.ReadFile(p)
			if readErr != nil || string(b) != "old" {
				t.Fatalf("lost old key: %v", readErr)
			}
			entries, err := os.ReadDir(filepath.Dir(p))
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary file leak: %v", err)
			}
		})
	}
}

func TestCompactTestFailsEarlyWhenKeyFileUnreadableByServerUser(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("never-print-this-key"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(401) }))
	defer s.Close()
	_, err := TestMenuModel(context.Background(), ModelConfig{BaseURL: s.URL, Model: "m", SecretPath: p}, "Compact", s.Client())
	if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "not readable by server user 65532:65532") || !strings.Contains(err.Error(), "sudo chown") || strings.Contains(err.Error(), "never-print-this-key") || requests != 0 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}

func TestUpdateStopsBeforeServiceChangesWhenModelKeyUnreadable(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("never-print-this-key"), 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPACT_BASE_URL=https://gateway.example\nZOE_COMPACT_MODEL=m\nZOE_COMPACT_API_KEY_FILE="+key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	commands := 0
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { commands++; return nil, nil })
	var out bytes.Buffer
	err := m.Update(context.Background(), LifecycleOptions{Root: root, Out: &out})
	if err == nil || commands != 0 || !strings.Contains(out.String(), "WARNING:") || !strings.Contains(out.String(), key) || !strings.Contains(out.String(), "sudo chown") || strings.Contains(out.String()+err.Error(), "never-print-this-key") {
		t.Fatalf("err=%v commands=%d output=%s", err, commands, &out)
	}
}
