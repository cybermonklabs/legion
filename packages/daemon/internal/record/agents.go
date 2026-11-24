package record

import (
	"maps"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// KeySessions is, by the name of each claim of an issue whose session is the session of a key's
// agent (supervise.Claim.SessionAgent, `key:<key>`), that key: the names NameAgents keeps for a key
// where no row names the key any longer.
type KeySessions map[claim.Agent]string

// KeySessionsOf is the KeySessions of claims, the stored claims of one issue (Store.IssueClaims):
// each claim whose session is the session of a key's agent other than the key owner, by the name
// its token ends in. A claim whose session is a step's agent with no key, or that records none,
// keeps no name.
func KeySessionsOf(claims []IssueClaim) KeySessions {
	sessions := KeySessions{}
	for _, c := range claims {
		key, keyed := c.SessionAgent.Key()
		_, name, ok := c.Token.Cut()
		if keyed && ok && !(plan.Agent{Key: key}).Owner() {
			sessions[name] = key
		}
	}
	return sessions
}

// NameAgents is steps, copied, with the name of each step's agent (PlanStep.Agent) given: the one
// rule of a name, which reads no store of its own. earlier is every plan_steps row of the issue, of
// every generation (PlanStepsOf), read before the version's steps are written, since the write
// deletes the steps of each task it gives. sessions is the names whose claims hold a key's session
// (KeySessionsOf), read with earlier. Every writer of steps names them from both: a version an
// owner gives and the project's default steps a run with no plan takes. It names in four passes,
// in this order, so that a key the version names never changes its name and no new name can take
// that key's. A name a key held stays that key's while a version leaves the key out: no other
// agent takes it, so the name's claim, and the session that claim holds, wait for the key's return,
// and the agent that would have taken the name takes one of its own (`implementer-2`) and so a slot
// of its own. A key holds the name an earlier row of it held, and, since a version replaces the
// rows of each task it gives and so can leave no row that names the key, the name whose claim holds
// its session. That holds whatever release wrote the rows: one before the claim recorded its
// session's agent left the claim's session to the agent of the next start that reached it, so a
// row alone is all that says the name was the key's. A name only agents with no key held is free,
// a numbered one as much as the role's own: a new task's agent with no key takes the name of a
// stopped task's agent of its role, and the claim of that name starts it in a session of its own
// (supervise.Claim.SessionAgent), so each later agent with no key of a role needs no new number, and
// so no new slot of its pod.
//
//  1. A step of the key owner has no name: its agent is an architect, the issue's own
//     (OwnerClaim) or one above that runs the step for it (workflow's ownerAgent), whose token
//     names its issue and its role and no name of an agent.
//  2. Each other key that an earlier row names takes the name of its latest such row, and a key no
//     earlier row names takes the name whose claim holds its session, each only where it is the
//     key that held that name last. The key that held a name last is the key of the latest earlier
//     row of the name that names a key, and a later row of the name with no key does not end that
//     hold; where no earlier row of the name names a key, it is the key whose session the name's
//     claim holds. Every such key is named before any other pass gives a name. Rows written
//     before a name a key held was kept for it can show another key holding the name after it,
//     where that key took by pass 4 the name of a key its version did not name; the name's claim
//     ran that other key's agent last, so the key whose row held it before is named by pass 4,
//     whether or not the version names the other key, and no name passes from one key to another.
//  3. Each step with no key takes the name of the latest earlier row of its task, place and role
//     with no key, where that row holds one, unless pass 2 or an earlier step of this pass gave
//     that name, or a key holds it. A row that holds no name gives none: the first form of
//     20261117213545 left every row it found at the empty string, and a step named "" has no
//     claim token, so such a step is named by pass 4.
//  4. Each agent still with no name, in the order of its first step, takes the role of that first
//     step, cut to 32 bytes (claim.RoleAgentName), where no agent of steps has that name and no key
//     holds it; and otherwise a name of its own, `<role>-2`, `<role>-3` and so on, the first that
//     no agent of steps and no key holds (claim.NumberedAgentName), so two agents never share a
//     name, and so a claim and a session.
//
// The steps of one key are one agent (PlanStep.PlanAgent), so they share one name in every task.
func NameAgents(earlier []PlanStep, sessions KeySessions, steps []PlanStep) []PlanStep {
	named := slices.Clone(steps)
	keyNames := map[string]claim.Agent{}
	// lastHolder is the key that holds each name a key holds: the key of the latest earlier row of
	// the name that names a key, else the key whose agent's session the name's claim holds.
	lastHolder := map[claim.Agent]string{}
	keyHeld := func(name claim.Agent) bool {
		_, ok := lastHolder[name]
		return ok
	}
	for _, row := range earlier {
		if agent := row.PlanAgent(); agent.Key != "" && !agent.Owner() && row.AgentName() != "" {
			keyNames[agent.Key] = row.AgentName()
			lastHolder[row.AgentName()] = agent.Key
		}
	}
	for _, name := range slices.Sorted(maps.Keys(sessions)) {
		key := sessions[name]
		if _, ok := keyNames[key]; !ok {
			keyNames[key] = name
		}
		if _, ok := lastHolder[name]; !ok {
			lastHolder[name] = key
		}
	}
	names := map[plan.Agent]claim.Agent{}
	taken := map[claim.Agent]bool{}
	give := func(agent plan.Agent, name claim.Agent) {
		names[agent] = name
		if name != "" {
			taken[name] = true
		}
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if _, done := names[agent]; done {
			continue
		}
		if agent.Owner() {
			give(agent, "")
		} else if name, ok := keyNames[agent.Key]; ok && agent.Key != "" && lastHolder[name] == agent.Key {
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if agent.Key != "" {
			continue
		}
		if name := earlierPlaceName(earlier, step); name != "" && !taken[name] && !keyHeld(name) {
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if _, ok := names[agent]; ok {
			continue
		}
		// The role's own name, or else the first numbered one, where no agent of steps and no key
		// holds it: an earlier row's agent with no key that the version no longer names frees it.
		name := claim.RoleAgentName(claim.Role(step.Role))
		for n := claim.FirstAgentNumber; taken[name] || keyHeld(name); n++ {
			name = claim.NumberedAgentName(claim.Role(step.Role), n)
		}
		give(agent, name)
	}
	for i := range named {
		named[i].Agent = names[named[i].PlanAgent()]
	}
	return named
}

// earlierPlaceName is the name of the latest row of earlier at step's task and place, on step's
// role and with no key: the agent that place had, which a step with no key keeps (pass 3). It is
// "" where no such row is, or where that row holds no name.
func earlierPlaceName(earlier []PlanStep, step PlanStep) claim.Agent {
	for i := len(earlier) - 1; i >= 0; i-- {
		row := earlier[i]
		if row.AgentKey == "" && row.Task == step.Task && row.Position == step.Position && row.Role == step.Role {
			return row.AgentName()
		}
	}
	return ""
}
