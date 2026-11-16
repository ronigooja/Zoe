package control

import "testing"

func TestRotationSnapshotStrictRequiredFields(t *testing.T) {
	valid := `{"schema_version":1,"instructions":null,"entries":[],"task_prior_contexts":[]}`
	if _, err := decodeRotationSnapshot([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `{"schema_version":1}`, `["old"]`,
		`{"instructions":null,"entries":[],"task_prior_contexts":[]}`,
		`{"schema_version":1,"entries":[],"task_prior_contexts":[]}`,
		`{"schema_version":1,"instructions":null,"task_prior_contexts":[]}`,
		`{"schema_version":1,"instructions":null,"entries":[]}`,
		`{"schema_version":2,"instructions":null,"entries":[],"task_prior_contexts":[]}`,
		`{"schema_version":1,"instructions":null,"entries":[],"task_prior_contexts":[],"unknown":true}`,
		`{"schema_version":1,"instructions":null,"entries":[{"unknown":true}],"task_prior_contexts":[]}`,
		valid + ` {}`, valid + ` trailing`,
	} {
		if _, err := decodeRotationSnapshot([]byte(body)); err == nil {
			t.Fatalf("accepted incomplete/unsupported snapshot: %s", body)
		}
	}
}
