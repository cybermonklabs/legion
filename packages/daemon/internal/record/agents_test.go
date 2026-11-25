package record

import (
	"slices"
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
	got := NameAgents(nil, steps, nil)
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
	again := NameAgents(got, later, nil)
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
	}, nil)
	v2 := NameAgents(v1, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
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

// A step names the agent its row holds, and no other: a row with no name has none. A step of the
// key owner has no name whatever it holds: its agent is the tree's architect, whose token names
// the tree, not the issue.
func TestAStepNamesTheAgentItsRowHolds(t *testing.T) {
	if got := (PlanStep{Role: "reviewer"}).AgentName(); got != "" {
		t.Errorf("AgentName = %q, want none: the row names no agent", got)
	}
	if got := (PlanStep{Role: "acceptance", AgentKey: "owner", Agent: "acceptance"}).AgentName(); got != "" {
		t.Errorf("AgentName = %q, want none: the owner is the tree's architect", got)
	}
	if got := (PlanStep{Role: "tester", AgentKey: "a", Agent: "implementer"}).AgentName(); got != "implementer" {
		t.Errorf("AgentName = %q, want the name the row holds", got)
	}
}

// ofGeneration is steps as rows of generation gen of an issue's plan.
func ofGeneration(gen uint64, steps []PlanStep) []PlanStep {
	rows := make([]PlanStep, len(steps))
	for i, step := range steps {
		step.Generation = gen
		rows[i] = step
	}
	return rows
}

// agentNames is the name of each step of named, in order.
func agentNames(named []PlanStep) []claim.Agent {
	got := make([]claim.Agent, len(named))
	for i, step := range named {
		got[i] = step.Agent
	}
	return got
}

// A name a key held is that key's alone, in every later version and generation: a key of a later
// generation that would take it gets none, so the version is refused (UnnamedAgents, the key that
// holds the name named) rather than run in that name's claim and session. When the first key comes
// back, it keeps the name.
func TestAKeyOfALaterGenerationTakesNoNameAnEarlierKeyHeld(t *testing.T) {
	first := ofGeneration(1, NameAgents(nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil))
	second := NameAgents(first, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "b"},
		{Task: "t", Position: 2, Role: "tester"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
	if want := []claim.Agent{"", "tester", "reviewer"}; !slices.Equal(agentNames(second), want) {
		t.Fatalf("the names of generation 2 = %q, want %q: key b takes no name key a held", agentNames(second), want)
	}
	if unnamed := UnnamedAgents(first, second, nil); len(unnamed) != 1 || unnamed[0].Step.Position != 1 || unnamed[0].Name != "implementer" || unnamed[0].Holder != "a" {
		t.Fatalf("UnnamedAgents = %+v, want step 1's agent, key b, which would take implementer, which key a holds", unnamed)
	}
	back := NameAgents(first, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "b"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
	if want := []claim.Agent{"", "implementer", "reviewer"}; !slices.Equal(agentNames(back), want) {
		t.Fatalf("the names with key a back = %q, want %q: key a keeps implementer and key b takes none", agentNames(back), want)
	}
}

// A key's name is its newest: rows of an earlier generation that named it otherwise do not move it
// within a generation. The project's default steps name key a for its first role, implementer, with
// no earlier rows (defaultRows); the owner's version of that generation keeps that name, and not the
// name tester that key a had in generation 1.
func TestAKeysNameIsItsNewestAcrossGenerations(t *testing.T) {
	first := ofGeneration(1, NameAgents(nil, []PlanStep{
		{Task: "t", Position: 1, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "implementer"},
	}, nil))
	defaults := ofGeneration(2, NameAgents(nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil))
	earlier := append(slices.Clone(first), defaults...)
	got := NameAgents(earlier, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
	if want := []claim.Agent{"implementer", "implementer", "reviewer"}; !slices.Equal(agentNames(got), want) {
		t.Fatalf("the names = %q, want %q: key a keeps the name its generation's default steps gave it", agentNames(got), want)
	}
}

// A key the version drops keeps its name: no step with no key and no other key takes it, whichever
// pass would give it, so no agent but that key resumes the session of that name's claim. The agent
// that would take it gets none, and the version is refused naming the key that holds the name.
func TestADroppedKeysNameIsTakenByNoOtherAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first []PlanStep
		later []PlanStep
		want  []claim.Agent
		// unnamed is the place of the step left with no name, the name it would take and the key
		// that holds that name.
		unnamed UnnamedAgent
	}{
		{
			name:    "a step with no key at the key's first role",
			first:   []PlanStep{{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"}, {Task: "t", Position: 2, Role: "tester", AgentKey: "a"}, {Task: "t", Position: 3, Role: "reviewer"}},
			later:   []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "tester"}, {Task: "t", Position: 3, Role: "reviewer"}},
			want:    []claim.Agent{"", "tester", "reviewer"},
			unnamed: UnnamedAgent{Step: PlanStep{Task: "t", Position: 1, Role: "implementer"}, Name: "implementer", Holder: "a"},
		},
		{
			name:    "a new key at the dropped key's first role",
			first:   []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "tester", AgentKey: "k"}, {Task: "t", Position: 3, Role: "reviewer", AgentKey: "k"}},
			later:   []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "tester", AgentKey: "m"}, {Task: "t", Position: 3, Role: "reviewer"}},
			want:    []claim.Agent{"implementer", "", "reviewer"},
			unnamed: UnnamedAgent{Step: PlanStep{Task: "t", Position: 2, Role: "tester", AgentKey: "m"}, Name: "tester", Holder: "k"},
		},
		{
			name:    "a step with no key at the place the key's step had",
			first:   []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "tester", AgentKey: "k"}, {Task: "t", Position: 3, Role: "reviewer", AgentKey: "k"}},
			later:   []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "tester"}, {Task: "t", Position: 3, Role: "reviewer"}},
			want:    []claim.Agent{"implementer", "", "reviewer"},
			unnamed: UnnamedAgent{Step: PlanStep{Task: "t", Position: 2, Role: "tester"}, Name: "tester", Holder: "k"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := ofGeneration(1, NameAgents(nil, tc.first, nil))
			got := NameAgents(first, tc.later, nil)
			if !slices.Equal(agentNames(got), tc.want) {
				t.Fatalf("the names = %q, want %q", agentNames(got), tc.want)
			}
			unnamed := UnnamedAgents(first, got, nil)
			if len(unnamed) != 1 || unnamed[0].Step.Position != tc.unnamed.Step.Position || unnamed[0].Name != tc.unnamed.Name || unnamed[0].Holder != tc.unnamed.Holder {
				t.Fatalf("UnnamedAgents = %+v, want step %d's agent, which would take %s, held by key %s", unnamed, tc.unnamed.Step.Position, tc.unnamed.Name, tc.unnamed.Holder)
			}
		})
	}
}

