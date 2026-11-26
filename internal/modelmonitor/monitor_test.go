package modelmonitor

import (
	"testing"
	"time"
)

func TestOverflowEvictsAndStopsCapture(t *testing.T) {
	s := Subscribe()
	defer s.Close()
	for i := 0; i <= queueSize; i++ {
		Publish(Event{Component: "worker", CallID: "call", Time: time.Now(), Type: "request"})
	}
	select {
	case <-s.Gap:
	default:
		t.Fatal("missing gap")
	}
	if Enabled() {
		t.Fatal("evicted subscriber remains enabled")
	}
}

func TestGenerationFencesMembershipChanges(t *testing.T) {
	if Generation() != 0 {
		t.Fatal("expected disabled generation")
	}
	a := Subscribe()
	defer a.Close()
	g := Generation()
	b := Subscribe()
	defer b.Close()
	if Generation() <= g {
		t.Fatal("subscription did not advance generation")
	}
	e := Event{Component: "worker", CallID: "call", Time: time.Now(), Type: "request"}
	PublishFor(g, e)
	select {
	case <-a.Events:
		t.Fatal("stale event delivered")
	default:
	}
	select {
	case <-b.Events:
		t.Fatal("stale event delivered to new subscriber")
	default:
	}
	g = Generation()
	b.Close()
	if Generation() <= g {
		t.Fatal("removal did not advance generation")
	}
	PublishFor(g, e)
	select {
	case <-a.Events:
		t.Fatal("event survived membership removal")
	default:
	}
	PublishFor(Generation(), e)
	select {
	case <-a.Events:
	default:
		t.Fatal("current event not delivered")
	}
	a.Close()
	if Generation() != 0 {
		t.Fatal("generation should be zero without subscribers")
	}
}

func TestGapForOrderedFencedAndRetained(t *testing.T) {
	a := Subscribe()
	defer a.Close()
	g := Generation()
	e := Event{Component: "worker", CallID: "call", Time: time.Now(), Type: "request"}
	PublishFor(g, e)
	GapFor(g)
	PublishFor(g, e)
	first, second, third := <-a.Events, <-a.Events, <-a.Events
	if first.Gap || !second.Gap || third.Gap {
		t.Fatal("gap not ordered between events")
	}
	if Generation() != g {
		t.Fatal("gap retired subscription")
	}
	b := Subscribe()
	defer b.Close()
	GapFor(g)
	GapFor(0)
	select {
	case <-a.Events:
		t.Fatal("stale gap delivered")
	default:
	}
	select {
	case <-b.Events:
		t.Fatal("stale gap delivered to new subscriber")
	default:
	}
	GapFor(Generation())
	if !(<-a.Events).Gap || !(<-b.Events).Gap {
		t.Fatal("current gap missing")
	}
}

func TestGapForOverflowEvicts(t *testing.T) {
	s := Subscribe()
	defer s.Close()
	g := Generation()
	for i := 0; i <= queueSize; i++ {
		GapFor(g)
	}
	select {
	case <-s.Gap:
	default:
		t.Fatal("overflow did not signal gap")
	}
	if Generation() != 0 {
		t.Fatal("overflow did not evict")
	}
}
