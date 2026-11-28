package record

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Each agent of a version gets one name, the last part of its claim token: the steps of one key
// share it, a step with no key gets the role it is on, and the owner gets none, since its claim is
// the tree's architect. A later version keeps the name a key already has, and an agent beside it
// that would take the same name gets a name of its own, `implementer-2`.
func TestNameAgentsGivesOneNameToEachAgentAndKeepsAKeysName(t *testing.T) {
	steps := []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
		{Task: "t", Position: 4, Role: "acceptance", AgentKey: "owner"},
	}
<<<<<<< HEAD
	got := NameAgents(nil, nil, steps)
=======
	got := NameAgents(nil, steps, nil)
>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
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
<<<<<<< HEAD
	again := NameAgents(got, nil, later)
=======
	again := NameAgents(got, later, nil)
>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
	if again[0].Agent != "implementer" {
		t.Errorf("key a in a later version: name %q, want implementer, the name it had", again[0].Agent)
	}
	if again[1].Agent != "implementer-2" {
		t.Errorf("a fresh implementer beside key a: name %q, want implementer-2, a name of its own", again[1].Agent)
	}
}

// A key keeps its name when a later version gives the role it was named for to a fresh step that
// comes before it: every key an earlier row names is named before any new name is given, so the
// fresh step cannot take it, and a step with no key keeps the name its place had.
func TestAKeyKeepsItsNameWhenALaterVersionGivesItsRoleToAFreshStep(t *testing.T) {
	v1 := NameAgents(nil, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
<<<<<<< HEAD
	})
	v2 := NameAgents(v1, nil, []PlanStep{
=======
	}, nil)
	v2 := NameAgents(v1, []PlanStep{
>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
	if v2[1].Agent != "implementer" {
		t.Errorf("key a in version 2: name %q, want implementer", v2[1].Agent)
	}
	if v2[0].Agent != "implementer-2" {
		t.Errorf("the fresh step 1 of version 2: name %q, want implementer-2, a name of its own", v2[0].Agent)
	}
	if v2[2].Agent != "reviewer" {
		t.Errorf("step 3 of version 2: name %q, want reviewer", v2[2].Agent)
	}
}

// A second agent of one role gets a name of its own: the role and the first number from 2 that no
// agent of the version and no key holds, here an earlier row's key b, so two agents never share a
// name, and so a session. The role is cut so that the name has at most 32 bytes, the agent
// grammar's bound. The fault: step 3 of `implementer reviewer implementer` shares step 1's name, or
// gets none.
func TestASecondAgentOfOneRoleGetsANameOfItsOwn(t *testing.T) {
	steps := []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "reviewer"},
		{Task: "t", Position: 3, Role: "implementer"},
	}
	if got := NameAgents(nil, nil, steps); got[0].Agent != "implementer" || got[2].Agent != "implementer-2" {
		t.Errorf("implementer reviewer implementer: names %q and %q, want implementer and implementer-2", got[0].Agent, got[2].Agent)
	}
	earlier := []PlanStep{{Task: "old", Position: 1, Role: "tester", AgentKey: "b", Agent: "implementer-2"}}
	if got := NameAgents(earlier, nil, steps); got[2].Agent != "implementer-3" {
		t.Errorf("step 3 beside an earlier row of key b named implementer-2: name %q, want implementer-3", got[2].Agent)
	}
	long := strings.Repeat("r", 32)
	twice := NameAgents(nil, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: long},
		{Task: "t", Position: 3, Role: long},
	})
	if want := claim.Agent(strings.Repeat("r", 30) + "-2"); twice[2].Agent != want || len(twice[2].Agent) != 32 {
		t.Errorf("a second agent of a role of 32 bytes: name %q, want %q, 32 bytes", twice[2].Agent, want)
	}
	for _, step := range twice {
		if _, err := claim.AgentToken("legion", "LEGION-208", step.Agent); err != nil {
			t.Errorf("the name %q makes no claim token: %v", step.Agent, err)
		}
	}
}