// Rows of the key owner in earlier name no agent: the owner's step of a later version still has no
// name, and no other key or step takes a name from the owner's rows.
func TestTheOwnersEarlierRowsNameNoAgent(t *testing.T) {
	earlier := ofGeneration(1, NameAgents(nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "acceptance", AgentKey: "owner"},
	}, nil))
	earlier[1].Agent = "acceptance"
	got := NameAgents(earlier, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "acceptance", AgentKey: "owner"},
	}, nil)
	if want := []claim.Agent{"implementer", ""}; !slices.Equal(agentNames(got), want) {
		t.Fatalf("the names = %q, want %q: the owner has no name whatever its earlier rows hold", agentNames(got), want)
	}
}

// UnnamedAgents names each agent NameAgents left with no name once, at its first step, with the
// name it would take, a key's newest earlier name or else its role's, and the key that holds that
// name: the key of the agent of the version that holds it, or else the key whose earlier row held it
// last. A named agent and the owner are not named, nor is a later step of an unnamed key.
func TestUnnamedAgentsNamesEachAgentWithNoNameAndTheNameItWouldTake(t *testing.T) {
	earlier := []PlanStep{
		{Generation: 1, Task: "t", Position: 1, Role: "implementer", AgentKey: "a", Agent: "tester"},
		{Generation: 2, Task: "t", Position: 1, Role: "implementer", AgentKey: "a", Agent: "critic"},
	}
	named := []PlanStep{
		{Task: "t", Position: 1, Role: "reviewer", AgentKey: "b", Agent: "reviewer"},
		{Task: "t", Position: 2, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 4, Role: "reviewer"},
		{Task: "t", Position: 5, Role: "acceptance", AgentKey: "owner"},
	}
	got := UnnamedAgents(earlier, named, nil)
	want := []UnnamedAgent{{Step: named[1], Name: "critic", Holder: "a"}, {Step: named[3], Name: "reviewer", Holder: "b"}}
	if !slices.Equal(got, want) {
		t.Fatalf("UnnamedAgents = %+v, want %+v", got, want)
	}
	if none := UnnamedAgents(nil, NameAgents(nil, named[:3], nil), nil); len(none) != 0 {
		t.Fatalf("UnnamedAgents of a version every agent of which has a name = %+v, want none", none)
	}
}

// A step's token is the tree's architect's for a step of the key owner, whose agent has no name on
// the issue, and the token of its agent's name on the issue otherwise, so the steps of one key are
// one claim. A name outside the agent grammar has no token.
func TestStepTokenIsTheTreeArchitectsForTheOwnerAndItsAgentsOtherwise(t *testing.T) {
	for _, tc := range []struct {
		step PlanStep
		want claim.Token
	}{
		{PlanStep{Task: "t", Position: 1, Role: "implementer", AgentKey: "a", Agent: "implementer"}, "legion-legion-legion-209-implementer"},
		{PlanStep{Task: "t", Position: 2, Role: "tester", AgentKey: "a", Agent: "implementer"}, "legion-legion-legion-209-implementer"},
		{PlanStep{Task: "t", Position: 3, Role: "reviewer", Agent: "reviewer"}, "legion-legion-legion-209-reviewer"},
		{PlanStep{Task: "t", Position: 4, Role: "acceptance", AgentKey: "owner"}, "legion-legion-legion-208-architect"},
	} {
		got, err := StepToken("legion", "LEGION-209", "LEGION-208", tc.step)
		if err != nil || got != tc.want {
			t.Errorf("StepToken(step %d) = %q, %v; want %q", tc.step.Position, got, err, tc.want)
		}
	}
	if got, err := StepToken("legion", "LEGION-209", "LEGION-208", PlanStep{Task: "t", Position: 1, Role: "implementer", Agent: "Implementer"}); err == nil {
		t.Errorf("StepToken of a step named outside the agent grammar = %q, want a refusal", got)
	}
}
