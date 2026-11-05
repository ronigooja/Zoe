package ops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePrivateFileUsesContainerOwnerAndPrivateMode(t *testing.T) {
	oldChown := ModelKeyChown
	var owners [][2]int
	ModelKeyChown = func(path string, uid, gid int) error {
		owners = append(owners, [2]int{uid, gid})
		return nil
	}
	t.Cleanup(func() { ModelKeyChown = oldChown })

	path := filepath.Join(t.TempDir(), "voice-key")
	if err := WritePrivateFile(path, []byte("voice-secret\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("voice secret mode = %v (%v)", info, err)
	}
	if len(owners) != 1 || owners[0] != [2]int{memoryWorkerUID, memoryWorkerGID} {
		t.Fatalf("voice secret owners = %v", owners)
	}
}