// Rows written before a name a key held was kept for it can show two keys that held one name
// across generations: generation 1 names key a implementer, and in generation 2, where no step
// names key a, key b took implementer by pass 4. A later version that
// names both keys gives the name to one key alone, the one whose row held it last, key b, whose
// agent ran on that name's claim last; key a gets a name of its own, implementer-2. So one name is
// one claim and one session for one agent. The fault: pass 2 gave each key the name its first row
// had, both implementer, two agents on one claim and one session.
func TestTwoKeysThatHeldOneNameInTurnGetTwoNames(t *testing.T) {
	earlier := []PlanStep{
		{Generation: 1, Task: "t", Position: 1, Role: "implementer", AgentKey: "a", Agent: "implementer"},
		{Generation: 1, Task: "t", Position: 2, Role: "reviewer", Agent: "reviewer"},
		{Generation: 2, Task: "u", Position: 1, Role: "implementer", AgentKey: "b", Agent: "implementer"},
		{Generation: 2, Task: "u", Position: 2, Role: "reviewer", Agent: "reviewer"},
	}
	got := NameAgents(earlier, nil, []PlanStep{
		{Generation: 3, Task: "v", Position: 1, Role: "implementer", AgentKey: "a"},
		{Generation: 3, Task: "v", Position: 2, Role: "reviewer"},
		{Generation: 3, Task: "v", Position: 3, Role: "implementer", AgentKey: "b"},
	})
	if got[0].Agent == got[2].Agent {
		t.Fatalf("keys a and b both named %q, want two names", got[0].Agent)
	}
	if got[2].Agent != "implementer" || got[0].Agent != "implementer-2" {
		t.Errorf("key a named %q and key b %q, want key b implementer, the key that held it last, and key a implementer-2", got[0].Agent, got[2].Agent)
	}
	if again := NameAgents(append(earlier, got...), nil, []PlanStep{
		{Generation: 3, Task: "w", Position: 1, Role: "implementer", AgentKey: "a"},
		{Generation: 3, Task: "w", Position: 2, Role: "implementer", AgentKey: "b"},
	}); again[0].Agent != got[0].Agent || again[1].Agent != got[2].Agent {
		t.Errorf("a later version names key a %q and key b %q, want each the name it had, %q and %q", again[0].Agent, again[1].Agent, got[0].Agent, got[2].Agent)
	}
}

// A name an earlier row of a key held is that key's: no agent of a later version takes it but that
// key, so the claim of the name, and the session it holds, wait for the key across a version that
// leaves the key out. v1 `implementer:k reviewer` names key k implementer; v2 `implementer
// reviewer` gives step 1, which has no key, implementer-2, a name of its own, at the cost of a slot
// of its own; v3 `implementer:k reviewer` gives key k implementer again. Another key is held to the
// same rule, and so is a step with no key whose place had the name before the key took it. A name
// only steps with no key held stays free: a new task's step with no key takes the name of a
// stopped task's step of its role. The fault: the step with no key of v2 took implementer, so key
// k's return in v3 found that name's claim holding another agent's session, and started fresh.
func TestANameAKeyHeldIsKeptForThatKey(t *testing.T) {
	v1 := NameAgents(nil, nil, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "k"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer"},
	})
	if v1[0].Agent != "implementer" {
		t.Fatalf("key k in version 1: name %q, want implementer", v1[0].Agent)
	}
	v2 := NameAgents(v1, nil, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer"},
	})
	if v2[0].Agent != "implementer-2" || v2[1].Agent != "reviewer" {
		t.Fatalf("version 2, which leaves key k out: names %q and %q, want implementer-2, a name of its own beside key k's, and reviewer",
			v2[0].Agent, v2[1].Agent)
	}
	// The version written replaces the steps of the task it gives, so the rows version 3 is named
	// from are version 2's.
	v3 := NameAgents(v2, nil, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "k"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer"},
	})
	if v3[0].Agent != "implementer" {
		t.Errorf("key k back in version 3: name %q, want implementer, the name whose claim holds its session", v3[0].Agent)
	}
	if other := NameAgents(v1, nil, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "j"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer"},
	}); other[0].Agent != "implementer-2" {
		t.Errorf("key j in the place of key k: name %q, want implementer-2, since key k held implementer", other[0].Agent)
	}
	// Generation 1 ran task a with a step with no key named implementer, and generation 2 gave
	// that place to key k, which took the name. A later version of generation 2 that takes the key
	// off again finds the place's name of generation 1, which key k holds now.
	keyed := []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", Agent: "implementer"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer", Agent: "reviewer"},
		{Generation: 2, Task: "a", Position: 1, Role: "implementer", AgentKey: "k", Agent: "implementer"},
		{Generation: 2, Task: "a", Position: 2, Role: "reviewer", Agent: "reviewer"},
	}
	if place := NameAgents(keyed, nil, []PlanStep{
		{Generation: 2, Task: "a", Position: 1, Role: "implementer"},
		{Generation: 2, Task: "a", Position: 2, Role: "reviewer"},
	}); place[0].Agent != "implementer-2" {
		t.Errorf("a step with no key at a place whose name key k took: name %q, want implementer-2", place[0].Agent)
	}
	if free := NameAgents(v2, nil, []PlanStep{
		{Generation: 1, Task: "p", Position: 1, Role: "reviewer"},
	}); free[0].Agent != "reviewer" {
		t.Errorf("control: a new task's reviewer, where only a step with no key held reviewer: name %q, want reviewer", free[0].Agent)
	}
}

