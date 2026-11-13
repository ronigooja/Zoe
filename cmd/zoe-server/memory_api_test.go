package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
)

func TestMemoryAPIConfigurationDisabled(t *testing.T) {
	oldOutput, oldLevel := log.Writer(), logging.CurrentLevel()
	t.Cleanup(func() { log.SetOutput(oldOutput); logging.SetLevel(oldLevel) })
	logging.SetLevel(logging.Info)
	for _, mode := range []string{"unset", "partial", "invalid", "configured"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ZOE_MEMORY_API_URL", "")
			t.Setenv("ZOE_MEMORY_API_TOKEN_FILE", "")
			if mode != "unset" {
				t.Setenv("ZOE_MEMORY_API_URL", "http://worker:8788")
			}
			if mode == "invalid" || mode == "configured" {
				t.Setenv("ZOE_MEMORY_API_TOKEN_FILE", "/private-token-reference")
			}
			if mode == "invalid" {
				t.Setenv("ZOE_MEMORY_API_URL", "invalid")
			}
			var output bytes.Buffer
			log.SetOutput(&output)
			client := memoryAPIFromEnv()
			if (client != nil) != (mode == "configured") {
				t.Fatal("configuration state")
			}
			if mode == "unset" && !strings.Contains(output.String(), "memory status=disabled reason=not_configured missing=ZOE_MEMORY_API_URL,ZOE_MEMORY_API_TOKEN_FILE") {
				t.Fatal(output.String())
			}
			if strings.Contains(output.String(), "private-token-reference") {
				t.Fatal("private reference logged")
			}
		})
	}
}
