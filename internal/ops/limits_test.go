package ops

import (
	"os"
	"path/filepath"
	"testing"
)

func setProviderTestLimits(t *testing.T) {
	t.Helper()
	path, err := filepath.Abs("../../deploy/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOE_LIMITS_FILE", path)
}

func TestLifecycleLimitsInstallKeepAndReject(t *testing.T) {
	release, root := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "limits.json")
	t.Setenv("ZOE_LIMITS_FILE", path)
	policy, err := os.ReadFile("../../deploy/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "limits.json"), policy, 0644); err != nil {
		t.Fatal(err)
	}
	o := LifecycleOptions{Release: release, Root: root}
	m := ReleaseManifest{Artifacts: []ReleaseArtifact{{Path: "limits.json"}}}
	if err := installLifecycleLimits(o, m); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(policy) {
		t.Fatal("limits not installed", err)
	}
	if err := os.WriteFile(path, append([]byte("\n"), policy...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installLifecycleLimits(o, m); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); len(data) != len(policy)+1 {
		t.Fatal("operator limits replaced")
	}
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installLifecycleLimits(o, m); err == nil {
		t.Fatal("invalid host limits accepted")
	}
}
