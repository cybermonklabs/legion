package plan

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The two checks a plan passes before Legion takes it. Both run on every plan, and each gives one
// reason for each cause it finds, so a plan with two causes gets two reasons, and the owner reads
// every cause at once rather than one for each try. Each reason names its task where it has one.

// noTask is the reason a plan with no task block gets. No later release takes such a plan, so it
// is Validate's and does not say "yet".
const noTask = "the plan has no task block, so it gives Legion no work."

// notYet ends each reason of Supported: what a later release carries out.
const notYet = " This release of Legion does not carry it out yet."

// Validate is what Legion cannot carry out in any release: a plan with no task, each role the
// plan defines that would share a name with an agent of another role (numberedRoleRefusals), and
// each step that names a role neither d nor the plan's own role blocks define, refused once for
// each role at the first step that names it. It refuses no plan for a step the plan lacks. A role
// block's name is judged here although Supported refuses every role block in this release, so
// that no release takes such a name. The other causes no release carries out need an attribute or
// a block that Supported refuses in this release, so the release that lifts that refusal adds
// them here.
func Validate(p Plan, d Deployment) []Refusal {
	if len(p.Tasks) == 0 {
		return []Refusal{{Reason: noTask}}
	}
	defined := make(map[string]bool, len(p.RoleBlocks))
	for _, role := range p.RoleBlocks {
		defined[role] = true
	}
	refusals := numberedRoleRefusals(p, d)
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

// numberedRoleRefusals is a refusal for each role the plan defines, in the order of its role
// blocks, whose name is the name Legion gives a second or later agent of another role
// (claim.NumberedAgentRole), of Legion's, of the deployment's or of the plan's own, or a second or
// later agent of which Legion would name as a role of the deployment: an agent of each role would
// hold one claim token, and so one slot and one session. The settings loader refuses such a name
// among the deployment's own roles (config's readRoles), so the deployment's roles alone give no
// refusal here.
func numberedRoleRefusals(p Plan, d Deployment) []Refusal {
	const held = ", so an agent of each role would hold one claim."
	deployment := slices.Sorted(maps.Keys(d.Roles))
	names := make([]claim.Role, 0, len(deployment)+len(p.RoleBlocks))
	for _, name := range slices.Concat(deployment, p.RoleBlocks) {
		names = append(names, claim.Role(name))
	}
	var refusals []Refusal
	for _, role := range p.RoleBlocks {
		if of, ok := claim.NumberedAgentRole(role, names); ok {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
				"the plan defines the role %s, the name Legion gives a second or later agent of the role %s%s", role, of, held)})
			continue
		}
		for _, name := range deployment {
			if _, ok := claim.NumberedAgentRole(name, []claim.Role{claim.Role(role)}); ok {
				refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
					"the plan defines the role %s, whose second or later agent Legion would name %s, the deployment's role%s", role, name, held)})
				break
			}
		}
	}
	return refusals
}

// Supported is what this release does not carry out yet: more than one task, a role block, and,
// for each task, `needs`, `owner`, `part_of` and `merges`, the key owner on a role but acceptance,
// acceptance with a key but owner or with none, no implementer, each step of a role that reads the
// implementer's pull request with no implementer step before it, and each step after the
// implementer's whose role d gives the type implementation, since Legion's retro is done by the
// implementer. Any other key is taken: it names the agent of its steps (AgentOf). A role can stand
// at two steps, of one agent or of two, and a second agent of one role gets a name of its own. Its
// steps stand in any other order: the planner at any place, the tester, the reviewer and the
// acceptance in any order after the implementer, and a role of the deployment at any place. A task
// that names no steps runs the project's default steps, which the configuration loader holds to
// these same checks, so none of its step causes apply.
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
// order; no implementer; each step of a role that reads a pull request with no implementer step
// before it, in step order, named by its word as the plan writes it; and each step after the
// implementer's whose role d gives the type implementation, in step order. The owner does the
// accepting step in this release, so a key on it but owner would name an agent Legion does not run
// it in.
func stepReasons(t Task, d Deployment) []string {
	var reasons []string
	implementerAt := 0 // the place of the first step of the implementer, 0 before it
	var unopened, retro []string
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case step.Role == "acceptance" && step.Key == "":
			reasons = append(reasons, fmt.Sprintf("step %d of task %s names acceptance with no key, which asks for a fresh agent of the role acceptance.", at, t.ID))
		// The key owner and the role acceptance go together: either one without the other is refused.
		case step.Key != "" && (step.Key == "owner") != (step.Role == "acceptance"):
			reasons = append(reasons, fmt.Sprintf("step %d of task %s names the key %s.", at, t.ID, step.Key))
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
	if implementerAt == 0 {
		reasons = append(reasons, fmt.Sprintf("task %s has no step implementer.", t.ID))
	}
	reasons = append(reasons, unopened...)
	return append(reasons, retro...)
}