// A name whose claim holds the session of a key's agent is that key's, after the versions that
// left the key out have replaced every row that named it: version 2, `implementer reviewer`,
// replaced key k's rows, so version 3, `planner implementer reviewer`, is named from rows that name
// no key, and the claim implementer, whose session is key k's (StoredClaimsOf), keeps implementer
// for key k. Version 3's implementer takes implementer-2, which only version 2's step with no key
// held, and version 4, `implementer:k reviewer`, gives key k implementer, the name whose claim
// holds its session; another key is given a name of its own. Each claim is kept by its name with
// its role; a claim whose session is a step's agent with no key, or the key owner's, or that
// records none, holds no key. The fault: the rows alone named version 3's implementer implementer,
// the claim of key k's session.
func TestANameWhoseClaimHoldsAKeysSessionIsKeptForThatKey(t *testing.T) {
	token := func(name claim.Agent) claim.Token {
		got, err := claim.AgentToken("legion", "LEGION-208", name)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	sessions := StoredClaimsOf([]IssueClaim{
		{Token: token("implementer"), Role: "implementer", SessionAgent: "key:k"},
		{Token: token("implementer-2"), Role: "implementer", SessionAgent: "step:1:a"},
		{Token: token("reviewer"), Role: "reviewer"},
		{Token: token("tester"), Role: "tester", SessionAgent: "key:owner"},
	})
	want := StoredClaims{"implementer": {Role: "implementer", Key: "k"}, "implementer-2": {Role: "implementer"},
		"reviewer": {Role: "reviewer"}, "tester": {Role: "tester"}}
	if !maps.Equal(sessions, want) {
		t.Fatalf("the key sessions of the claims = %v, want %v", sessions, want)
	}
	v2 := []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", Agent: "implementer-2"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer", Agent: "reviewer"},
	}
	v3 := NameAgents(v2, sessions, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "planner"},
		{Generation: 1, Task: "a", Position: 2, Role: "implementer"},
		{Generation: 1, Task: "a", Position: 3, Role: "reviewer"},
	})
	if got := []claim.Agent{v3[0].Agent, v3[1].Agent, v3[2].Agent}; !slices.Equal(got, []claim.Agent{"planner", "implementer-2", "reviewer"}) {
		t.Fatalf("version 3, beside the claim implementer of key k's session: names %q, want planner, implementer-2 and reviewer", got)
	}
	if v4 := NameAgents(v3, sessions, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "k"},
		{Generation: 1, Task: "a", Position: 2, Role: "reviewer"},
	}); v4[0].Agent != "implementer" {
		t.Errorf("key k in version 4: name %q, want implementer, the name whose claim holds its session", v4[0].Agent)
	}
	if other := NameAgents(v3, sessions, []PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "j"},
	}); other[0].Agent == "implementer" {
		t.Errorf("key j beside the claim implementer of key k's session: name %q, want a name of its own", other[0].Agent)
	}
}

// Rows a release before the claim recorded its session's agent wrote can show a name held by one
// key and then by another: generation 1 names key a implementer, and generation 2, where no step
// named key a, gave implementer to key b. No claim records a key's session (that release records
// none), so the rows alone say whose the name is: key b's, which held it last, and whose agent ran
// on its claim last. A version that names key a and not key b gives key a a name of its own,
// implementer-2, so key b's name, and the session its claim holds, pass to no other key. The
// control: a version that names key b alone gives it implementer. The fault: key a, the one key of
// the version that ever held implementer, took it by its older row, and with it key b's session.
func TestAKeyDoesNotTakeANameAnotherKeyHeldAfterItInRowsOfTheReleaseBefore(t *testing.T) {
	earlier := []PlanStep{
		{Generation: 1, Task: "t", Position: 1, Role: "implementer", AgentKey: "a", Agent: "implementer"},
		{Generation: 1, Task: "t", Position: 2, Role: "reviewer", Agent: "reviewer"},
		{Generation: 2, Task: "u", Position: 1, Role: "implementer", AgentKey: "b", Agent: "implementer"},
		{Generation: 2, Task: "u", Position: 2, Role: "reviewer", Agent: "reviewer"},
	}
	for _, tc := range []struct {
		name string
		key  string
		want claim.Agent
	}{
		{name: "key a, whose name key b held after it", key: "a", want: "implementer-2"},
		{name: "control: key b, which held the name last", key: "b", want: "implementer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NameAgents(earlier, StoredClaims{}, []PlanStep{
				{Generation: 3, Task: "v", Position: 1, Role: "implementer", AgentKey: tc.key},
				{Generation: 3, Task: "v", Position: 2, Role: "reviewer"},
			})
			if got[0].Agent != tc.want {
				t.Errorf("key %s named %q, want %q", tc.key, got[0].Agent, tc.want)
			}
		})
	}
}

