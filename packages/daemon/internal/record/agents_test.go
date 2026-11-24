package record

import (
	"regexp"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
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

// Each task with a sub-owner has an agent of its own, named for the task: `sub-owner-` and the
// task's slug, or `sub-owner-t-` and the first 12 hexadecimal characters of the SHA-256 of the id
// where the slug would make a name longer than an agent's name may be. Each name is an agent's
// name, it makes the claim token of the issue's sub-owner of that task, and no step's name reads
// as a sub-owner's.
func TestEachTaskWithASubOwnerHasAnAgentOfItsOwn(t *testing.T) {
	agentName := regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	long := "a-task-id-of-forty-characters-0123456789"
	for _, tc := range []struct {
		task string
		want claim.Agent
	}{
		{task: "f", want: "sub-owner-f"},
		// The SHA-256 of the id is d6796c45cce506c8…, and the id is a slug of 40 characters.
		{task: long, want: "sub-owner-t-d6796c45cce5"},
		{task: "F", want: claim.Agent("sub-owner-" + plan.TaskSlug("F"))},
	} {
		got := plan.SubOwnerAgent(tc.task)
		if got != tc.want {
			t.Errorf("SubOwnerAgent(%q) = %q, want %q", tc.task, got, tc.want)
		}
		if !agentName.MatchString(string(got)) {
			t.Errorf("SubOwnerAgent(%q) = %q (%d bytes), which is no agent's name: %s, at most 32 bytes", tc.task, got, len(got), agentName)
		}
		if !plan.IsSubOwnerAgent(got) {
			t.Errorf("IsSubOwnerAgent(%q) = false, want true: it is the agent of the sub-owner of task %q", got, tc.task)
		}
		if _, err := claim.AgentToken("legion", "I-1", got); err != nil {
			t.Errorf("AgentToken of SubOwnerAgent(%q) = %q: %v", tc.task, got, err)
		}
	}
	if got := plan.SubOwnerAgent(long); len(got) != 24 {
		t.Errorf("SubOwnerAgent of an id of 40 characters = %q, %d bytes; want 24", got, len(got))
	}
	for _, name := range []claim.Agent{"implementer", "implementer-2", "architect"} {
		if plan.IsSubOwnerAgent(name) {
			t.Errorf("IsSubOwnerAgent(%q) = true, want false: it is a step's agent or the architect", name)
		}
	}
	token, err := claim.AgentToken("legion", "I-1", plan.SubOwnerAgent("f"))
	if err != nil || token != "legion-legion-i-1-sub-owner-f" {
		t.Errorf("AgentToken(legion, I-1, SubOwnerAgent(f)) = %q, %v; want legion-legion-i-1-sub-owner-f", token, err)
	}
}

// A claim is a task's sub-owner's where the agent its token ends in is named for one, and only
// then: a step's claim, an architect's, the controller's and a token that names no agent are not,
// even one that starts with `sub-owner-`. The fault it catches is a test of the whole token in
// place of its agent (its control: SubOwnerOf testing the prefix of the whole token, which misses
// the sub-owner's claim and takes the token with no agent).
func TestSubOwnerOfReadsTheAgentOfTheClaim(t *testing.T) {
	token := func(agent claim.Agent) claim.Token {
		t.Helper()
		token, err := claim.AgentToken("legion", "I-1", agent)
		if err != nil {
			t.Fatalf("AgentToken(legion, I-1, %s): %v", agent, err)
		}
		return token
	}
	for _, tc := range []struct {
		name      string
		token     claim.Token
		wantAgent claim.Agent
		want      bool
	}{
		{name: "a sub-owner's claim", token: token(plan.SubOwnerAgent("f")), wantAgent: "sub-owner-f", want: true},
		{name: "a step's claim", token: token("implementer")},
		{name: "an architect's claim", token: token(claim.Agent(claim.RoleArchitect))},
		{name: "the controller's claim", token: claim.ControllerToken("legion")},
		{name: "a token that names no agent", token: claim.Token(plan.SubOwnerAgent("f"))},
	} {
		if agent, ok := SubOwnerOf(tc.token); agent != tc.wantAgent || ok != tc.want {
			t.Errorf("SubOwnerOf(%s), %s = %q, %t; want %q, %t", tc.token, tc.name, agent, ok, tc.wantAgent, tc.want)
		}
	}
}
