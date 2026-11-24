package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Each agent of a version gets one name, the last part of its claim token: the steps of one key
// share it, a step with no key gets the role it is on, and the owner gets none, since its claim is
// the tree's architect. A later version keeps the name a key already has, and an agent beside it
// that would take the same name gets none in this release.
func TestNameAgentsGivesOneNameToEachAgentAndKeepsAKeysName(t *testing.T) {
	steps := []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
		{Task: "t", Position: 4, Role: "acceptance", AgentKey: "owner"},
	}
	got := NameAgents(nil, steps)
	want := []claim.Agent{"implementer", "implementer", "reviewer", ""}
	for i := range want {
		if got[i].Agent != want[i] {
			t.Errorf("step %d: name %q, want %q", i+1, got[i].Agent, want[i])
		}
	}
	if steps[0].Agent != "" {
		t.Errorf("NameAgents named the steps it was given (%q), want a copy named", steps[0].Agent)
	}
	later := []PlanStep{{Task: "u", Position: 1, Role: "tester", AgentKey: "a"}, {Task: "u", Position: 2, Role: "implementer"}}
	again := NameAgents(got, later)
	if again[0].Agent != "implementer" {
		t.Errorf("key a in a later version: name %q, want implementer, the name it had", again[0].Agent)
	}
	if again[1].Agent != "" {
		t.Errorf("a fresh implementer beside key a: name %q, want none in this release", again[1].Agent)
	}
}

// A key keeps its name when a later version gives the role it was named for to a fresh step that
// comes before it: every key an earlier row names is named before any new name is given, so the
// fresh step cannot take it, and a step with no key keeps the name its place had.
func TestAKeyKeepsItsNameWhenALaterVersionGivesItsRoleToAFreshStep(t *testing.T) {
	v1 := NameAgents(nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	})
	v2 := NameAgents(v1, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	})
	if v2[1].Agent != "implementer" {
		t.Errorf("key a in version 2: name %q, want implementer", v2[1].Agent)
	}
	if v2[0].Agent != "" {
		t.Errorf("the fresh step 1 of version 2: name %q, want none in this release", v2[0].Agent)
	}
	if v2[2].Agent != "reviewer" {
		t.Errorf("step 3 of version 2: name %q, want reviewer", v2[2].Agent)
	}
}

// A row that a release before the column wrote has no name, and reads as its role, which named its
// agent then. A step of the key owner has no name whatever it holds: its agent is the tree's
// architect, whose token names the tree, not the issue.
func TestARowWrittenBeforeTheColumnReadsAsItsRole(t *testing.T) {
	if got := (PlanStep{Role: "reviewer"}).AgentName(); got != "reviewer" {
		t.Errorf("AgentName = %q, want reviewer", got)
	}
	if got := (PlanStep{Role: "acceptance", AgentKey: "owner"}).AgentName(); got != "" {
		t.Errorf("AgentName = %q, want none: the owner is the tree's architect", got)
	}
	if got := (PlanStep{Role: "tester", AgentKey: "a", Agent: "implementer"}).AgentName(); got != "implementer" {
		t.Errorf("AgentName = %q, want the name the row holds", got)
	}
}