// A numbered name that only agents with no key held is given again: generation 1's task a ran
// `implementer reviewer implementer`, its step 3 on implementer-2, and a later version's new task
// b with the same steps names its step 3 implementer-2 too, rather than implementer-3, so the run
// needs no new slot and its pod no new round; the claim implementer-2 starts that agent in a
// session of its own (supervise.Claim.SessionAgent). A numbered name a key held is not: where key
// c held implementer-2, task b's step 3 takes implementer-3, and the name waits for key c. The
// fault: every numbered name an earlier row held was never given again, so each later agent with
// no key of the role took a new number.
func TestANumberedNameOnlyAgentsWithNoKeyHeldIsGivenAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want claim.Agent
	}{
		{name: "implementer-2 held by a step with no key", want: "implementer-2"},
		{name: "control: implementer-2 held by key c", key: "c", want: "implementer-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlier := []PlanStep{
				{Generation: 1, Task: "a", Position: 1, Role: "implementer", Agent: "implementer"},
				{Generation: 1, Task: "a", Position: 2, Role: "reviewer", Agent: "reviewer"},
				{Generation: 1, Task: "a", Position: 3, Role: "implementer", AgentKey: tc.key, Agent: "implementer-2"},
			}
			got := NameAgents(earlier, nil, []PlanStep{
				{Generation: 1, Task: "b", Position: 1, Role: "implementer"},
				{Generation: 1, Task: "b", Position: 2, Role: "reviewer"},
				{Generation: 1, Task: "b", Position: 3, Role: "implementer"},
			})
			if names := []claim.Agent{got[0].Agent, got[1].Agent, got[2].Agent}; !slices.Equal(names, []claim.Agent{"implementer", "reviewer", tc.want}) {
				t.Errorf("task b beside task a's rows: names %q, want implementer, reviewer and %s", names, tc.want)
			}
		})
	}
}

