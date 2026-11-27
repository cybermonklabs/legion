package plan

import (
	"fmt"
	"strings"
)

// The two checks a plan passes before Legion takes it. Both run on every plan, and each gives one
// reason for each cause it finds, so a plan with two causes gets two reasons, and the owner reads
// every cause at once rather than one for each try. Each reason names its task where it has one.

// noTask is the reason a plan with no task block gets. No later release takes such a plan, so it
// is Validate's and does not say "yet".
const noTask = "the plan has no task block, so it gives Legion no work."

// notYet ends each reason of Supported: what a later release carries out.
const notYet = " This release of Legion does not carry it out yet."

// Validate is what Legion cannot carry out in any release: a plan with no task, and each step
// that names a role neither d nor the plan's own role blocks define, refused once for each role
// at the first step that names it. It refuses no plan for a step the plan lacks. The other causes
// no release carries out need an attribute or a block that Supported refuses in this release, so
// the release that lifts that refusal adds them here.
func Validate(p Plan, d Deployment) []Refusal {
	if len(p.Tasks) == 0 {
		return []Refusal{{Reason: noTask}}
	}
	defined := make(map[string]bool, len(p.RoleBlocks))
	for _, role := range p.RoleBlocks {
		defined[role] = true
	}
	var refusals []Refusal
	for _, task := range p.Tasks {
		refused := map[string]bool{}
		for i, step := range task.Steps {
			if d.Defines(step.Role) || defined[step.Role] || refused[step.Role] {
				continue
			}
			refused[step.Role] = true
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf(
				"step %d of task %s names the role %s, which neither the deployment nor the plan defines.", i+1, task.ID, step.Role)})
		}
	}
	return refusals
}

// Supported is what this release does not carry out yet: more than one task, a role block, and,
// for each task, `needs`, `owner`, `part_of` and `merges`, the key owner on a role but acceptance,
// acceptance with a key but owner or with none, a second agent that starts with the role of
// another, no implementer, each step of a role that reads the implementer's pull request with no
// implementer step before it, and each step after the implementer's whose role d gives the type
// implementation, since Legion's retro is done by the implementer. Any other key is taken: it names
// the agent of its steps (AgentOf), and a role can stand at two steps. Its steps stand in any other
// order: the planner at any place, the tester, the reviewer and the acceptance in any order after
// the implementer, and a role of the deployment at any place. A task that names no steps runs the
// project's default steps, which the configuration loader holds to these same checks, so none of
// its step causes apply.
func Supported(p Plan, d Deployment) []Refusal {
	var refusals []Refusal
	if len(p.Tasks) > 1 {
		ids := make([]string, len(p.Tasks))
		for i, task := range p.Tasks {
			ids[i] = task.ID
		}
		refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
			"the plan has %d tasks (%s), and this release runs one task for each issue.", len(p.Tasks), strings.Join(ids, ", ")) + notYet})
	}
	for _, role := range p.RoleBlocks {
		refusals = append(refusals, Refusal{Reason: "the plan defines the role " + role + "." + notYet})
	}
	for _, task := range p.Tasks {
		for _, reason := range taskReasons(task, d) {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: reason + notYet})
		}
	}
	return refusals
}

// taskReasons is Supported's causes in one task: its attributes first, then its steps (stepReasons).
func taskReasons(t Task, d Deployment) []string {
	var reasons []string
	if len(t.Needs) > 0 {
		reasons = append(reasons, fmt.Sprintf("task %s needs %s.", t.ID, strings.Join(t.Needs, ", ")))
	}
	if t.Owner != "" {
		reasons = append(reasons, fmt.Sprintf("task %s names owner.", t.ID))
	}
	if t.PartOf != "" {
		reasons = append(reasons, fmt.Sprintf("task %s names part_of.", t.ID))
	}
	if t.Merges {
		reasons = append(reasons, fmt.Sprintf("task %s names merges.", t.ID))
	}
	if t.NamesSteps() {
		reasons = append(reasons, stepReasons(t, d)...)
	}
	return reasons
}

// readsPullRequest is each role whose step reads the pull request the implementer's step opens:
// the tester and the reviewer judge its head, and the owner's acceptance reads the evidence of it.
// The implementer's move on waits for that pull request (the workflow's guard hasOpenOrMergedPR),
// so such a step with no implementer step before it has no pull request to read, and this release
// refuses it; a later release that gives such a step another pull request to read takes it. A
// role of the deployment reports pass or fail and reads no pull request, so it stands anywhere.
// The role decides, never the step's type.
var readsPullRequest = map[string]bool{"tester": true, "reviewer": true, "acceptance": true}

// stepReasons is Supported's causes in the steps of t, in this order: each step that names the
// key owner on a role but acceptance, or acceptance with a key but owner or with none, in step
// order; the first step of each agent that starts with the role another agent of t started with
// before it, in step order; no implementer; each step of a role that reads a pull
// request with no implementer step before it, in step order, named by its word as the plan writes
// it; and each step after the implementer's whose role d gives the type implementation, in step
// order. The owner does the accepting step in this release, so a key on it but owner would name an
// agent Legion does not run it in. An agent other than the owner is named by the role it starts
// with, and this release runs one agent that starts with each role for an issue, so a second agent
// of one first role has no name, while an agent that starts with another role can work a step of
// that role; the owner's claim is the tree's architect, and it takes no name.
func stepReasons(t Task, d Deployment) []string {
	var keys, unnamed []string
	agentSeen := map[Agent]bool{}
	startsAt := map[string]int{} // the first step of the first agent that starts with each role
	implementerAt := 0           // the place of the first step of the implementer, 0 before it
	var unopened, retro []string
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case step.Role == "acceptance" && step.Key == "":
			keys = append(keys, fmt.Sprintf("step %d of task %s names acceptance with no key, which asks for a fresh agent of the role acceptance.", at, t.ID))
		// The key owner and the role acceptance go together: either one without the other is refused.
		case step.Key != "" && (step.Key == "owner") != (step.Role == "acceptance"):
			keys = append(keys, fmt.Sprintf("step %d of task %s names the key %s.", at, t.ID, step.Key))
		}
		if agent := AgentOf(t.ID, at, step); !agentSeen[agent] && !agent.Owner() {
			agentSeen[agent] = true
			role := step.Role // the agent's first step, so the role it starts with
			if first, taken := startsAt[role]; taken {
				unnamed = append(unnamed, fmt.Sprintf("step %d of task %s needs an agent of its own beside the agent of step %d, and both agents start with the role %s, and this release runs one agent that starts with each role for an issue.",
					at, t.ID, first, role))
			} else {
				startsAt[role] = at
			}
		}
		if readsPullRequest[step.Role] && implementerAt == 0 {
			unopened = append(unopened, fmt.Sprintf("step %d of task %s names %s, and no step before it names the role implementer, whose pull request %s reads; a role of this deployment that reports pass or fail needs none.",
				at, t.ID, step, step))
		}
		if implementerAt > 0 && step.Role != "implementer" && d.Roles[step.Role].Type == TypeImplementation {
			retro = append(retro, fmt.Sprintf("task %s names %s at step %d, a step of the type implementation after the step implementer at step %d, and Legion's retro is done by the implementer in this release.",
				t.ID, step.Role, at, implementerAt))
		}
		if implementerAt == 0 && step.Role == "implementer" {
			implementerAt = at
		}
	}
	reasons := append(keys, unnamed...)
	if implementerAt == 0 {
		reasons = append(reasons, fmt.Sprintf("task %s has no step implementer.", t.ID))
	}
	reasons = append(reasons, unopened...)
	return append(reasons, retro...)
}
