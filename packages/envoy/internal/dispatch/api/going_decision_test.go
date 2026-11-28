package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// The decision puts to Jev the catalogue's proposed actions (Actions) and none, the twenty
// going/decision_models.json measured it on, and of those only the ones the caller may run: a
// bearer holding every scope is offered all twenty, and a person at a browser every one but the
// handoff, which only a bearer may write. Before, the decision offered every write tool the
// caller could call, sixty at a browser, forty of which no measurement had put to Jev, and its
// choice among them was a choice nothing said it made well.
func TestTheDecisionOffersOnlyTheMeasuredActions(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	measured := make([]string, 0, len(catalogue.Actions()))
	for _, action := range catalogue.Actions() {
		measured = append(measured, action.Action)
	}
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayProse, displayConfidence: 1})
	s := &server{deps: Deps{Decider: standIn.decider(t, staticToken)}}
	for _, caller := range []struct {
		name       string
		credential goingCredential
		offered    []string
	}{
		{"a bearer holding every scope", goingCredential{credential: personCredential}, measured},
		{"a person at a browser", goingCredential{cookie: true, credential: personCredential},
			slices.DeleteFunc(slices.Clone(measured), func(action string) bool { return action == "dispatch_handoff_write" })},
	} {
		if _, decided := s.goingDecide(t.Context(), "board drag", goingRunStart{actions: catalogue.runnableBy(caller.credential)}); !decided {
			t.Fatalf("%s: the decision came to nothing", caller.name)
		}
		want := slices.Sorted(slices.Values(append(slices.Clone(caller.offered), going.ActionNone)))
		if offered := standIn.offeredActions(t); !slices.Equal(offered, want) {
			t.Errorf("%s was offered %d choices %v, want the %d measured ones it may run and none %v", caller.name, len(offered), offered, len(want), want)
		}
	}
}
