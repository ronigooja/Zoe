package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderUnitUsesConfiguredOpenCodePath(t *testing.T) {
	unit := []byte("ExecStart=/root/.opencode/bin/opencode serve\n")
	got := string(bytesReplacePaths(unit, LifecycleOptions{OpenCodeBinary: "/opt/zoe/opencode/v2.2.5-ronigooja.2/opencode"}))
	if !strings.Contains(got, "ExecStart=/opt/zoe/opencode/v2.2.5-ronigooja.2/opencode serve") {
		t.Fatalf("rendered unit = %q", got)
	}
}

func TestInstallOpenCodeDownloadsAndVerifiesLocally(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	binary := []byte("fake opencode")
	if err := tw.WriteHeader(&tar.Header{Name: "opencode", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(archive.Bytes())
	asset := OpenCodeAsset{Name: "opencode-linux-x64.tar.gz", SHA256: hex.EncodeToString(h[:])}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			_, _ = w.Write([]byte(asset.SHA256 + "  " + asset.Name + "\n"))
		case asset.Name:
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previous := openCodeDownloadBaseURL
	openCodeDownloadBaseURL = server.URL
	t.Cleanup(func() { openCodeDownloadBaseURL = previous })
	release := OpenCodeRelease{Repository: "test/repo", Tag: "vtest", Assets: map[string]OpenCodeAsset{"amd64": asset}}
	root := t.TempDir()
	if err := installOpenCode(context.Background(), root, release, asset); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "opencode", release.Tag, "opencode")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(binary) {
		t.Fatalf("installed binary = %q, error = %v", data, err)
	}
	if mode := dataMode(t, path); mode != 0755 {
		t.Fatalf("mode = %04o, want 0755", mode)
	}
}
