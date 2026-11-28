package record

import (
	"maps"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// StoredClaims is what NameAgents keeps of the stored claims of one issue (StoredClaimsOf), by the
// name each claim's token ends in: the role the claim holds and the key whose agent's session it
// holds (NameClaim).
type StoredClaims map[claim.Agent]NameClaim

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

// StoredClaimsOf is the StoredClaims of claims, the stored claims of one issue (Store.IssueClaims):
// each claim by the name its token ends in, with its role, and with the key whose agent's session
// it holds where that key is not the key owner's.
func StoredClaimsOf(claims []IssueClaim) StoredClaims {
	sessions := StoredClaims{}
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

// NameAgents copies steps and names each step's agent. earlier is every stored step of the
// issue, sessions is its stored claims by name, and held is the lasting record of every name a
// key held. Every writer supplies all three histories before replacing any plan row.
//
// It names the owner first, with no issue-local name; keeps each key's last name next; then keeps
// a step's earlier name at its task, place and role where no key or earlier agent holds it. Each
// still-unnamed agent takes its role's name or the first free numbered name of that role. Names a
// key held stay reserved even after its rows and claims are gone, and no new agent takes a name
// that another role's claim or earlier row held. The steps of one key share one name, while two
// agents of one role get different names and therefore different claims and sessions.
func NameAgents(earlier []PlanStep, sessions StoredClaims, steps []PlanStep, held []KeyName) []PlanStep {
	named := slices.Clone(steps)
	keyNames := map[string]claim.Agent{}
	// lastHolder is the key that holds each name a key holds: the key of the latest earlier row of
	// the name that names a key, else the key whose agent's session the name's claim holds.
	lastHolder := map[claim.Agent]string{}
	keyHeld := func(name claim.Agent) bool {
		_, ok := lastHolder[name]
		return ok
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
	// The lasting record reserves names after their plan rows and claims are gone.
	for _, h := range held {
		if h.AgentKey == "" || h.Name == "" {
			continue
		}
		if _, ok := keyNames[h.AgentKey]; !ok {
			keyNames[h.AgentKey] = h.Name
		}
		if _, ok := lastHolder[h.Name]; !ok {
			lastHolder[h.Name] = h.AgentKey
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
	for _, name := range slices.Sorted(maps.Keys(sessions)) {
		held := sessions[name]
		holdOn(name, string(held.Role))
		if held.Key == "" {
			continue
		}
		if _, ok := keyNames[held.Key]; !ok {
			keyNames[held.Key] = name
		}
		if _, ok := lastHolder[name]; !ok {
			lastHolder[name] = held.Key
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

// KeyName is one name a key held on an issue (KeyNames): the lasting record of every name a key's
// agent held, which no later plan's write or deletion removes, since the name's claim records that
// key's session in every later generation. Generation is the latest generation whose steps gave
// the key that name, which says whether a row with no key that shows the name came before the key
// held it or after (workflow's handedBefore).
type KeyName struct {
	Name       claim.Agent
	AgentKey   string
	Generation uint64
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
