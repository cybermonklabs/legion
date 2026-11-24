package record

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// NameAgents is steps, copied, with the name of each step's agent (PlanStep.Agent) given: the one
// rule of a name, which reads no store of its own. earlier is every plan_steps row of the issue, of
// every generation (PlanStepsOf), read before the version's steps are written, since the write
// deletes the steps of each task it gives; held is the lasting record of every name a key held on
// the issue (KeyNames), which outlives the rows. A production writer passes both; a caller with no
// history passes neither. Every writer of steps names them so: a version an owner gives
// (workflow's versionRefusal) and the project's default steps a run with no plan takes (workflow's
// defaultRows). It names in four passes, in this order, so that a key the version names never
// changes its name and no two agents of the version share one.
//
// A name a key held is that key's alone, in every later version and generation, whatever release
// wrote the rows (keyHolders, from earlier and held by one rule): no step with no key and no other
// key takes it, so no agent but that key ever runs in the name's claim and resumes its session. A
// key the version no longer names keeps its name for its return; the agent that would have taken
// it is left with no name.
//
//  1. A step of the key owner has no name: its agent is the tree's architect, whose token names the
//     tree and no name on the issue.
//  2. Each other key that an earlier row names takes the name of its newest such row
//     (earlierKeyNames), where it is the key that holds that name (keyHolders) and no agent named
//     before it in this pass took it; otherwise it keeps no name. Every such key is named before
//     any other pass gives a name.
//  3. Each step with no key takes the name of the latest earlier row of its task, place and role
//     with no key, unless pass 2 or an earlier step of this pass gave that name, or a key holds it.
//  4. Each agent still with no name, in the order of its first step, takes the role of that first
//     step (RoleName), where no agent of steps has that name and no other key holds it.
//
// An agent left with no name would share its name with another agent, or take a key's: this
// release runs one agent of each role for an issue, so the caller refuses an owner's version with
// one before it writes anything (UnnamedAgents), and gives the default steps of a new generation
// the name with a session of their own (workflow's defaultRows).
//
// The steps of one key are one agent (PlanStep.PlanAgent), so they share one name in every task.
func NameAgents(earlier, steps []PlanStep, held ...KeyName) []PlanStep {
	named := slices.Clone(steps)
	keyNames := earlierKeyNames(earlier, held)
	holders := keyHolders(earlier, held)
	names := map[plan.Agent]claim.Agent{}
	taken := map[claim.Agent]bool{}
	give := func(agent plan.Agent, name claim.Agent) {
		names[agent] = name
		if name != "" {
			taken[name] = true
		}
	}
	// heldByAnother is whether a key other than key holds name.
	heldByAnother := func(name claim.Agent, key string) bool {
		holder, held := holders[name]
		return held && holder != key
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if _, done := names[agent]; done {
			continue
		}
		if agent.Owner() {
			give(agent, "")
			continue
		}
		name, ok := keyNames[agent.Key]
		switch {
		case agent.Key == "" || !ok:
		case taken[name] || heldByAnother(name, agent.Key):
			give(agent, "")
		default:
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if agent.Key != "" {
			continue
		}
		if name, ok := earlierPlaceName(earlier, step); ok && !taken[name] && !heldByAnother(name, "") {
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := step.PlanAgent()
		if _, ok := names[agent]; ok {
			continue
		}
		name := RoleName(step.Role)
		if taken[name] || heldByAnother(name, agent.Key) {
			give(agent, "")
			continue
		}
		give(agent, name)
	}
	for i := range named {
		named[i].Agent = names[named[i].PlanAgent()]
	}
	return named
}

// RoleName is the name an agent takes from the role of its first step (NameAgents' pass 4): the
// role itself. A step's role is a role of the deployment's or one of Legion's (plan.Supported
// refuses a plan's own role block), and config reads a role of the deployment's in the agent
// grammar already (its roleWord, at most maxRoleWord bytes), so a role needs no cut to be a name.
func RoleName(role string) claim.Agent { return claim.Agent(role) }

// UnnamedAgent is an agent of a version that NameAgents gave no name: the agent's first step, the
// name it would have taken, and Holder, the key that holds that name ("" where the agent of the
// version that holds it has no key).
type UnnamedAgent struct {
	Step   PlanStep
	Name   claim.Agent
	Holder string
}

// UnnamedAgents is each agent of named, steps NameAgents gave from earlier and held, but the owner,
// that has no name, with the name it would have taken — a key's newest earlier name
// (earlierKeyNames), and otherwise its role's (RoleName) — and the key that holds that name: the
// key of the agent of named that holds it, and otherwise the key whose earlier row, or the lasting
// record, holds it (keyHolders). This release runs one agent of each role for an issue, and a name
// a key held is that key's alone, so a version with one is refused before any write.
func UnnamedAgents(earlier, named []PlanStep, held ...KeyName) []UnnamedAgent {
	keyNames := earlierKeyNames(earlier, held)
	holders := keyHolders(earlier, held)
	var unnamed []UnnamedAgent
	seen := map[plan.Agent]bool{}
	for _, step := range named {
		agent := step.PlanAgent()
		if seen[agent] || agent.Owner() {
			continue
		}
		seen[agent] = true
		if step.Agent != "" {
			continue
		}
		name, ok := keyNames[agent.Key]
		if agent.Key == "" || !ok {
			name = RoleName(step.Role)
		}
		holder := holders[name]
		if at := slices.IndexFunc(named, func(other PlanStep) bool { return other.Agent == name }); at >= 0 {
			holder = named[at].AgentKey
		}
		unnamed = append(unnamed, UnnamedAgent{Step: step, Name: name, Holder: holder})
	}
	return unnamed
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

// earlierKeyNames is the name each key but the owner's held, read newest first: the name of the
// key's row of the latest generation of earlier that names it, and for a key no row of earlier
// names, the name the lasting record holds for it (held). earlier is ordered by generation, as
// PlanStepsOf reads it, and the steps of one key in one generation share one name, so a key named
// again since an earlier generation keeps the name it holds now and not its oldest.
func earlierKeyNames(earlier []PlanStep, held []KeyName) map[string]claim.Agent {
	names := map[string]claim.Agent{}
	for i := len(earlier) - 1; i >= 0; i-- {
		row := earlier[i]
		// AgentName is "" for a step of the key owner, as for a row that holds no name.
		name := row.AgentName()
		if row.AgentKey == "" || name == "" {
			continue
		}
		if _, ok := names[row.AgentKey]; !ok {
			names[row.AgentKey] = name
		}
	}
	for _, h := range held {
		if _, ok := names[h.AgentKey]; !ok && h.AgentKey != "" && h.Name != "" {
			names[h.AgentKey] = h.Name
		}
	}
	return names
}

// keyHolders is, by each name a key but the owner's held, the key that holds it: the key of the
// latest row of earlier of the name that names a key, and for a name no such row holds, the key
// the lasting record holds it for (held). A plan's rows go when a later version gives the task
// again or the plan of an earlier owner is passed over, and the record stays, so the record is what
// keeps a name a key held that key's after its rows went. A name a key held is that key's alone
// (NameAgents), so in rows NameAgents wrote only that key's rows hold it; rows an earlier build
// wrote may show another key at it later, and then the later key holds it, since the name's claim
// ran that key's agent last.
func keyHolders(earlier []PlanStep, held []KeyName) map[claim.Agent]string {
	holders := map[claim.Agent]string{}
	for i := len(earlier) - 1; i >= 0; i-- {
		row := earlier[i]
		name := row.AgentName()
		if row.AgentKey == "" || name == "" {
			continue
		}
		if _, ok := holders[name]; !ok {
			holders[name] = row.AgentKey
		}
	}
	for _, h := range held {
		if _, ok := holders[h.Name]; !ok && h.AgentKey != "" && h.Name != "" {
			holders[h.Name] = h.AgentKey
		}
	}
	return holders
}

// earlierPlaceName is the name of the latest row of earlier at step's task and place, on step's
// role and with no key: the agent that place had, which a step with no key keeps (pass 3).
func earlierPlaceName(earlier []PlanStep, step PlanStep) (claim.Agent, bool) {
	for i := len(earlier) - 1; i >= 0; i-- {
		row := earlier[i]
		if row.AgentKey == "" && row.Task == step.Task && row.Position == step.Position && row.Role == step.Role {
			return row.AgentName(), true
		}
	}
	return "", false
}
