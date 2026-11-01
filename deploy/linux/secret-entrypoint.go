// zoe-secret-entrypoint loads deployment-mounted private secret files into
// the environment and then replaces itself with the configured application.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

var allowedNames = map[string]bool{
	"ZOE_INSTANCE_KEY":          true,
	"ZOE_RUNNER_TOKEN":          true,
	"ZOE_VOICE_GATEWAY_API_KEY": true,
	"ZOE_MEM0_LLM_API_KEY":      true,
	"ZOE_QDRANT_API_KEY":        true,
	"ZOE_PROVIDER_KEY":          true,
	"OPENCODE_SERVER_PASSWORD":  true,
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zoe-secret-entrypoint:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("application command is required")
	}
	mapping := strings.TrimSpace(os.Getenv("ZOE_SECRET_FILE_MAP"))
	if err := os.Unsetenv("ZOE_SECRET_FILE_MAP"); err != nil {
		return err
	}
	if mapping != "" {
		for _, pair := range strings.Split(mapping, ",") {
			name, path, ok := strings.Cut(pair, "=")
			if !ok || !allowedNames[name] || strings.TrimSpace(path) != path || path == "" {
				return errors.New("deployment secret file mapping is invalid")
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
				return errors.New("deployment secret reference must be a private regular file")
			}
			value, err := os.ReadFile(path)
			if err != nil {
				return errors.New("deployment secret reference could not be read")
			}
			value = []byte(strings.TrimSuffix(string(value), "\n"))
			if len(value) == 0 || strings.ContainsAny(string(value), "\r\n") {
				return errors.New("deployment secret file must contain one non-empty line")
			}
			if err = os.Setenv(name, string(value)); err != nil {
				return errors.New("deployment secret could not be loaded")
			}
			for i := range value {
				value[i] = 0
			}
		}
	}
	path, err := execPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, os.Environ())
}

func execPath(command string) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	path := os.Getenv("PATH")
	for _, directory := range filepathList(path) {
		candidate := directory + "/" + command
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("application command was not found in PATH")
}

func filepathList(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, string(os.PathListSeparator))
}
