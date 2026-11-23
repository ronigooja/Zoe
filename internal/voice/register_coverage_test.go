package voice

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRegisterRetainedCoverageAfterMemoryAdvanceAndFailure(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old compact source", f.now.Add(-3*time.Hour))
	if !f.rebuild(f.now) {
		t.Fatal("initial build")
	}
	before := f.layers(f.now)
	f.boundary = f.now.Add(-30 * time.Minute)
	f.turn("handoff-session", 1, "assistant", "retained interval raw", f.now.Add(-45*time.Minute))
	f.turn("handoff-session", 2, "user", "new interval source", f.now.Add(-10*time.Minute))
	later := f.now.Add(time.Hour)
	f.fail = true
	if _, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, later); err == nil {
		t.Fatal("expected model failure")
	}
	after := f.layers(later)
	if after.Compact != before.Compact || after.CompactFrom != before.CompactFrom || after.CompactThrough != before.CompactThrough || after.RawFrom != before.RawFrom || after.Version != before.Version || !strings.Contains(after.Raw, "retained interval raw") {
		t.Fatalf("lost retained coverage: %+v", after)
	}
}

func TestRegisterBackdatedUnconsumedSourceAndOwnerScope(t *testing.T) {
	f := newRegisterFixture(t)
	f.exec(`CREATE TABLE IF NOT EXISTS memory_ingest_batches(id TEXT PRIMARY KEY,principal_id TEXT,from_ns INTEGER,until_ns INTEGER)`)
	f.exec(`CREATE TABLE IF NOT EXISTS memory_ingest_members(principal_id TEXT,source_id TEXT,batch_id TEXT,PRIMARY KEY(principal_id,source_id))`)
	f.turn("handoff-session", 0, "user", "initial source", f.now.Add(-3*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	f.boundary = f.now.Add(-30 * time.Minute)
	f.exec(`INSERT INTO memory_ingest_batches(id,principal_id,from_ns,until_ns) VALUES('covered','owner',0,?)`, f.boundary.UnixNano())
	f.exec(`INSERT INTO memory_ingest_members VALUES('owner',?,'covered')`, `turn:"handoff-session":0`)
	f.turn("handoff-session", 1, "user", "BACKDATED_UNSEEN", f.now.Add(-2*time.Hour))
	f.exec(`UPDATE voice_transcript_turns SET received_at=? WHERE session_id='handoff-session' AND seq=1`, f.now.Add(time.Minute).Format(time.RFC3339Nano))
	f.session("foreign", "other")
	f.turn("foreign", 0, "user", "SECRET_FOREIGN", f.now.Add(-2*time.Hour))
	after := f.layers(f.now.Add(2 * time.Minute))
	if after.RawFrom != before.RawFrom || !strings.Contains(after.Raw, "BACKDATED_UNSEEN") || strings.Contains(after.Raw, "SECRET_FOREIGN") {
		t.Fatalf("late raw/isolation: %+v", after)
	}
	f.fail = true
	if _, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, f.now.Add(time.Hour)); err == nil {
		t.Fatal("expected failure")
	}
	f.fail = false
	f.output = "late source compact"
	if !f.rebuild(f.now.Add(time.Hour)) {
		t.Fatal("late source not rebuilt")
	}
	input := f.inputs[len(f.inputs)-1]
	if !strings.Contains(input, "BACKDATED_UNSEEN") || strings.Contains(input, "SECRET_FOREIGN") || strings.Contains(input, "initial source") {
		t.Fatalf("membership rebuild input=%q", input)
	}
	if strings.Contains(f.layers(f.now.Add(time.Hour)).Raw, "BACKDATED_UNSEEN") {
		t.Fatal("successful coverage still replays late raw")
	}
}

func TestRegisterFailedMemoryFenceRetainsPriorInterval(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "initial", f.now.Add(-3*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	f.turn("handoff-session", 1, "assistant", "raw after edge", f.now.Add(-30*time.Minute))
	f.onCall = func() { f.boundary = f.now.Add(-15 * time.Minute) }
	built, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, f.now.Add(time.Hour))
	if err != nil || built {
		t.Fatalf("changed M committed built=%v err=%v", built, err)
	}
	after := f.layers(f.now.Add(time.Hour))
	if after.Compact != before.Compact || after.CompactFrom != before.CompactFrom || after.RawFrom != before.RawFrom || after.Version != before.Version || !strings.Contains(after.Raw, "raw after edge") {
		t.Fatalf("M fence lost interval: %+v", after)
	}
}