// An agent named by pass 4 passes over a name that any earlier row of the issue held on another
// role, at its own place or at any other, so the name of a second agent of a role never equals the
// name of an agent of another role. At its own place, that name's claim would also resume the
// session of the place's agent of the other role, since the agent of a step with no key is
// `step:<place>:<task>` whatever its role. Two roles give one name where a role is named as a
// numbered agent of another, as `planner-2` is of the planner, and where two roles of 30 bytes or
// more agree in their first 30, which claim.NumberedAgentName cuts to one name. The controls: the
// same role at the place keeps its name (pass 3); a name held only on the agent's own role is given
// in another task, as a stopped task's agent's name is; and a key that holds a name keeps it,
// whatever roles its rows were on. The faults: version 2's second planner took planner-2, which
// the role planner-2's agent held, at that agent's place or at another.
func TestAnAgentPassesOverANameAnotherRoleHeld(t *testing.T) {
	steps := func(task string, words ...string) []PlanStep {
		named := make([]PlanStep, len(words))
		for i, word := range words {
			role, key, _ := strings.Cut(word, ":")
			named[i] = PlanStep{Generation: 1, Task: task, Position: i + 1, Role: role, AgentKey: key}
		}
		return named
	}
	long := strings.Repeat("r", 30)
	for _, tc := range []struct {
		name          string
		first, second []PlanStep
		want          []claim.Agent
	}{
		{name: "a role named as the planner's second agent, at its place",
			first: steps("t", "implementer", "planner-2"), second: steps("t", "planner", "planner", "implementer"),
			want: []claim.Agent{"planner", "planner-3", "implementer"}},
		{name: "a role named as the planner's second agent, at another place",
			first: steps("t", "implementer", "planner-2"), second: steps("t", "planner", "reviewer", "planner"),
			want: []claim.Agent{"planner", "reviewer", "planner-3"}},
		{name: "two roles that a cut of 30 bytes makes one, at the place",
			first: steps("t", long+"-a", long+"-a"), second: steps("t", long+"-b", long+"-b"),
			want: []claim.Agent{claim.Agent(long + "-b"), claim.Agent(long + "-3")}},
		{name: "two roles that a cut of 30 bytes makes one, in another task",
			first: steps("t", long+"-a", long+"-a"), second: steps("u", long+"-b", long+"-b"),
			want: []claim.Agent{claim.Agent(long + "-b"), claim.Agent(long + "-3")}},
		{name: "a new key whose first step is the planner's",
			first: steps("t", "implementer", "planner-2"), second: steps("t", "planner", "planner:j", "implementer"),
			want: []claim.Agent{"planner", "planner-3", "implementer"}},
		{name: "control: the same role at the place",
			first: steps("t", "implementer", "reviewer", "implementer"), second: steps("t", "implementer", "reviewer", "implementer"),
			want: []claim.Agent{"implementer", "reviewer", "implementer-2"}},
		{name: "control: the agent's own role in another task",
			first: steps("t", "implementer", "reviewer", "implementer"), second: steps("u", "implementer", "reviewer", "implementer"),
			want: []claim.Agent{"implementer", "reviewer", "implementer-2"}},
		{name: "control: a key that holds a name its rows held on two roles",
			first: steps("t", "implementer", "planner:k", "reviewer:k"), second: steps("t", "implementer", "reviewer:k", "planner"),
			want: []claim.Agent{"implementer", "planner", "planner-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := NameAgents(nil, nil, tc.first)
			if got := agentNames(NameAgents(first, nil, tc.second)); !slices.Equal(got, tc.want) {
				t.Errorf("version 2 after version 1 named %q: names %q, want %q", agentNames(first), got, tc.want)
			}
		})
	}
}

// A step with no key takes a name even where the latest earlier row of its task, place and role
// holds none: its role, or a name of its own where another agent of the version holds the role.
// The first form of the migration that added plan_steps.agent, 20261123203837, left every row it
// found at the empty string, and a row that holds no name now reads as no name
// (PlanStep.AgentName), so a database that ran that form holds such rows of steps that are not
// the owner's. A step named "" has no claim token (StepToken). The control: rows that hold names
// still give them to the steps at their places. The fault: pass 3 gave each step at the place of
// such a row the empty name.
func TestAStepWithNoKeyTakesANameWhereItsPlacesRowHoldsNone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		earlier [2]claim.Agent
		want    []claim.Agent
	}{
		{name: "rows the first form of the migration left with no name", want: []claim.Agent{"implementer", "reviewer", "implementer-2", ""}},
		{name: "control: rows that hold names", earlier: [2]claim.Agent{"implementer-2", "reviewer"}, want: []claim.Agent{"implementer-2", "reviewer", "implementer", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlier := []PlanStep{
				{Generation: 1, Task: "t", Position: 1, Role: "implementer", Agent: tc.earlier[0]},
				{Generation: 1, Task: "t", Position: 2, Role: "reviewer", Agent: tc.earlier[1]},
				{Generation: 1, Task: "t", Position: 3, Role: "acceptance", AgentKey: "owner"},
			}
			got := NameAgents(earlier, nil, []PlanStep{
				{Generation: 1, Task: "t", Position: 1, Role: "implementer"},
				{Generation: 1, Task: "t", Position: 2, Role: "reviewer"},
				{Generation: 1, Task: "t", Position: 3, Role: "implementer"},
				{Generation: 1, Task: "t", Position: 4, Role: "acceptance", AgentKey: "owner"},
			})
			if !slices.Equal(agentNames(got), tc.want) {
				t.Fatalf("the names = %q, want %q", agentNames(got), tc.want)
			}
			for _, step := range got {
				if _, err := StepToken("legion", "LEGION-209", "LEGION-208", step); err != nil {
					t.Errorf("step %d, %s, has no claim token: %v", step.Position, step.Role, err)
				}
			}
		})
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

<<<<<<< HEAD
=======
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

>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
// A key's name is its newest: rows of an earlier generation that named it otherwise do not move it
// within a generation. Generation 2's default steps are named here from no earlier rows, so key a
// takes its first role, implementer; the owner's version of that generation keeps that name, and
// not the name tester that key a had in generation 1.
func TestAKeysNameIsItsNewestAcrossGenerations(t *testing.T) {
	first := ofGeneration(1, NameAgents(nil, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "implementer"},
<<<<<<< HEAD
	}))
	defaults := ofGeneration(2, NameAgents(nil, nil, []PlanStep{
=======
	}, nil))
	defaults := ofGeneration(2, NameAgents(nil, []PlanStep{
>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil))
	earlier := append(slices.Clone(first), defaults...)
	got := NameAgents(earlier, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "tester", AgentKey: "a"},
		{Task: "t", Position: 3, Role: "reviewer"},
	}, nil)
	if want := []claim.Agent{"implementer", "implementer", "reviewer"}; !slices.Equal(agentNames(got), want) {
		t.Fatalf("the names = %q, want %q: key a keeps the name its generation's default steps gave it", agentNames(got), want)
	}
}

