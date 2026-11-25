package record

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// maxAgentName is the longest name of an agent, in bytes: the bound of the agent grammar
// (claim.AgentToken).
const maxAgentName = 32

// NameAgents is steps, copied, with the name of each step's agent (PlanStep.Agent) given: the one
// rule of a name, which reads no store of its own. earlier is every plan_steps row of the issue, of
// every generation (PlanStepsOf), read before the version's steps are written, since the write
// deletes the steps of each task it gives. It names in four passes, in this order, so that a key
// the version names never changes its name and no new name can take that key's. A key the version
// no longer names frees its name: an agent of the version can take it, and with it that name's
// claim and session, as a new task's agent with no key takes the name of a stopped task's agent
// of its role.
//
//  1. A step of the key owner has no name: its agent is the tree's architect, whose token names the
//     tree and no name on the issue.
//  2. Each other key that an earlier row names takes the name of that row. Every such key is named
//     before any other pass gives a name.
//  3. Each step with no key takes the name of the latest earlier row of its task, place and role
//     with no key, unless pass 2 or an earlier step of this pass gave that name.
//  4. Each agent still with no name, in the order of its first step, takes the role of that first
//     step, cut to maxAgentName bytes, where no agent of steps has that name. An agent whose role's
//     name another agent holds keeps no name: this release runs one agent of each role for an
//     issue, and the caller refuses such a version before it writes anything.
//
// The steps of one key are one agent (plan.AgentOf), so they share one name in every task.
func NameAgents(earlier, steps []PlanStep) []PlanStep {
	named := slices.Clone(steps)
	agentOf := func(s PlanStep) plan.Agent {
		return plan.AgentOf(s.Task, s.Position, plan.Step{Role: s.Role, Key: s.AgentKey})
	}
	keyNames := map[string]claim.Agent{}
	for _, row := range earlier {
		if agent := agentOf(row); agent.Key != "" && !agent.Owner() {
			if _, ok := keyNames[agent.Key]; !ok && row.AgentName() != "" {
				keyNames[agent.Key] = row.AgentName()
			}
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
		if agent := agentOf(step); agent.Owner() {
			give(agent, "")
		} else if name, ok := keyNames[agent.Key]; ok && agent.Key != "" {
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := agentOf(step)
		if agent.Key != "" {
			continue
		}
		if name, ok := earlierPlaceName(earlier, step); ok && !taken[name] {
			give(agent, name)
		}
	}
	for _, step := range named {
		agent := agentOf(step)
		if _, ok := names[agent]; ok {
			continue
		}
		name := RoleName(step.Role)
		if taken[name] {
			give(agent, "")
			continue
		}
		give(agent, name)
	}
	for i := range named {
		named[i].Agent = names[agentOf(named[i])]
	}
	return named
}

// RoleName is the name an agent takes from the role of its first step (NameAgents' pass 4): the
// role, cut to maxAgentName bytes.
func RoleName(role string) claim.Agent {
	if len(role) > maxAgentName {
		role = role[:maxAgentName]
	}
	return claim.Agent(role)
}

// CheckAgent is the agent that works the production check of task's merge, for the checking role
// role: the agent of task's last step of that role (PlanStep.AgentName, of the step at the highest
// place), and where task has none, a fresh agent of the task, `check-<slug>` (plan.TaskSlug), or
// `check-t-` and the first 12 lower-case hexadecimal characters of the SHA-256 of the task's id
// where that name is longer than maxAgentName bytes. steps are the plan's steps; only task's are
// read, so no other task's agent and no list of names enter the answer, and the name of a fresh
// agent depends on the task alone. A step of the key owner names no agent of the issue (its agent
// is the tree's architect), so it counts as none. A step's agent takes a fresh agent's name only
// where a role is itself named `check-<slug>`, and none is.
func CheckAgent(steps []PlanStep, task string, role claim.Role) claim.Agent {
	var agent claim.Agent
	last := 0
	for _, step := range steps {
		if name := step.AgentName(); step.Task == task && claim.Role(step.Role) == role && name != "" && step.Position > last {
			agent, last = name, step.Position
		}
	}
	if agent != "" {
		return agent
	}
	if fresh := "check-" + plan.TaskSlug(task); len(fresh) <= maxAgentName {
		return claim.Agent(fresh)
	}
	sum := sha256.Sum256([]byte(task))
	return claim.Agent("check-t-" + hex.EncodeToString(sum[:6]))
}

// UnnamedAgents is the first step of each agent of named, steps NameAgents gave, but the owner,
// that has no name: an agent whose role's name (RoleName) another agent of the plan holds. This
// release runs one agent of each role for an issue, so a version with one is refused before any
// write.
func UnnamedAgents(named []PlanStep) []PlanStep {
	var unnamed []PlanStep
	seen := map[plan.Agent]bool{}
	for _, step := range named {
		agent := plan.AgentOf(step.Task, step.Position, plan.Step{Role: step.Role, Key: step.AgentKey})
		if seen[agent] || agent.Owner() {
			continue
		}
		seen[agent] = true
		if step.Agent == "" {
			unnamed = append(unnamed, step)
		}
	}
	return unnamed
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
