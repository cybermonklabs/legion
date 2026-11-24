package plan

import (
	"fmt"
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

// Validate is what Legion cannot carry out in any release: a plan with no task; each task block
// whose `parent` names an id of the plan's document that no typed block of the version holds
// (Task.MissingParent), in the document's order; each element that names `steps`, `owner` or
// `merges` (Element.Names), in the document's order, since Legion does not carry out an element;
// for each task, each `needs` word that names no task of the plan, in the block's order
// (Task.Unresolved, which FromBlocks fills), then each `needs` word that names an element
// (Task.ElementNeeds), then each `needs` word that is a `dispatch://` reference not of the plan's
// issue, in the block's order (Task.OutsideIssue), one into another issue naming that issue and one
// that starts with no issue key as naming none, and each step that names a role neither d nor the
// plan's own role blocks define, refused once for each role at the first step that names it; then
// each cycle of needs (cycles), named by its first task in id order with the chain from that task;
// then the causes of the tasks with a sub-owner, of their parts, and of each need of a part
// (partRefusals), a task of the top level's among them, which read the same cycles, so a need on a
// cycle gets the cycle's reason alone. It refuses no plan for a step the plan lacks.
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
		if task.MissingParent != "" {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf("task block %s names %s as its parent, which no typed block of version %d of the plan holds. "+
				"Name a typed block of this version in parent, or leave parent out.", task.ID, task.MissingParent, p.Version)})
		}
	}
	elements := make(map[string]Element, len(p.Elements))
	for _, element := range p.Elements {
		elements[element.ID] = element
		if len(element.Names) == 0 {
			continue
		}
		under := fmt.Sprintf("%s, which names no sub-owner", element.Under)
		if element.Under != element.Of {
			under = fmt.Sprintf("%s, an element of %s", element.Under, element.Of)
		}
		refusals = append(refusals, Refusal{Task: element.ID, Reason: fmt.Sprintf("task block %s is an element of task %s: it stands under %s. "+
			"Legion does not carry out an element, so an element names no steps, owner or merges, and %s names %s.",
			element.ID, element.Of, under, element.ID, wordList(element.Names))})
	}
	for _, task := range p.Tasks {
		for _, word := range task.Unresolved {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf("task %s needs %s, which names no task of this plan.", task.ID, word)})
		}
		for _, word := range task.ElementNeeds {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf("task %s needs %s, which is an element of task %s. "+
				"Legion does not carry out an element, so a task cannot need one.", task.ID, word, elements[neededID(word)].Of)})
		}
		for _, word := range task.OutsideIssue {
			var reason string
			if other, _ := issueOf(word); other != "" {
				reason = fmt.Sprintf("task %s needs %s, which names another issue, %s: "+
					"a task can need only a task of this plan, and an issue waits for another issue by a blocked_by link between the two.", task.ID, word, other)
			} else {
				reason = fmt.Sprintf("task %s needs %s, which names no issue: "+
					"a task can need only a task of this plan, and a reference to one starts with the plan's issue key.", task.ID, word)
			}
			refusals = append(refusals, Refusal{Task: task.ID, Reason: reason})
		}
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
	found := cycles(p.Tasks)
	for _, cycle := range found {
		chain := make([]string, len(cycle))
		for i, id := range cycle {
			chain[i] = id + " needs " + cycle[(i+1)%len(cycle)]
		}
		refusals = append(refusals, Refusal{Task: cycle[0], Reason: fmt.Sprintf("task %s needs itself: %s.", cycle[0], strings.Join(chain, ", "))})
	}
	return append(refusals, partRefusals(p.Tasks, found)...)
}

// partRefusals is Validate's causes of the tasks with a sub-owner, of their parts, and of each need
// of a part, in task id order, a task's own causes together, one for each cause:
//   - a task that names both steps and a sub-owner, whose work is its parts;
//   - for each of its needs, in its block's order, the one reason the rules of parts give it
//     (waits.needReasons over found, the plan's cycles of needs, whose needs it leaves to their
//     cycle): a need of a part of another task, which no task needs; or a need that is done only
//     after the task itself starts, so the task never starts and the tasks above it are never done;
//   - a task whose sub-owner's agent takes the name (SubOwnerAgent) of the agent of a task before
//     it in id order: one claim cannot be two sub-owners.
//
// A part may need its siblings and a task of the top level that does not wait for its own task,
// which Legion carries out.
func partRefusals(tasks []Task, found [][]string) []Refusal {
	w := newWaits(tasks)
	refused := w.needReasons(found)
	named := map[claim.Agent]string{}
	var refusals []Refusal
	for _, id := range w.ids {
		task := w.task[id]
		if task.Owner != "" && task.NamesSteps() {
			refusals = append(refusals, Refusal{Task: id, Reason: fmt.Sprintf(
				"task %s names both steps and the sub-owner %s, and a task with a sub-owner has no steps: its work is its parts.", id, task.Owner)})
		}
		for _, need := range task.Needs {
			if reason, ok := refused[waitEdge{id, need}]; ok {
				refusals = append(refusals, Refusal{Task: id, Reason: reason})
			}
		}
		if task.Owner == "" {
			continue
		}
		agent := SubOwnerAgent(id)
		if first, taken := named[agent]; taken {
			refusals = append(refusals, Refusal{Task: id, Reason: fmt.Sprintf(
				"task %s names a sub-owner, and its agent takes the name %s, which the agent of the sub-owner of task %s takes too: give one of the two tasks another id.",
				id, agent, first)})
			continue
		}
		named[agent] = id
	}
	return refusals
}

