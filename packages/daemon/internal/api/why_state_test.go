package api

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The one rendered answer an issue that is not working now carries reaches the wire under `why`,
// as the two keys a reader parses it by and in that order: the state word and the sentence. An
// issue carrying none — one at work, one the record does not hold in the workflow — carries no
// `why` key at all, rather than an empty object a reader would have to tell from an answer.
func TestWhyStateIsServedOnTheIssueThatCarriesOne(t *testing.T) {
	state := State{Issues: map[string]Issue{
		"LEGION-208": {Key: "LEGION-208", Phase: phase.Implementing, Status: "in_progress",
			Why: &WhyView{State: WhyClaimStopped,
				Reason: "Its phase is implementing and the implementer's claim is stranded at failed, so nothing will wake it."}},
		"LEGION-209": {Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"},
	}}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal the state: %v", err)
	}
	var body struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("read the state back, %s: %v", encoded, err)
	}
	raw, carried := body.Issues["LEGION-208"]["why"]
	if !carried {
		t.Fatalf("LEGION-208 is served no why: %s", encoded)
	}
	want := `{"state":"claim_stopped","reason":"Its phase is implementing and the implementer's claim is stranded at failed, so nothing will wake it."}`
	if string(raw) != want {
		t.Errorf("LEGION-208's why = %s, want %s", raw, want)
	}
	if served, carries := body.Issues["LEGION-209"]["why"]; carries {
		t.Errorf("LEGION-209 is served why %s, want no such key: it carries no answer", served)
	}
}
