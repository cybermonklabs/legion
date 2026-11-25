package record

import (
	"maps"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// KeySessions is what NameAgents keeps of the stored claims of one issue (KeySessionsOf), by the
// name each claim's token ends in: the role the claim holds and the key whose agent's session it
// holds (NameClaim).
type KeySessions map[claim.Agent]NameClaim

// NameClaim is what the stored claim of a name holds, for NameAgents: Role, the role the claim
// holds, and Key, the key whose agent's session the claim holds (supervise.Claim.SessionAgent,
// `key:<key>`), "" for a claim whose session is a step's agent with no key, the key owner's, or
// none. A name whose claim holds a key's session is the name NameAgents keeps for that key where no
// row names the key any longer (pass 2), and a name whose claim holds another role is no name for
// an agent of a role (pass 4), since that claim's session is the other role's agent's.
type NameClaim struct {
	Role claim.Role
	Key  string
}

// KeySessionsOf is the KeySessions of claims, the stored claims of one issue (Store.IssueClaims):
// each claim by the name its token ends in, with its role, and with the key whose agent's session
// it holds where that key is not the key owner's.
func KeySessionsOf(claims []IssueClaim) KeySessions {
	sessions := KeySessions{}
	for _, c := range claims {
		_, name, ok := c.Token.Cut()
		if !ok {
			continue
		}
		held := NameClaim{Role: c.Role}
		if key, keyed := c.SessionAgent.Key(); keyed && !(plan.Agent{Key: key}).Owner() {
			held.Key = key
		}
		sessions[name] = held
	}
	return sessions
}

// KeyName is one name a key held on an issue (KeyNames): the lasting record of every name a key's
// agent held, which no later plan's write or deletion removes, since the name's claim records that
// key's session in every later generation. Generation is the latest generation whose steps gave
// the key that name.
type KeyName struct {
	Name       claim.Agent
	AgentKey   string
	Generation uint64
}

// NameAgents is steps, copied, with the name of each step's agent (PlanStep.Agent) given: the one
// rule of a name, which reads no store of its own. earlier is every plan_steps row of the issue, of
// every generation (PlanStepsOf), read before the version's steps are written, since the write
// deletes the steps of each task it gives; held is the lasting record of every name a key held on
// the issue (KeyNames), which outlives the rows; sessions is the stored claims of the issue by name
// (KeySessionsOf), read with earlier. All three are required, and a caller with no history passes
// nil for each: a writer that could leave held or sessions out would reserve the names by the rows
// alone, and a name whose rows went would be given to another agent. Every writer of steps names
// them from all three: a version an owner gives and the project's default steps a run with no plan
// takes. It names in four passes, in this order, so that a key the version names never changes its
// name and no new name can take that key's. A name a key held stays that key's while a version
// leaves the key out: no other agent takes it, so the name's claim, and the session that claim
// holds, wait for the key's return, and the agent that would have taken the name takes one of its
// own (`implementer-2`) and so a slot of its own. A key holds the name an earlier row of it held,
// and, since a version replaces the rows of each task it gives and so can leave no row that names
// the key, the name the lasting record holds for it, and the name whose claim holds its session
// (KeySessionsOf). That holds whatever release wrote the rows: a claim of a release before the
// claim recorded its session's agent records, once 20261125053850 has run, the agent
// that ran on it last where that release kept its latest start, else the agent the rows gave its
// name last, so where that agent is a key's, the name stays the key's after the last row that said
// so is gone. Where the agent recorded is one with no key that the release before gave a key's name
// in a later version, the name stays the key's only while the key's earlier row, or its lasting
// record, stands. A name only agents with no key held is free for their role, a numbered one as
// much as the role's own: a new task's agent with no key takes the name of a stopped task's agent
// of its role, and the claim of that name starts it in a session of its own
// (supervise.Claim.SessionAgent), so each later agent with no key of a role needs no new number,
// and so no new slot of its pod.
//
//  1. A step of the key owner has no name: its agent is an architect, the issue's own
//     (OwnerClaim) or one above that runs the step for it (workflow's ownerAgent), whose token
//     names its issue and its role and no name of an agent.
//  2. Each other key that an earlier row names takes the name of its latest such row, and a key no
//     earlier row names takes the name the lasting record holds for it, else the name whose claim
//     holds its session, each only where it is the key that held that name last. The key that held
//     a name last is the key of the latest earlier row of the name that names a key, and a later
//     row of the name with no key does not end that hold; where no earlier row of the name names a
//     key, it is the key the lasting record holds the name for, else the key whose session the
//     name's claim holds. Every such key is named before any other pass gives a name. Rows written
//     before a name a key held was kept for it can show another key holding the name after it,
//     where that key took by pass 4 the name of a key its version did not name; the name's claim
//     ran that other key's agent last, so the key whose row held it before is named by pass 4,
//     whether or not the version names the other key, and no name passes from one key to another.
//  3. Each step with no key takes the name of the latest earlier row of its task, place and role
//     with no key, where that row holds one, unless pass 2 or an earlier step of this pass gave
//     that name, or a key holds it. A row that holds no name gives none: the first form of
//     20261124213820 left every row it found at the empty string, and a step named "" has no
//     claim token, so such a step is named by pass 4.
//  4. Each agent still with no name, in the order of its first step, takes the role of that first
//     step, cut to 32 bytes (claim.RoleAgentName), where no agent of steps has that name, no key
//     holds it and neither an earlier row nor the name's stored claim held it on another role; and
//     otherwise a name of its own, `<role>-2`, `<role>-3` and so on, the first that no agent of
//     steps and no key holds and that neither an earlier row nor its claim held on another role
//     (claim.NumberedAgentName). So two agents never share a name, and so a claim and a session,
//     and an agent of one role never takes the name an agent of another role had. Two roles give
//     one name where a role is named as another role's numbered agent, which the settings loader
//     and plan.Validate refuse but rows written under earlier settings can hold, and where two
//     roles of 30 bytes or more agree in their first 30, which claim.NumberedAgentName cuts to one
//     name. The claim of such a name holds the role whose agent ran on it, so the name stays that
//     role's while the claim stands, after the versions that replaced every row of it: its
//     session is that agent's, and where that agent was a step's at the same task and place, the
//     claim would resume it, since the agent of a step with no key is `step:<place>:<task>`
//     whatever its role. Pass 3 never gives a name this pass passes over: it gives a step only a
//     name its own role held at its place, and in the rows this rule and the release before
//     write, a name stands on two roles only where a key's agent moved to another role under it,
//     a name keyHeld passes over while those rows stand. A key that holds a name keeps it, since
//     pass 2 names it first.
//
// The steps of one key are one agent (PlanStep.PlanAgent), so they share one name in every task.
func NameAgents(earlier []PlanStep, held []KeyName, sessions KeySessions, steps []PlanStep) []PlanStep {
	named := slices.Clone(steps)
	keyNames := map[string]claim.Agent{}
	// lastHolder is the key that holds each name a key holds: the key of the latest earlier row of
	// the name that names a key, else the key the lasting record holds it for, else the key whose
	// agent's session the name's claim holds.
	lastHolder := map[claim.Agent]string{}
	keyHeld := func(name claim.Agent) bool {
		_, ok := lastHolder[name]
		return ok
	}
	// keepFor names key by name, and name by key, where no earlier source named either.
	keepFor := func(key string, name claim.Agent) {
		if _, ok := keyNames[key]; !ok {
			keyNames[key] = name
		}
		if _, ok := lastHolder[name]; !ok {
			lastHolder[name] = key
		}
	}
	// heldOn is the roles each name was held on, by the earlier rows that hold it and by its stored
	// claim (pass 4).
	heldOn := map[claim.Agent]map[string]bool{}
	holdOn := func(name claim.Agent, role string) {
		if heldOn[name] == nil {
			heldOn[name] = map[string]bool{}
		}
		heldOn[name][role] = true
	}
	for _, row := range earlier {
		if agent := row.PlanAgent(); agent.Key != "" && !agent.Owner() && row.AgentName() != "" {
			keyNames[agent.Key] = row.AgentName()
			lastHolder[row.AgentName()] = agent.Key
		}
		if name := row.AgentName(); name != "" {
			holdOn(name, row.Role)
		}
	}
	heldOnAnotherRole := func(name claim.Agent, role string) bool {
		for held := range heldOn[name] {
			if held != role {
				return true
			}
		}
		return false
	}
	for _, h := range held {
		if h.AgentKey == "" || h.Name == "" || (plan.Agent{Key: h.AgentKey}).Owner() {
			continue
		}
		keepFor(h.AgentKey, h.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(sessions)) {
		held := sessions[name]
		holdOn(name, string(held.Role))
		if held.Key == "" {
			continue
		}
		keepFor(held.Key, name)
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
		// holds it and neither an earlier row nor the name's claim held it on another role: an
		// earlier row's agent with no key of this role that the version no longer names frees it.
		name := claim.RoleAgentName(claim.Role(step.Role))
		for n := claim.FirstAgentNumber; taken[name] || keyHeld(name) ||
			heldOnAnotherRole(name, step.Role); n++ {
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
