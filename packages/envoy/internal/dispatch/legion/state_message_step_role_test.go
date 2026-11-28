package legion

import (
	"encoding/json"
	"testing"
)

// A state event's change of an issue's step role is observed as the whole read of the same state
// observes it (FieldStepRole, stepRoleValue): the role of the running step of an issue at step, a
// role the deployment defines, which metric 2 puts that step's hours on. A move from one step of
// the deployment to another moves no phase, so the step role is all the event names, and the move
// out of the step names the phase and a step role of null. The daemon writes the field as
// `stepRole` (packages/contracts' legionStateChange).
func TestAStateEventsStepRoleIsObservedAsAWholeReadObservesIt(t *testing.T) {
	events, err := decodeEvents(json.RawMessage(`[
		{"seq": 7, "kind": "change", "issue": "LEGION-240", "changes": [{"field": "stepRole", "old": "checker", "new": "linter"}]},
		{"seq": 8, "kind": "change", "issue": "LEGION-240", "changes": [
			{"field": "phase", "old": "step", "new": "retro"}, {"field": "stepRole", "old": "linter", "new": null}]}]`))
	if err != nil {
		t.Fatalf("decode the events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("decoded %d events, want 2", len(events))
	}
	atStep := State{Issues: map[string]Issue{"LEGION-240": {Key: "LEGION-240", Phase: PhaseStep, StepRole: "linter"}}}
	whole := map[string]string{}
	for _, observation := range atStep.Observations() {
		if observation.IssueKey == "LEGION-240" {
			whole[observation.Field] = string(observation.Value)
		}
	}
	got := func(event stateEvent) map[string]string {
		fields := map[string]string{}
		for _, observation := range event.observations {
			if observation.IssueKey != "LEGION-240" {
				t.Errorf("an event of LEGION-240 observes %s of %s", observation.Field, observation.IssueKey)
			}
			fields[observation.Field] = string(observation.Value)
		}
		return fields
	}
	if moved := got(events[0]); len(moved) != 1 || moved[FieldStepRole] != `"linter"` || moved[FieldStepRole] != whole[FieldStepRole] {
		t.Errorf("the move between two steps of the deployment observes %v, want step_role \"linter\" alone, as a whole read at that step observes it (%s)",
			moved, whole[FieldStepRole])
	}
	if left := got(events[1]); left[FieldPhase] != `"retro"` || left[FieldStepRole] != "null" {
		t.Errorf("the move out of the step observes %v, want phase \"retro\" and step_role null", left)
	}
}
