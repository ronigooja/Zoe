package main

import "testing"

func TestMemoryDrainGapCoalescing(t *testing.T) {
	var state memoryDrainState
	for i := 0; i < 3; i++ {
		if state.result(true, false) {
			t.Fatal("missing worker produced gap")
		}
	}
	if state.result(false, false) {
		t.Fatal("successful drain produced gap")
	}
	if !state.result(true, false) || state.result(true, false) {
		t.Fatal("failure was not coalesced")
	}
	if state.result(false, false) {
		t.Fatal("recovery produced gap")
	}
	if !state.result(false, true) {
		t.Fatal("overflow did not produce gap")
	}
	if !state.result(true, false) {
		t.Fatal("new failure not reported")
	}
}
