package workflow

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The agent of a place is one agent by its name and by its identity: the claim a start of the
// place's work runs on is made from the name AgentAt gives (Engine.stepToken), and the agent the
// start names is the identity agentIDAt gives (record.SuperviseRequest.Agent), which the claim's
// record of its session's agent is held to. So at every place the two name the same step's agent.
// The retro runs in the agent of the task's last step of the type implementation, which is not the
// first: step 3's agent, implementer-2, in `implementer reviewer implementer`, and key b's in
// `implementer:a reviewer implementer:b`. The fault: one of the two reads a rule of its own for
// the retro, so the retro's start names another agent than the one whose claim it runs on, and
// that claim starts in a new session.
func TestTheAgentOfAPlaceIsOneAgentByItsNameAndByItsIdentity(t *testing.T) {
	for _, tc := range []struct {
		words string
		// named and id are the name and the identity of the retro's agent.
		named claim.Agent
		id    plan.AgentID
	}{
		{words: "implementer reviewer implementer", named: "implementer-2", id: "step:3:a"},
		{words: "implementer:a reviewer implementer:b", named: "implementer-2", id: "key:b"},
	} {
		t.Run(tc.words, func(t *testing.T) {
			steps, err := plan.ParseSteps(tc.words)
			if err != nil {
				t.Fatalf("the steps %q: %v", tc.words, err)
			}
			rows := record.NameAgents(nil, nil, nil, planSteps(plan.BuiltinRoles(), "LEGION-208", 1, "a", steps))
			w := Walk{Stored: true, Task: "a", Steps: rows}
			retro := record.TaskPlace{Kind: record.PlaceRetro}
			if named, id := w.AgentAt(retro), w.agentIDAt(retro); named != tc.named || id != tc.id {
				t.Fatalf("the retro's agent is named %q with the identity %q, want %q and %q", named, id, tc.named, tc.id)
			}
			places := []record.TaskPlace{retro, {Kind: record.PlaceMerge}}
			for at := range rows {
				places = append(places, stepPlace(at+1))
			}
			for _, place := range places {
				named, id := w.AgentAt(place), w.agentIDAt(place)
				if named == "" || id == "" {
					if (named == "") != (id == "") {
						t.Errorf("at %+v the agent is named %q with the identity %q, want both or neither", place, named, id)
					}
					continue
				}
				at := slices.IndexFunc(rows, func(row record.PlanStep) bool { return row.AgentName() == named })
				if want := plan.AgentOf("a", at+1, plan.Step{Role: rows[at].Role, Key: rows[at].AgentKey}).ID(); id != want {
					t.Errorf("at %+v the agent is named %q, the agent %q, and its identity is %q", place, named, want, id)
				}
			}
		})
	}
}
