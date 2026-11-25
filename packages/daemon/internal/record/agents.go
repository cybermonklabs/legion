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
// the issue (KeyNames), which outlives the rows. Both are required, and a caller with no history
// passes nil for each: a writer that could leave held out would reserve the names by the rows
// alone, and a name whose rows went would be given to another agent. Every writer of steps names
// them so: a version an owner gives
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
//  2. Each other key that an earlier row names takes the name of its newest such row, where it is
//     the key that holds that name (keyNamesHeld answers both) and no agent named
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
func NameAgents(earlier, steps []PlanStep, held []KeyName) []PlanStep {
	named := slices.Clone(steps)
	keyNames, holders := keyNamesHeld(earlier, held)
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
// that has no name, with the name it would have taken — a key's newest earlier name, and
// otherwise its role's (RoleName) — and the key that holds that name: the key of the agent of
// named that holds it, and otherwise the key whose earlier row, or the lasting record, holds it
// (keyNamesHeld). This release names each agent of an issue for a role and gives each name to one
// agent, and a name a key held is that key's alone, so a version with one is refused before any
// write. held is required, as it is of NameAgents: a caller with no history passes none of its
// own, and one that forgets it would read the names from the rows alone.
func UnnamedAgents(earlier, named []PlanStep, held []KeyName) []UnnamedAgent {
	keyNames, holders := keyNamesHeld(earlier, held)
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

// keyNamesHeld is, read newest first, the name each key but the owner's held and, by each such
// name, the key that holds it: one walk of the rows and the record, since the two are the same
// pairs with the keys and the values swapped. The name of a key is the name of its row of the
// latest generation of earlier that names it, and for a key no row of earlier names, the name the
// lasting record holds for it (held); the holder of a name is the key of the latest row of earlier
// of that name that names a key, and for a name no such row holds, the key the record holds it
// for. earlier is ordered by generation, as PlanStepsOf reads it, and the steps of one key in one
// generation share one name, so a key named again since an earlier generation keeps the name it
// holds now and not its oldest.
//
// A plan's rows go when a later version gives the task again or the plan of an earlier owner is
// passed over, and the record stays, so the record is what keeps a name a key held that key's
// after its rows went. A name a key held is that key's alone (NameAgents), so in rows NameAgents
// wrote only that key's rows hold it; rows an earlier build wrote may show another key at it
// later, and then the later key holds it, since the name's claim ran that key's agent last.
func keyNamesHeld(earlier []PlanStep, held []KeyName) (names map[string]claim.Agent, holders map[claim.Agent]string) {
	names, holders = map[string]claim.Agent{}, map[claim.Agent]string{}
	take := func(key string, name claim.Agent) {
		if key == "" || name == "" {
			return
		}
		if _, ok := names[key]; !ok {
			names[key] = name
		}
		if _, ok := holders[name]; !ok {
			holders[name] = key
		}
	}
	for i := len(earlier) - 1; i >= 0; i-- {
		// AgentName is "" for a step of the key owner, as for a row that holds no name.
		take(earlier[i].AgentKey, earlier[i].AgentName())
	}
	for _, h := range held {
		take(h.AgentKey, h.Name)
	}
	return names, holders
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
