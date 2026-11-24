package plan

import (
	"context"
	"fmt"
	"slices"
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

// Validate is what Legion cannot carry out in any release: a plan with no task; for each task,
// each `needs` word that names no task of the plan, in the block's order (Task.Unresolved, which
// FromBlocks fills), then each `needs` word that is a `dispatch://` reference not of the plan's
// issue, in the block's order (Task.OutsideIssue), one into another issue naming that issue and one
// that starts with no issue key as naming none, and each step that names a role neither d nor the
// plan's own role blocks define, refused once for each role at the first step that names it; then
// one cycle of each set of tasks that need each other (cycles), named by its first task in id order
// with the chain from that task. It refuses no plan for a step the plan lacks. The other refusals
// of a plan no release carries out need an attribute or a block that Supported refuses in this
// release, so the release that lifts that refusal adds them here. Its one error is ctx's, once ctx
// ends before the check of cycles is done: an owner writes the plan, and register_plan checks it
// before the fact lock, where only the request's own end can stop it.
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
		for _, word := range task.Unresolved {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf("task %s needs %s, which names no task of this plan.", task.ID, word)})
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
	found, err := cycles(ctx, p.Tasks)
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
	return refusals, nil
}

// contextSteps is how many steps of the check of cycles pass between two reads of its context.
// Each step is one task or one need, so a check whose request has ended stops within that many.
const contextSteps = 1024

// cycles is one cycle of needs for each set of tasks that need each other: each strongly
// connected component of the tasks' needs that holds a cycle, which is one of more than one task
// or one task that needs itself (components, one pass over the tasks and their needs). The cycle
// is the shortest from the set's first task in id order back to that task, found breadth first
// inside the set (cycleFrom), as Dispatch's spec graph names its loops (findLoops in
// packages/envoy/internal/dispatch/specgraph). So the check takes time linear in the tasks and
// their needs, and a set gives one reason however many cycles run through it: a search from
// every need, which named the shortest cycle through each one, was quadratic on a ring of tasks,
// and Dispatch takes an owner's plan of 10,000 of them. Each cycle starts at its set's first task,
// and the cycles are in the order of those tasks' ids. A need is a task id (FromBlocks); one that
// names no task of tasks closes no cycle. cycles reads ctx as it goes and answers ctx's error
// once ctx has ended.
func cycles(ctx context.Context, tasks []Task) ([][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	steps := 0
	ended := func() error {
		if steps++; steps%contextSteps != 0 {
			return nil
		}
		return ctx.Err()
	}
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
		return nil, err
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
	var found [][]string
	for c := range count {
		if members[c] == 1 && !itself[c] {
			continue
		}
		path, err := cycleFrom(needs, component, parent, first[c], ended)
		if err != nil {
			return nil, err
		}
		cycle := make([]string, len(path))
		for i, task := range path {
			cycle[i] = tasks[task].ID
		}
		found = append(found, cycle)
	}
	slices.SortFunc(found, slices.Compare)
	return found, nil
}

// components numbers the strongly connected components of the graph in which task i needs each
// task of needs[i], by Tarjan's algorithm kept on explicit stacks, so a chain of 10,000 needs
// costs no call stack: component[i] is task i's component, of count in all. It reads ended once
// for each task and each need, and answers its error once it has one.
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

// Supported is what this release does not carry out yet: more than one task that no task needs
// (Sinks, over the task ids FromBlocks reads), whose work would each merge by itself where this
// release merges an issue's work as one change, a role block, and, for each task, `owner`,
// `part_of` and `merges`, the key owner on a role but acceptance, acceptance with a key but owner
// or with none, a role named at two steps, no implementer, each step of a role that reads the
// implementer's pull request with no implementer step before it, and each step after the
// implementer's whose role d gives the type implementation, since Legion's retro is done by the
// implementer. A plan of several tasks, and a task's `needs`, are taken: each task runs once the
// tasks it needs are done. Any other key is taken: it names the agent of its steps (AgentOf). Its
// steps stand in any other order: the planner at any place, the tester, the reviewer and the
// acceptance in any order after the implementer, and a role of the deployment at any place. A
// task that names no steps runs the project's default steps, which the configuration loader holds
// to these same checks, so none of its step causes apply.
func Supported(p Plan, d Deployment) []Refusal {
	var refusals []Refusal
	if sinks := Sinks(p.Tasks); len(sinks) > 1 {
		refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
			"the plan has %d tasks that no task needs (%s), and the work of each would merge by itself.", len(sinks), strings.Join(sinks, ", ")) + notYet})
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
