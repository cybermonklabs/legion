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

// todaysOrder is the order this release runs the steps of a task in, as its refusal names it.
var todaysOrder = func() string {
	names := make([]string, len(builtinRoles))
	for i, role := range builtinRoles {
		names[i] = role.Name
	}
	return strings.Join(names, ", ")
}()

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
			if _, ok := d.Roles[step.Role]; ok || defined[step.Role] || refused[step.Role] {
				continue
			}
			refused[step.Role] = true
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf(
				"step %d of task %s names the role %s, which neither the deployment nor the plan defines.", i+1, task.ID, step.Role)})
		}
	}
	return refusals
}

// Supported is what this release does not carry out yet: more than one task, a role block,
// and, for each task, `needs`, `owner`, `part_of` and `merges`, each key but the owner's on
// acceptance, acceptance with no key, a role named at two steps, steps in another order than
// today's, and no implementer. A task that names no steps runs the project's default steps,
// which the configuration loader holds to these same checks, so none of its step causes apply.
func Supported(p Plan) []Refusal {
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
		for _, reason := range taskReasons(task) {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: reason + notYet})
		}
	}
	return refusals
}

// taskReasons is Supported's causes in one task: its attributes first, then its steps (stepReasons).
func taskReasons(t Task) []string {
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
		reasons = append(reasons, stepReasons(t)...)
	}
	return reasons
}

// stepReasons is Supported's causes in the steps of t, in this order: each step's key, in step
// order; each role named at two steps, at its first two; the first step that comes after a step
// later in today's order, named with the latest such step before it; and no implementer. A role
// outside Legion's five has no place in today's order, and Validate refuses it.
func stepReasons(t Task) []string {
	var keys, twice []string
	firstAt := map[string]int{}
	var roles []string // each role once, in the order of its first step
	secondAt := map[string]int{}
	outOfOrder := ""
	latest := -1 // the index of the step latest in today's order so far
	hasImplementer := false
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case step.Role == "acceptance" && step.Key == "":
			keys = append(keys, fmt.Sprintf("step %d of task %s names acceptance with no key, which asks for a fresh agent of the role acceptance.", at, t.ID))
		case step.Key != "" && (step.Role != "acceptance" || step.Key != "owner"):
			keys = append(keys, fmt.Sprintf("step %d of task %s names the key %s.", at, t.ID, step.Key))
		}
		if _, seen := firstAt[step.Role]; !seen {
			firstAt[step.Role] = at
			roles = append(roles, step.Role)
		} else if _, seen := secondAt[step.Role]; !seen {
			secondAt[step.Role] = at
		}
		hasImplementer = hasImplementer || step.Role == "implementer"
		rank := orderRank(step.Role)
		if rank < 0 {
			continue
		}
		if latest >= 0 && rank < orderRank(t.Steps[latest].Role) {
			if outOfOrder == "" {
				outOfOrder = fmt.Sprintf("task %s names %s at step %d after %s at step %d, and this release runs the steps in the order %s.",
					t.ID, step.Role, at, t.Steps[latest].Role, latest+1, todaysOrder)
			}
			continue
		}
		latest = i
	}
	for _, role := range roles {
		if second, ok := secondAt[role]; ok {
			twice = append(twice, fmt.Sprintf("task %s names the role %s at steps %d and %d.", t.ID, role, firstAt[role], second))
		}
	}
	reasons := append(keys, twice...)
	if outOfOrder != "" {
		reasons = append(reasons, outOfOrder)
	}
	if !hasImplementer {
		reasons = append(reasons, fmt.Sprintf("task %s has no step implementer.", t.ID))
	}
	return reasons
}

// orderRank is role's place in today's order, or -1 for a role outside Legion's five.
func orderRank(role string) int {
	for i, builtin := range builtinRoles {
		if builtin.Name == role {
			return i
		}
	}
	return -1
}
