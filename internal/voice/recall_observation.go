package voice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

type recallObservation struct {
	id         string
	generation uint64
}

func startRecallObservation(owner, input string) *recallObservation {
	// The monitor subscription is authorized only for the instance owner.
	if owner != "owner" {
		return nil
	}
	generation := modelmonitor.Generation()
	if generation == 0 {
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	o := &recallObservation{id: hex.EncodeToString(id[:]), generation: generation}
	o.publish("request", "input", input)
	return o
}

func (o *recallObservation) response(text string) {
	o.publish("response", "content", text)
}

func (o *recallObservation) publish(kind, field, text string) {
	if o == nil || modelmonitor.Generation() != o.generation {
		return
	}
	text, truncated := modelclient.ObservationText(text)
	payload, err := json.Marshal(map[string]string{field: text})
	if err != nil {
		return
	}
	modelmonitor.PublishFor(o.generation, modelmonitor.Event{Component: "recall", CallID: o.id, Time: time.Now(), Type: kind, Payload: payload, Truncated: truncated})
}