// wordList is words joined as a sentence lists them: "a", "a and b", "a, b and c".
func wordList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// cycles is the cycles of the tasks' needs that a reason names: for each need that lies on a
// cycle, the shortest cycle through it, once however many of its needs it is the shortest
// through. So each need that closes a cycle is in one of them, and the answer holds at most one
// cycle for each need. Each cycle is its tasks from its first in id order, and the cycles are in
// the order of their tasks' ids, compared one by one. A need is a task id (FromBlocks); one that
// names no task of tasks closes no cycle.
func cycles(tasks []Task) [][]string {
	needs := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		needs[task.ID] = task.Needs
	}
	var found [][]string
	for _, task := range tasks {
		for _, needed := range task.Needs {
			back := shortestPath(needed, task.ID, func(id string) []string { return needs[id] })
			if back == nil {
				continue
			}
			cycle := append([]string{task.ID}, back[:len(back)-1]...)
			first := 0
			for i, id := range cycle {
				if id < cycle[first] {
					first = i
				}
			}
			cycle = append(slices.Clone(cycle[first:]), cycle[:first]...)
			if !slices.ContainsFunc(found, func(other []string) bool { return slices.Equal(other, cycle) }) {
				found = append(found, cycle)
			}
		}
	}
	slices.SortFunc(found, slices.Compare)
	return found
}

// shortestPath is the fewest nodes from from to to, both ends included, each step from a node to
// one next answers for it, taken in next's order: from alone when it is to, and nil when no path
// leads there. cycles walks the tasks' needs with it, and needReasons the waits of a task's start
// and end (waits.waitPath).
func shortestPath[N comparable](from, to N, next func(N) []N) []N {
	parent := map[N]N{from: from}
	for queue := []N{from}; len(queue) > 0; queue = queue[1:] {
		at := queue[0]
		if at == to {
			path := []N{at}
			for at != from {
				at = parent[at]
				path = append(path, at)
			}
			slices.Reverse(path)
			return path
		}
		for _, node := range next(at) {
			if _, reached := parent[node]; !reached {
				parent[node] = at
				queue = append(queue, node)
			}
		}
	}
	return nil
}

// Supported is what this release does not carry out yet: a role block, and, for each task,
// `owner`, a task that is a part of another (Places), the key owner on a role but acceptance,
// acceptance with a key but owner or with none, a role named at two steps, no implementer, each
// step of a role that reads the implementer's pull request with no implementer step before it,
// and each step after the
// implementer's whose role d gives the type implementation, since Legion's retro is done by the
// implementer. A plan of several tasks, and a task's `needs`, are taken: each task runs once the
// tasks it needs are done. So is a plan in which the work of several tasks merges, each by itself
// (Merging). Any other key is taken: it names the agent of its steps (AgentOf). Its steps stand in
// any other order: the planner at any place, the tester, the reviewer and the acceptance in any
// order after the implementer, and a role of the deployment at any place. A task that names no
// steps runs the project's default steps, which the configuration loader holds to these same
// checks, so none of its step causes apply.
func Supported(p Plan, d Deployment) []Refusal {
	var refusals []Refusal
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
	if t.Owner != "" {
		reasons = append(reasons, fmt.Sprintf("task %s names owner.", t.ID))
	}
	if t.PartOf != "" {
		reasons = append(reasons, fmt.Sprintf("task %s is a part of task %s.", t.ID, t.PartOf))
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
// order; each role named at two steps, at its first two; no implementer; each step of a role that
// reads a pull request with no implementer step before it, in step order, named by its word as the
// plan writes it; and each step after the implementer's whose role d gives the type
// implementation, in step order. The owner does the accepting step in this release, so a key on
// it but owner would name an agent Legion does not run it in.
func stepReasons(t Task, d Deployment) []string {
	var keys, twice []string
	firstAt := map[string]int{}
	var roles []string // each role once, in the order of its first step
	secondAt := map[string]int{}
	implementerAt := 0 // the place of the first step of the implementer, 0 before it
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
		if _, seen := firstAt[step.Role]; !seen {
			firstAt[step.Role] = at
			roles = append(roles, step.Role)
		} else if _, seen := secondAt[step.Role]; !seen {
			secondAt[step.Role] = at
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
	for _, role := range roles {
		if second, ok := secondAt[role]; ok {
			twice = append(twice, fmt.Sprintf("task %s names the role %s at steps %d and %d.", t.ID, role, firstAt[role], second))
		}
	}
	reasons := append(keys, twice...)
	if implementerAt == 0 {
		reasons = append(reasons, fmt.Sprintf("task %s has no step implementer.", t.ID))
	}
	reasons = append(reasons, unopened...)
	return append(reasons, retro...)
}
