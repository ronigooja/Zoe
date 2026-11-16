package voice

import (
	"context"
	"errors"
	"testing"
)

type textReceiptFixture struct {
	submits, lookups int
	lost             bool
}

func (a *textReceiptFixture) Submit(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.submits++
	if a.lost {
		a.lost = false
		return ResultChunkReceipt{}, errors.New("response lost")
	}
	return ResultChunkReceipt{MediaGeneration: p.MediaGeneration, DeliveryID: p.DeliveryID, ContentHash: p.ContentHash, State: "applied"}, nil
}
func (a *textReceiptFixture) Lookup(_ context.Context, _ string, p ResultChunkRequest) (ResultChunkReceipt, error) {
	a.lookups++
	return ResultChunkReceipt{MediaGeneration: p.MediaGeneration, DeliveryID: p.DeliveryID, ContentHash: p.ContentHash, State: "applied"}, nil
}

func TestDurableTextLookupNamespaceAndMissingCapability(t *testing.T) {
	r := newDeliveryRig(t)
	r.seedCall(t, "text-call", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=7 WHERE id='text-call'`)
	ctx := context.Background()
	send := func(kind, id, text string) (bool, string) {
		return r.svc.deliverDurableText(ctx, "default", "user-1", kind, id, "text-call", 1, text)
	}
	if ok, code := send("fast_reply", "same", "full text"); ok || code != "receipt_capability_disabled" {
		t.Fatalf("nil=%v %s", ok, code)
	}
	adapter := &textReceiptFixture{lost: true}
	r.svc.resultReceiptAdapter = adapter
	if ok, code := send("fast_reply", "same", "full text"); ok || code != "receipt_transport_unknown" {
		t.Fatalf("lost=%v %s", ok, code)
	}
	if ok, code := send("fast_reply", "same", "full text"); !ok {
		t.Fatalf("reconcile=%s", code)
	}
	if ok, _ := send("fast_reply", "same", "full text"); !ok {
		t.Fatal("repeat")
	}
	if adapter.submits != 1 || adapter.lookups != 1 {
		t.Fatalf("duplicate submits=%d lookup=%d", adapter.submits, adapter.lookups)
	}
	if ok, code := send("ui_feedback", "same", "different namespace"); !ok {
		t.Fatalf("namespace=%s", code)
	}
	if adapter.submits != 2 {
		t.Fatal("source kind collision")
	}
	if ok, _ := send("fast_reply", "same", "mutated content"); ok {
		t.Fatal("source changed")
	}
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='text-call'`)
	if ok, code := send("fast_reply", "no-call", "pending text"); ok || code != "awaiting_context" {
		t.Fatalf("no call=%v %s", ok, code)
	}
}
