package record

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

// A merge's production check goes to its task's last agent of the checking role, and where the task
// has no step of that role, to a fresh agent of the task, named from the task alone: `check-<slug>`,
// or, where that is longer than an agent's name may be, `check-t-` and the first 12 hexadecimal
// characters of the SHA-256 of the task's id. No other task's agents and no list of names enter it,
// so the name is the same whatever else the plan holds, and each is a name an agent's token takes.
//
// The fault it catches is the check of a task with no step of the role going to the session of
// another task's agent of that role, which is no fresh agent (its control: CheckAgent answering the
// role's own name where the task has none, under which b's check goes to "tester").
func TestTheCheckGoesToTheTasksLastAgentOfTheRoleElseAFreshOne(t *testing.T) {
	steps := []PlanStep{
		{Task: "a", Position: 1, Role: "implementer", Agent: "implementer"},
		{Task: "a", Position: 2, Role: "tester", Agent: "t"},
		{Task: "b", Position: 1, Role: "implementer", AgentKey: "k", Agent: "implementer"},
		{Task: "c", Position: 1, Role: "tester", Agent: "tester"},
	}
	long := strings.Repeat("x", 40)
	sum := sha256.Sum256([]byte(long))
	hashed := claim.Agent("check-t-" + hex.EncodeToString(sum[:6]))
	for _, tc := range []struct {
		name  string
		steps []PlanStep
		task  string
		want  claim.Agent
	}{
		{"a task with a tester step", steps, "a", "t"},
		{"its last tester step", append(append([]PlanStep{}, steps...), PlanStep{Task: "a", Position: 3, Role: "tester", AgentKey: "q", Agent: "q"}), "a", "q"},
		{"a task with no tester step, beside a task whose tester is named tester", steps, "b", "check-b"},
		{"the same task with no other task's steps", steps[2:3], "b", "check-b"},
		{"the same task with no steps at all", nil, "b", "check-b"},
		{"a task whose id is 40 characters", nil, long, hashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckAgent(tc.steps, tc.task, claim.RoleTester)
			if got != tc.want {
				t.Fatalf("CheckAgent(%s, tester) = %q, want %q", tc.task, got, tc.want)
			}
			if len(got) > maxAgentName {
				t.Fatalf("CheckAgent(%s, tester) = %q, %d bytes, want at most %d", tc.task, got, len(got), maxAgentName)
			}
			if _, err := claim.AgentToken("legion", "LEGION-208", got); err != nil {
				t.Fatalf("CheckAgent(%s, tester) = %q, which no agent's token takes: %v", tc.task, got, err)
			}
		})
	}
}
