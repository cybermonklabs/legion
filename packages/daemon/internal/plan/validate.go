package plan

import (
	"context"
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
// one cycle of each set of tasks that need each other (cycles), named by its first task in id order
// with the chain from that task; then the causes of the tasks with a sub-owner, of their parts, and
// of each need of a part (partRefusals), a task of the top level's among them, which read the needs
// on those cycles from the same pass, so a need on a cycle gets the cycle's reason alone. It refuses
// no plan for a step the plan lacks. Its one error is ctx's, once ctx ends before the checks of
// cycles and of parts are done: an owner or a sub-owner writes the plan, and register_plan checks
// it before the fact lock, where only the request's own end can stop it.
func Validate(ctx context.Context, p Plan, d Deployment) ([]Refusal, error) {
	if len(p.Tasks) == 0 {
		return []Refusal{{Reason: noTask}}, nil
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ended := stepsOf(ctx)
	found, onCycle, err := cycles(p.Tasks, ended)
	if err != nil {
		return nil, err
	}
	for _, cycle := range found {
		chain := make([]string, len(cycle))
		for i, id := range cycle {
			chain[i] = id + " needs " + cycle[(i+1)%len(cycle)]
		}
		refusals = append(refusals, Refusal{Task: cycle[0], Reason: fmt.Sprintf("task %s needs itself: %s.", cycle[0], strings.Join(chain, ", "))})
	}
	parts, err := partRefusals(p.Tasks, onCycle, ended)
	if err != nil {
		return nil, err
	}
	return append(refusals, parts...), nil
}

// partRefusals is Validate's causes of the tasks with a sub-owner, of their parts, and of each need
// of a part, in task id order, a task's own causes together, one for each cause:
//   - a task that names both steps and a sub-owner, whose work is its parts;
//   - for each of its needs, in its block's order, the one reason the rules of parts give it
//     (waits.needReasons, which leaves each need of onCycle, the needs on a cycle of needs that
//     cycles found, to its cycle's reason): a need of a part of another task, which no task needs;
//     or a need that is done only after the task itself starts, so the task never starts and the
//     tasks above it are never done;
//   - a task whose sub-owner's agent takes the name (SubOwnerAgent) of the agent of a task before
//     it in id order: one claim cannot be two sub-owners.
//
// A part may need its siblings and a task of the top level that does not wait for its own task,
// which Legion carries out. It reads ended as the check of cycles does (stepsOf), and answers its
// error once it has one.
func partRefusals(tasks []Task, onCycle map[waitEdge]bool, ended func() error) ([]Refusal, error) {
	w := newWaits(tasks)
	refused, err := w.needReasons(onCycle, func() error { return nil })
	if err != nil {
		return nil, err
	}
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
	return refusals, nil
}

// wordList is words joined as a sentence lists them: "a", "a and b", "a, b and c".
func wordList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// contextSteps is how many steps of the checks of cycles and of parts pass between two reads of
// their context. Each step is one task, one need or one wait, so a check whose request has ended
// stops within that many.
const contextSteps = 1024

// stepsOf is the read of ctx that the checks of cycles and of parts make at each step: it reads
// ctx's error once in contextSteps calls, and answers it once ctx has ended. One count runs over
// both checks, which Validate makes one after the other on one request.
func stepsOf(ctx context.Context) func() error {
	steps := 0
	return func() error {
		if steps++; steps%contextSteps != 0 {
			return nil
		}
		return ctx.Err()
	}
}

// cycles is one cycle of needs for each set of tasks that need each other: each strongly
// connected component of the tasks' needs that holds a cycle, which is one of more than one task
// or one task that needs itself (components, one pass over the tasks and their needs). The cycle
// is the shortest from the set's first task in id order back to that task, found breadth first
// inside the set (cycleFrom), as Dispatch's spec graph names its loops (findLoops in
// packages/envoy/internal/dispatch/specgraph). So the check takes time linear in the tasks and
// their needs, and a set gives one reason however many cycles run through it: a search from
// every need, which named the shortest cycle through each one, was quadratic on a ring of tasks,
// and Dispatch takes an owner's plan of 10,000 of them. Each cycle starts at its set's first task,
// and the cycles are in the order of those tasks' ids. onCycle is each need that lies on a cycle,
// from the same pass: a need between two tasks of one component, which holds a cycle through each
// need inside it, a task's need of itself among them. The rules of parts give such a need no
// reason of their own (partRefusals), since its cycle's reason names it. A need is a task id
// (FromBlocks); one that names no task of tasks closes no cycle. cycles reads ended at each step
// (stepsOf) and answers its error once it has one.
func cycles(tasks []Task, ended func() error) (found [][]string, onCycle map[waitEdge]bool, err error) {
	at := make(map[string]int, len(tasks))
	for i, task := range tasks {
		at[task.ID] = i
	}
	needs := make([][]int, len(tasks))
	for i, task := range tasks {
		for _, id := range task.Needs {
			if needed, ok := at[id]; ok {
				needs[i] = append(needs[i], needed)
			}
		}
	}
	component, count, err := components(needs, ended)
	if err != nil {
		return nil, nil, err
	}
	onCycle = map[waitEdge]bool{}
	for i, of := range needs {
		for _, needed := range of {
			if component[needed] == component[i] {
				onCycle[waitEdge{tasks[i].ID, tasks[needed].ID}] = true
			}
		}
	}
	// Each component's first task in id order, and whether it holds a cycle.
	first := make([]int, count)
	members := make([]int, count)
	itself := make([]bool, count)
	for i, task := range tasks {
		c := component[i]
		if members[c] == 0 || task.ID < tasks[first[c]].ID {
			first[c] = i
		}
		members[c]++
		itself[c] = itself[c] || slices.Contains(needs[i], i)
	}
	parent := make([]int, len(tasks))
	for i := range parent {
		parent[i] = -1
	}
	for c := range count {
		if members[c] == 1 && !itself[c] {
			continue
		}
		path, err := cycleFrom(needs, component, parent, first[c], ended)
		if err != nil {
			return nil, nil, err
		}
		cycle := make([]string, len(path))
		for i, task := range path {
			cycle[i] = tasks[task].ID
		}
		found = append(found, cycle)
	}
	slices.SortFunc(found, slices.Compare)
	return found, onCycle, nil
}

// components numbers the strongly connected components of the graph in which node i leads to each
// node of needs[i], a task to each task it needs (cycles), or a moment of a task to each moment it
// waits for (waits.needReasons), by Tarjan's algorithm kept on explicit stacks, so a chain of
// 10,000 needs costs no call stack: component[i] is node i's component, of count in all. It reads
// ended once for each node and each edge, and answers its error once it has one.
func components(needs [][]int, ended func() error) (component []int, count int, err error) {
	const unvisited = -1
	// order is the order each task was first reached in, and low the earliest order reached from
	// it through the tasks still on the stack.
	order := make([]int, len(needs))
	low := make([]int, len(needs))
	onStack := make([]bool, len(needs))
	component = make([]int, len(needs))
	for i := range order {
		order[i] = unvisited
	}
	var stack []int
	// visiting is the path of tasks being visited, each with the index of its next need.
	type visit struct{ task, next int }
	var visiting []visit
	reached := 0
	enter := func(task int) {
		order[task], low[task] = reached, reached
		reached++
		stack = append(stack, task)
		onStack[task] = true
		visiting = append(visiting, visit{task: task})
	}
	for root := range needs {
		if order[root] != unvisited {
			continue
		}
		enter(root)
		for len(visiting) > 0 {
			if err := ended(); err != nil {
				return nil, 0, err
			}
			top := &visiting[len(visiting)-1]
			if top.next < len(needs[top.task]) {
				needed := needs[top.task][top.next]
				top.next++
				switch {
				case order[needed] == unvisited:
					enter(needed)
				case onStack[needed]:
					low[top.task] = min(low[top.task], order[needed])
				}
				continue
			}
			task := top.task
			visiting = visiting[:len(visiting)-1]
			if len(visiting) > 0 {
				caller := visiting[len(visiting)-1].task
				low[caller] = min(low[caller], low[task])
			}
			if low[task] != order[task] {
				continue
			}
			for {
				member := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[member] = false
				component[member] = count
				if member == task {
					break
				}
			}
			count++
		}
	}
	return component, count, nil
}

// cycleFrom is the shortest cycle from task back to task inside its component: the tasks from
// task to the last before it again, each needing the next, found breadth first with each task's
// needs in the order its block writes them. parent holds -1 for each task of the component;
// cycleFrom marks the tasks it reaches there, and no later call reads those marks, since each
// component is searched once. It reads ended once for each need it follows.
func cycleFrom(needs [][]int, component, parent []int, task int, ended func() error) ([]int, error) {
	parent[task] = task
	queue := []int{task}
	for head := 0; head < len(queue); head++ {
		at := queue[head]
		for _, needed := range needs[at] {
			if err := ended(); err != nil {
				return nil, err
			}
			if needed == task {
				path := []int{at}
				for at != task {
					at = parent[at]
					path = append(path, at)
				}
				slices.Reverse(path)
				return path, nil
			}
			if component[needed] == component[task] && parent[needed] < 0 {
				parent[needed] = at
				queue = append(queue, needed)
			}
		}
	}
	// A component of more than one task leads from each of its tasks back to it, and one of one
	// task holds a cycle only when that task needs itself, which the first need it follows finds.
	panic(fmt.Sprintf("plan: task %d leads back to itself by no need inside its component", task))
}

// shortestPath is the fewest nodes from from to to, both ends included, each step from a node to
// one next answers for it, taken in next's order: from alone when it is to, and nil when no path
// leads there. needReasons walks the waits of a task's start and end with it (waits.waitPath). It
// reads ended once for each node it reaches, and answers its error once it has one.
func shortestPath[N comparable](from, to N, next func(N) []N, ended func() error) ([]N, error) {
	parent := map[N]N{from: from}
	for queue := []N{from}; len(queue) > 0; queue = queue[1:] {
		if err := ended(); err != nil {
			return nil, err
		}
		at := queue[0]
		if at == to {
			path := []N{at}
			for at != from {
				at = parent[at]
				path = append(path, at)
			}
			slices.Reverse(path)
			return path, nil
		}
		for _, node := range next(at) {
			if _, reached := parent[node]; !reached {
				parent[node] = at
				queue = append(queue, node)
			}
		}
	}
	return nil, nil
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
