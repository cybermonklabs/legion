package contracts

//ci:reads packages/contracts/fixtures/dispatch-frame-verdicts.json TestDispatchPayloadIsEventAgreesWithTheHostsReading os.ReadFile (../../../contracts/fixtures): DispatchPayloadIsEvent must agree with readDispatchPayload's verdict on every payload

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDispatchPayloadIsEventAgreesWithTheHostsReading holds DispatchPayloadIsEvent to
// readDispatchPayload, the host's reading of a Dispatch payload in packages/contracts, which the
// plugin's renderInbound calls: for every payload of
// packages/contracts/fixtures/dispatch-frame-verdicts.json, which gen-go writes from that function
// over dispatchFrameCorpus (the four payloads a host reads as events and every one-step change of
// them), the payload is an event here exactly when the host reads it as one. A
// change of the zod shapes that `bun run gen:go` was not run for fails the fixture's check there,
// and a change of either reader that splits them fails here.
func TestDispatchPayloadIsEventAgreesWithTheHostsReading(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../../contracts/fixtures/dispatch-frame-verdicts.json")
	if err != nil {
		t.Fatalf("read the verdicts fixture: %v", err)
	}
	var cases []struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
		Kind    string `json:"kind"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode the verdicts fixture: %v", err)
	}
	events := 0
	for _, c := range cases {
		if c.Kind == "event" {
			events++
		}
		if got := DispatchPayloadIsEvent([]byte(c.Payload)); got != (c.Kind == "event") {
			t.Errorf("DispatchPayloadIsEvent(%s) = %t; the host reads it as %s (%s)", c.Payload, got, c.Kind, c.Name)
		}
	}
	if len(cases) < 500 || events < 100 || events == len(cases) {
		t.Fatalf("the fixture holds %d payloads, %d of them events: it tests too little to tell the readers apart", len(cases), events)
	}
}

// TestDispatchPayloadIsEventRefusesAPrototypeKey holds the one place the Go reader is stricter
// than the host's: a "__proto__" key at any depth, which zod's passthrough copies onto a prototype.
func TestDispatchPayloadIsEventRefusesAPrototypeKey(t *testing.T) {
	t.Parallel()
	event := `{"issue_key":"CORE-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":{"id":"c-1"}}`
	if !DispatchPayloadIsEvent([]byte(event)) {
		t.Fatalf("control: DispatchPayloadIsEvent(%s) = false, want true", event)
	}
	proto := `{"issue_key":"CORE-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":{"id":"c-1","__proto__":{"body":"PUB"}}}`
	if DispatchPayloadIsEvent([]byte(proto)) {
		t.Fatalf("DispatchPayloadIsEvent(%s) = true, want false", proto)
	}
}
