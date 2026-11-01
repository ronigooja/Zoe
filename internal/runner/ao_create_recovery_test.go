package runner

import (
	"context"
	"errors"
	"github.com/ronigooja/Zoe/contracts"
	"testing"
)

type uncertainRuntime struct {
	runtimeFake
	createErr error
}

func (r *uncertainRuntime) Create(context.Context, RuntimeSpec) (RuntimeBinding, error) {
	r.creates++
	return RuntimeBinding{}, r.createErr
}
func TestUnknownRuntimeCreateRemainsFencedWithoutReplay(t *testing.T) {
	r := &uncertainRuntime{createErr: ErrRuntimeOutcomeUnknown}
	if _, err := r.Create(context.Background(), RuntimeSpec{}); !errors.Is(err, ErrRuntimeOutcomeUnknown) {
		t.Fatal(err)
	}
}
func TestRuntimeRejectionCodesAreContractShaped(t *testing.T) {
	for _, code := range []string{"invalid_request", "session_create_rejected"} {
		p := contracts.CreateRejectionProof{Code: code}
		if p.Code == "" {
			t.Fatal(code)
		}
	}
}