<<<<<<< HEAD
=======
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

>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
// Rows of the key owner in earlier name no agent: the owner's step of a later version still has no
// name, and no other key or step takes a name from the owner's rows.
func TestTheOwnersEarlierRowsNameNoAgent(t *testing.T) {
	earlier := ofGeneration(1, NameAgents(nil, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer"},
		{Task: "t", Position: 2, Role: "acceptance", AgentKey: "owner"},
	}, nil))
	earlier[1].Agent = "acceptance"
	got := NameAgents(earlier, nil, []PlanStep{
		{Task: "t", Position: 1, Role: "implementer", AgentKey: "a"},
		{Task: "t", Position: 2, Role: "acceptance", AgentKey: "owner"},
	}, nil)
	if want := []claim.Agent{"implementer", ""}; !slices.Equal(agentNames(got), want) {
		t.Fatalf("the names = %q, want %q: the owner has no name whatever its earlier rows hold", agentNames(got), want)
	}
}

<<<<<<< HEAD
=======
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

>>>>>>> 92fa6581b8d4c64f13b4f7fb8241f6ce08133fe5
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

// Two roles of 30 bytes or more that agree in their first 30 cut to one numbered name
// (claim.NumberedAgentName), and a version replaces the rows that showed the name held on the
// other role. A name whose stored claim holds another role is passed over all the same, since that
// claim's session is the other role's agent's. With role A, 30 r's and -a, and role B, 30 r's and
// -b: version 1 gives task t `A A`, so step 2 runs as rrr…r-2 and its claim holds A; version 2,
// `B reviewer`, replaces version 1's rows; version 3, `B B`, must not give step 2 rrr…r-2, whose
// claim would resume A's agent's session in a step of B. The control: where the claim of that name
// holds B itself, the name is free for B's agent. The fault: pass 4 read only the earlier rows, found
// rrr…r-2 held on no other role, and gave it to B's step 2.
func TestANameWhoseClaimHoldsAnotherRoleIsNotGivenToAnAgentOfALongRole(t *testing.T) {
	a, b := strings.Repeat("r", 30)+"-a", strings.Repeat("r", 30)+"-b"
	cut := claim.Agent(strings.Repeat("r", 30) + "-2")
	if claim.NumberedAgentName(claim.Role(a), 2) != cut || claim.NumberedAgentName(claim.Role(b), 2) != cut {
		t.Fatalf("the second agents of %s and %s are named %q and %q, want both %q: the case's premise", a, b,
			claim.NumberedAgentName(claim.Role(a), 2), claim.NumberedAgentName(claim.Role(b), 2), cut)
	}
	token, err := claim.AgentToken("legion", "LEGION-208", cut)
	if err != nil {
		t.Fatal(err)
	}
	v2 := NameAgents(nil, nil, []PlanStep{
		{Generation: 1, Task: "t", Position: 1, Role: b},
		{Generation: 1, Task: "t", Position: 2, Role: "reviewer"},
	})
	v3 := []PlanStep{
		{Generation: 1, Task: "t", Position: 1, Role: b},
		{Generation: 1, Task: "t", Position: 2, Role: b},
	}
	for _, tc := range []struct {
		name  string
		holds string
		taken bool
	}{
		{name: "the claim holds the other role", holds: a},
		{name: "control: the claim holds the step's own role", holds: b, taken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := StoredClaimsOf([]IssueClaim{{Token: token, Role: claim.Role(tc.holds), SessionAgent: "step:2:t"}})
			got := NameAgents(v2, claims, v3)
			if taken := got[1].Agent == cut; taken != tc.taken {
				t.Errorf("version 3's step 2, of %s, beside the claim %s of %s: name %q, want %s taken = %v", b, cut, tc.holds,
					got[1].Agent, cut, tc.taken)
			}
		})
	}
}
