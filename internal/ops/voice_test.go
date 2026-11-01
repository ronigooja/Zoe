package ops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePrivateFileUsesContainerOwnerAndPrivateMode(t *testing.T) {
	oldEUID, oldChown := hostPrepEUID, hostPrepChown
	var owners [][2]int
	hostPrepEUID = func() int { return 0 }
	hostPrepChown = func(path string, uid, gid int) error {
		owners = append(owners, [2]int{uid, gid})
		return nil
	}
	t.Cleanup(func() { hostPrepEUID, hostPrepChown = oldEUID, oldChown })

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
