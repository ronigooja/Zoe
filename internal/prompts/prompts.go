// Package prompts loads the operator-maintained model instructions at startup.
package prompts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const DefaultDir = "/etc/zoe/prompts"
const MaxBytes = 16000

var names = [...]string{"ui_controller", "register", "compact", "steward", "task_ui_display", "voice_instruction"}

// Names returns the required prompt names in startup order.
func Names() []string { return append([]string(nil), names[:]...) }

// Dir returns the configured directory, or the default if unset.
func Dir() string {
	if dir := os.Getenv("ZOE_PROMPT_DIR"); dir != "" {
		return dir
	}
	return DefaultDir
}

// Error contains only a fixed category and the name of the affected prompt.
type Error struct{ Category, Name string }

func (e *Error) Error() string { return e.Category + ": " + e.Name }

type entry struct {
	text string
	path string
	size int
}

// Set is an immutable snapshot of all required prompts.
type Set struct{ entries map[string]entry }

// Load reads every required prompt once. The size limit applies to file bytes,
// before the optional final newline is removed.
func Load(dir string) (*Set, error) {
	s := &Set{entries: make(map[string]entry, len(names))}
	for _, name := range names {
		path := filepath.Join(dir, name+".txt")
		f, err := os.Open(path)
		if err != nil {
			category := "prompt_unreadable"
			if errors.Is(err, os.ErrNotExist) {
				category = "prompt_missing"
			}
			return nil, &Error{category, name}
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, &Error{"prompt_unreadable", name}
		}
		if len(data) > MaxBytes {
			return nil, &Error{"prompt_too_large", name}
		}
		if !utf8.Valid(data) {
			return nil, &Error{"prompt_invalid_utf8", name}
		}
		text := strings.TrimSuffix(string(data), "\n")
		if text == "" {
			return nil, &Error{"prompt_empty", name}
		}
		s.entries[name] = entry{text: text, path: path, size: len(data)}
	}
	return s, nil
}

func (s *Set) UIController() string  { return s.entries["ui_controller"].text }
func (s *Set) Register() string      { return s.entries["register"].text }
func (s *Set) Compact() string       { return s.entries["compact"].text }
func (s *Set) Steward() string       { return s.entries["steward"].text }
func (s *Set) TaskUIDisplay() string { return s.entries["task_ui_display"].text }
func (s *Set) VoiceInstruction() string {
	text := s.entries["voice_instruction"].text
	if text == "off" {
		return ""
	}
	return text
}

// Metadata returns the path and on-disk byte size without exposing content.
func (s *Set) Metadata(name string) (path string, size int, ok bool) {
	e, ok := s.entries[name]
	return e.path, e.size, ok
}
