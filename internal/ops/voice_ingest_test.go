package ops

import "testing"

func TestVoiceIngestEnvironmentIsOperatorOwned(t *testing.T) {
	for _, key := range []string{"ZOE_VOICE_INGEST_TOKEN_FILE", "ZOE_VOICE_INGEST_ID", "ZOE_VOICE_INGEST_WORKSPACES"} {
		if !isOperatorOwnedEnvKey(key) || isReleaseOwnedEnvKey(key) || !validOperatorEnvValue(key, "configured") {
			t.Fatalf("ingest setting %s is not preserved as operator-owned", key)
		}
	}
}
