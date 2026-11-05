package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

type containerOwnedKeyInfo struct{ os.FileInfo }

func (info containerOwnedKeyInfo) Sys() any {
	st := *info.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid, st.Gid = ops.ContainerUID, ops.ContainerGID
	return &st
}

// containerKeyFixture applies only to successful CLI fixtures that simulate
// deployment file ownership. Permission regression tests use the real defaults.
func containerKeyFixture(t *testing.T) {
	t.Helper()
	oldChown, oldStat := ops.ModelKeyChown, ops.ModelKeyStat
	ops.ModelKeyChown = func(_ string, uid, gid int) error {
		if uid != ops.ContainerUID || gid != ops.ContainerGID {
			t.Fatalf("unexpected key owner %d:%d", uid, gid)
		}
		return nil
	}
	ops.ModelKeyStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		return containerOwnedKeyInfo{info}, nil
	}
	t.Cleanup(func() { ops.ModelKeyChown, ops.ModelKeyStat = oldChown, oldStat })
}
