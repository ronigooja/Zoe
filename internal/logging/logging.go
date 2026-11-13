// Package logging provides process-wide leveled diagnostics through the standard logger.
package logging

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

type Level int32

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var threshold atomic.Int32

func ParseLevel(value string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "debug":
		return Debug, true
	case "info":
		return Info, true
	case "warn":
		return Warn, true
	case "error":
		return Error, true
	default:
		return Debug, false
	}
}

// ConfigureFromEnv applies at process startup. Invalid values are never echoed.
func ConfigureFromEnv() {
	level, valid := ParseLevel(os.Getenv("ZOE_LOG_LEVEL"))
	SetLevel(level)
	if !valid {
		Warnf("logging status=invalid_level default=debug error_kind=config")
	}
}

func SetLevel(level Level)     { threshold.Store(int32(level)) }
func CurrentLevel() Level      { return Level(threshold.Load()) }
func Enabled(level Level) bool { return level >= CurrentLevel() }

func printf(level Level, format string, args ...any) {
	if Enabled(level) {
		log.Printf("level="+[]string{"debug", "info", "warn", "error"}[level]+" "+format, args...)
	}
}

func Debugf(format string, args ...any) { printf(Debug, format, args...) }
func Infof(format string, args ...any)  { printf(Info, format, args...) }
func Warnf(format string, args ...any)  { printf(Warn, format, args...) }
func Errorf(format string, args ...any) { printf(Error, format, args...) }

func UserHash(user string) string {
	hash := sha256.Sum256([]byte(user))
	return fmt.Sprintf("%x", hash[:4])
}

type userHashKey struct{}

// WithUserHash applies when callers already know the owner's non-identifying hash.
func WithUserHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, userHashKey{}, hash)
}

// ModelCall holds logging-only identifiers; it never holds clients or credentials.
type ModelCall struct {
	component, id, parent, hash string
	attempt                     int
}

func NewModelCall(component, hash string, attempt int, parent string) ModelCall {
	return ModelCall{component, uuid.NewString(), parent, hash, attempt}
}
func ModelCallFromContext(ctx context.Context, component string) ModelCall {
	hash, _ := ctx.Value(userHashKey{}).(string)
	return NewModelCall(component, hash, 1, "")
}
func (c ModelCall) ID() string { return c.id }
func (c ModelCall) fields() string {
	if c.parent == "" {
		return fmt.Sprintf("component=%s call_id=%s attempt=%d user_hash=%q", c.component, c.id, c.attempt, c.hash)
	}
	return fmt.Sprintf("component=%s call_id=%s attempt=%d parent_call_id=%q user_hash=%q", c.component, c.id, c.attempt, c.parent, c.hash)
}
func (c ModelCall) Input(instructions, input string) {
	Debugf("model_call_input %s instructions=%q input=%q", c.fields(), instructions, input)
}
func (c ModelCall) Output(output string) {
	Debugf("model_call_output %s output=%q", c.fields(), output)
}

// Result accepts parsed call-site data and safe error kinds, never raw errors.
func (c ModelCall) Result(result any, kind string) {
	Debugf("model_call_result %s result=%q error_kind=%s", c.fields(), fmt.Sprintf("%+v", result), kind)
}
