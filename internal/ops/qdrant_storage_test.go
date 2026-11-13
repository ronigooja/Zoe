package ops

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

func TestQdrantComposeDurableStorage(t *testing.T) {
	for _, file := range []string{"../../deploy/linux/docker-compose.yml", "../../services/memory/docker-compose.yml"} {
		t.Run(file, func(t *testing.T) {
			path, err := filepath.Abs(file)
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			env := types.Mapping{}
			for _, match := range regexp.MustCompile(`\$\{([A-Z_0-9]+):\?`).FindAllStringSubmatch(string(content), -1) {
				env[match[1]] = "fixture"
			}
			env["ZOE_MEMORY_DATA_ROOT"] = "/var/lib/zoe/memory"
			project, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
				WorkingDir: filepath.Dir(path), Environment: env,
				ConfigFiles: []types.ConfigFile{{Filename: path, Content: content}},
			}, func(o *loader.Options) { o.SetProjectName("zoe", true) })
			if err != nil {
				t.Fatal(err)
			}
			qdrant := project.Services["qdrant"]
			if strings.Contains(file, "deploy/linux") {
				if qdrant.User != "1000:1000" || !qdrant.ReadOnly || int64(qdrant.MemLimit) != 2*1024*1024*1024 || len(qdrant.Tmpfs) != 1 || !strings.HasPrefix(qdrant.Tmpfs[0], "/tmp:") {
					t.Fatal("production Qdrant hardening or memory limit changed")
				}
			}
			if len(qdrant.Volumes) != 1 {
				t.Fatalf("storage mounts: %+v", qdrant.Volumes)
			}
			mount := qdrant.Volumes[0]
			if mount.Type != "bind" || mount.Source != "/var/lib/zoe/memory/qdrant" || mount.Target != "/qdrant/storage" || mount.Bind == nil || mount.Bind.CreateHostPath {
				t.Fatalf("storage bind: %+v", mount)
			}
			for _, tmpfs := range qdrant.Tmpfs {
				if strings.HasPrefix(tmpfs, "/qdrant/storage") {
					t.Fatalf("ephemeral storage: %s", tmpfs)
				}
			}
			if qdrant.Environment["QDRANT__STORAGE__SNAPSHOTS_PATH"] == nil || *qdrant.Environment["QDRANT__STORAGE__SNAPSHOTS_PATH"] != "/qdrant/storage/snapshots" {
				t.Fatal("snapshots must remain inside durable storage")
			}
		})
	}
}
