// Package prompts owns release-versioned model instructions and startup overrides.
package prompts

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

//go:embed defaults/*.txt
var defaults embed.FS

const MaxBytes = 64 << 10

// Default returns the exact release bytes; unknown names are programmer errors.
func Default(name string) string {
	b, err := defaults.ReadFile("defaults/" + name + ".txt")
	if err != nil {
		panic("unknown prompt name: " + name)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// Load applies only at startup, before workers start. Missing files use defaults.
// Diagnostics include names and reasons, never file contents or OS error text.
func Load(dir string, info, warn func(string, ...any)) (map[string]string, error) {
	entries, _ := defaults.ReadDir("defaults")
	loaded := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".txt")
		loaded[name] = Default(name)
	}
	if dir == "" {
		return loaded, nil
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("ZOE_PROMPT_DIR: not a readable regular directory")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ZOE_PROMPT_DIR: cannot read directory")
	}
	for _, file := range files {
		name := strings.TrimSuffix(file.Name(), ".txt")
		if _, known := loaded[name]; !known || file.Name() != name+".txt" {
			if warn != nil {
				warn("prompt_unknown file=%q", file.Name())
			}
			continue
		}
		path := filepath.Join(dir, file.Name())
		invalid := func(reason string) (map[string]string, error) {
			return nil, fmt.Errorf("prompt override %s: %s", file.Name(), reason)
		}
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() {
			return invalid("regular file required (symlinks forbidden)")
		}
		if st.Size() > MaxBytes {
			return invalid("exceeds 65536 bytes")
		}
		f, err := os.Open(path)
		if err != nil {
			return invalid("cannot open file")
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() {
			f.Close()
			return invalid("file changed while opening")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, MaxBytes+1))
		f.Close()
		if readErr != nil {
			return invalid("cannot read file")
		}
		if len(b) > MaxBytes {
			return invalid("exceeds 65536 bytes")
		}
		if !utf8.Valid(b) {
			return invalid("invalid UTF-8")
		}
		if strings.ContainsRune(string(b), 0) {
			return invalid("NUL forbidden")
		}
		if strings.TrimSpace(string(b)) == "" {
			return invalid("empty prompt")
		}
		loaded[name] = string(b)
		if info != nil {
			sum := sha256.Sum256(b)
			info("prompt_override name=%s sha256=%x bytes=%d", name, sum[:6], len(b))
		}
	}
	return loaded, nil
}
