package control

import (
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"strings"
	"testing"
)

func TestEntryReceiptBoundedIdentityOnly(t *testing.T) {
	receipt := contracts.EntryDelivery{AttemptID: strings.Repeat("a", 128), Fence: 1, EntryID: strings.Repeat("e", 128), QuestionID: strings.Repeat("q", 128), ActionDigest: strings.Repeat("d", 64)}
	if !validEntryReceipt(receipt) {
		t.Fatal("maximal identity rejected")
	}
	body, err := json.Marshal(receipt)
	if err != nil || len(body) > 4096 {
		t.Fatalf("bytes=%d err=%v", len(body), err)
	}
	if strings.Contains(string(body), `"question":`) {
		t.Fatal("full question leaked into receipt")
	}
	receipt.QuestionID += "x"
	if validEntryReceipt(receipt) {
		t.Fatal("oversized question ID accepted")
	}
	receipt.QuestionID = "q"
	receipt.ActionDigest = strings.Repeat("d", 65)
	if validEntryReceipt(receipt) {
		t.Fatal("oversized digest accepted")
	}
	receipt.ActionDigest = ""
	if validEntryReceipt(receipt) {
		t.Fatal("partial answer identity accepted")
	}
}
