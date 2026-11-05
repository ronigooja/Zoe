package ops

import (
	"os"
	"syscall"
	"testing"
)

type containerOwnedModelKeyInfo struct{ os.FileInfo }

func (info containerOwnedModelKeyInfo) Sys() any {
	st := *info.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid, st.Gid = ContainerUID, ContainerGID
	return &st
}

// modelKeyReadableFixture applies only to successful endpoint fixtures, not
// ownership/readability regression tests, which exercise real metadata.
func modelKeyReadableFixture(t *testing.T) {
	t.Helper()
	old := ModelKeyStat
	ModelKeyStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		return containerOwnedModelKeyInfo{info}, nil
	}
	t.Cleanup(func() { ModelKeyStat = old })
}
