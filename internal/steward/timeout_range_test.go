package steward

import (
	"strings"
	"testing"
)

func TestStewardTimeoutRangeRejectsOutsideBounds(t *testing.T) {
	for _, value := range []string{"199", "60001", "invalid"} {
		env := map[string]string{"ZOE_STEWARD_BASE_URL": "http://localhost", "ZOE_STEWARD_MODEL": "fake", "ZOE_STEWARD_API_KEY_FILE": "/fake", "ZOE_STEWARD_TIMEOUT_MS": value}
		_, err := FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fake-key"), nil })
		if err == nil || !strings.Contains(err.Error(), "between 200 and 60000") {
			t.Fatalf("timeout %q: err=%v", value, err)
		}
	}
}
