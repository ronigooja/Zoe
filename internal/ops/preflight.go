package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// MinimumFreeBytes is deliberately conservative: image pulls and the first
// database files need room in addition to the release itself.
const MinimumFreeBytes uint64 = 10 * 1024 * 1024 * 1024

var (
	preflightEUID   = os.Geteuid
	preflightStatfs = func(path string) (uint64, error) {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			return 0, err
		}
		return stat.Bavail * uint64(stat.Bsize), nil
	}
	preflightReadFile = os.ReadFile
)

// Preflight checks are kept separate so callers and tests can run all host
// probes before any deployment file is created.
func (m Manager) preflight(ctx context.Context, o LifecycleOptions, manifest ReleaseManifest) []error {
	var problems []error
	if preflightEUID() != 0 {
		problems = append(problems, fmt.Errorf("must run as root (use sudo)"))
	}
	if runtime.GOOS != "linux" {
		problems = append(problems, fmt.Errorf("unsupported operating system %s; install on Linux", runtime.GOOS))
	}
	if (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") && manifest.Architecture != runtime.GOARCH {
		problems = append(problems, fmt.Errorf("release architecture %s does not match host %s", manifest.Architecture, runtime.GOARCH))
	}
	if _, err := m.Run(ctx, "systemctl", "--version"); err != nil {
		problems = append(problems, fmt.Errorf("systemd is required (install systemd)"))
	}
	if _, err := m.Run(ctx, "docker", "version"); err != nil {
		problems = append(problems, fmt.Errorf("Docker is required (install Docker Engine)"))
	}
	if _, err := m.Run(ctx, "docker", "compose", "version"); err != nil {
		problems = append(problems, fmt.Errorf("docker compose plugin is required (install the Compose plugin)"))
	}
	if _, err := m.Run(ctx, "docker", "info"); err != nil {
		problems = append(problems, fmt.Errorf("Docker is not running (start docker.service)"))
	}
	for _, root := range []string{o.OptRoot, o.DataRoot} {
		free, err := preflightStatfs(root)
		if err != nil {
			if !os.IsNotExist(err) {
				problems = append(problems, fmt.Errorf("cannot check free space under %s", root))
			}
			continue
		}
		if free < MinimumFreeBytes {
			problems = append(problems, fmt.Errorf("insufficient free space under %s (need at least %d GiB)", root, MinimumFreeBytes/(1024*1024*1024)))
		}
	}
	ports := composePorts(o, manifest)
	if out, err := m.Run(ctx, "ss", "-ltnH"); err == nil {
		for _, port := range ports {
			if portInUse(string(out), port) {
				problems = append(problems, fmt.Errorf("required port %d is already in use", port))
			}
		}
	} else if len(ports) > 0 {
		problems = append(problems, fmt.Errorf("cannot check required ports (install iproute2/ss)"))
	}
	return problems
}

func formatPreflightProblems(problems []error) error {
	if len(problems) == 0 {
		return nil
	}
	lines := make([]string, len(problems))
	for i, problem := range problems {
		lines[i] = problem.Error()
	}
	return fmt.Errorf("install preflight failed:\n%s", strings.Join(lines, "\n"))
}

func composePorts(o LifecycleOptions, _ ReleaseManifest) []int {
	path := filepath.Join(o.Release, "deploy-linux", "docker-compose.yml")
	b, err := preflightReadFile(path)
	if err != nil {
		return []int{stackServerPort(o)}
	}
	port := stackServerPort(o)
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "127.0.0.1:${ZOE_SERVER_PORT:-") {
			continue
		}
		value := line[strings.Index(line, "127.0.0.1:${ZOE_SERVER_PORT:-")+len("127.0.0.1:${ZOE_SERVER_PORT:-"):]
		if end := strings.Index(value, "}"); end >= 0 {
			if n, e := strconv.Atoi(value[:end]); e == nil {
				port = n
			}
		}
	}
	return []int{port}
}

func stackServerPort(o LifecycleOptions) int {
	if value := readEnvValue(filepath.Join(o.Root, "zoe-stack.env"), "ZOE_SERVER_PORT"); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port <= 65535 {
			return port
		}
	}
	return 8787
}

func portInUse(output string, port int) bool {
	needle := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 3 && strings.Contains(fields[3], needle) {
			return true
		}
	}
	return false
}
