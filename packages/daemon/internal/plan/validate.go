package plan

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// The two checks a plan passes before Legion takes it. Both run on every plan (Refusals), and each
// gives a reason for each cause it finds, up to the bound of its kind, so a plan with two causes
// gets two reasons, and the owner reads every cause at once rather than one for each try. Each
// reason names its task where it has one. One refusal names at most MostReasons causes of each
// kind and counts the rest (reasons), and each reason quotes the plan within a bound of bytes: a
// task id of at most steprecord.TaskIDMax (FromBlocks refuses one outside Dispatch's task form), a
// word of at most MostQuotedBytes (quoted), a list of at most MostReasons ids (listed) and a chain
// of at most MostReasons needs, of two ids each (chainOf).

// Refusals is every reason Legion does not take p: Supported's, what this release does not carry
// out yet, then Validate's, what no release can. Each check bounds its own kinds (reasons), and the
// two share none, so joining their reasons bounds each kind of the refusal. Every caller that takes
// or refuses a plan reads it here: register_plan, the configuration loader's check of a project's
// default steps, and MovesBackPairs.
func Refusals(p Plan, d Deployment) []Refusal {
	return append(Supported(p), Validate(p, d)...)
}

// MostReasons is the most causes of one kind that one refusal of a plan names; one more reason
// counts the rest of that kind (reasons). It bounds a list and a chain as well: a reason lists at
// most MostReasons task ids and counts the rest (listed), and a cycle's chain names at most
// MostReasons needs, of two ids each (chainOf). The owner fixes what the refusal names and gives
// the plan again, and the next refusal names what is left. Twenty is about one screen of sentences
// of 50 to 170 bytes, a few kilobytes in an answer that writes each reason twice, which a terminal
// and an agent's tool result both hold, and enough causes to show a slip the plan makes at each of
// its items, such as a reference to the blocks of another document in each need or a key on each
// step.
const MostReasons = 20

// MostQuotedBytes is the most bytes of one word of the plan that a reason quotes: a need, a role, a
// key, a step, or a block id outside the task form. Twice the longest task id holds a reference
// to a task of another document (`dispatch://KEY/artifact/<slug>#b-<id>@v2`).
const MostQuotedBytes = 128

// quoted is word as a reason quotes it, with the credentials of every URL it writes removed
// (redact.URLCredentials): what the rule leaves, whole within MostQuotedBytes, else its first
// bytes up to that bound, cut at the start of a character, and a mark that it was cut, with the
// length of what the rule left (redact.Bytes). The rule reads the word whole before the cut,
// because a cut made first can end inside a URL's userinfo, before its "@", where the rule then
// finds no userinfo and the start of a user name or a password stays in the reason, which the
// route writes into `reasons` as it came and into `error` through the same rule (api's
// writeRefusal).
func quoted(word string) string {
	kept, cut := redact.Bytes(word, MostQuotedBytes)
	if !cut {
		return kept
	}
	return fmt.Sprintf("%s… (cut from %d bytes)", kept, len(redact.URLCredentials(word)))
}

// listed is ids as a reason lists them: the first MostReasons, joined by ", ", and past them a
// count of the rest.
func listed(ids []string) string {
	if len(ids) <= MostReasons {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:MostReasons], ", "), len(ids)-MostReasons)
}

// noTask is the reason a plan with no task block gets. No later release takes such a plan, so it
// is Validate's and does not say "yet".
const noTask = "the plan has no task block, so it gives Legion no work."

// notYet ends each reason of Supported: what a later release carries out.
const notYet = " This release of Legion does not carry it out yet."

// cause is the kind of a cause of a refusal, one for each reason Supported and Validate write.
// reasons bounds each kind apart, so no kind of cause pushes another out of the answer.
type cause int

// The kinds, in the order the checks find them. Some give one reason in a plan at most (the plan's
// tasks, its tasks that no task needs, no task block); the others give one for each item.
const (
	manyTasks cause = iota
	manySinks
	roleBlock
	taskNeeds
	taskOwner
	taskPartOf
	taskMerges
	acceptanceNoKey
	stepKey
	roleTwice
	noImplementer
	readsBeforeImplementer
	noTaskBlock
	unresolvedNeed
	undefinedRole
	needsItself
	causes // how many kinds there are
)

// causeWords is each kind as the reason that counts the rest of it names it.
var causeWords = [causes]string{
	manyTasks:              "plans of more than one task",
	manySinks:              "plans of more than one task that no task needs",
	roleBlock:              "roles the plan defines",
	taskNeeds:              "tasks that need other tasks",
	taskOwner:              "tasks that name owner",
	taskPartOf:             "tasks that name part_of",
	taskMerges:             "tasks that name merges",
	acceptanceNoKey:        "steps that name acceptance with no key",
	stepKey:                "steps that name a key",
	roleTwice:              "roles a task names at two steps",
	noImplementer:          "tasks with no step implementer",
	readsBeforeImplementer: "steps that read a pull request with no implementer step before them",
	noTaskBlock:            "plans with no task block",
	unresolvedNeed:         "needs that name no task of this plan",
	undefinedRole:          "roles that neither the deployment nor the plan defines",
	needsItself:            "sets of tasks that need each other",
}

// reasons is the reasons of one refusal of a plan, as Supported and Validate find them: of each
// kind of cause, the first MostReasons in the order found, and past them one more reason that
// counts the rest of that kind, right after the kind's last reason named. The reasons named keep
// the order the checks find them in. Every cause of either check is added here with its kind, so
// each kind a plan has is named at least once, and a plan whose blocks give a cause for each of
// their items (each `needs` word that names no task, each role nobody defines, each step's key,
// each step that reads a pull request with no implementer before it) gets a refusal of at most
// MostReasons + 1 reasons of each kind, however many items it holds: one reason for each item
// answered a 1 MiB attribute in 34 MB (its `needs` words), 44 MB (roles) or 66 MB (keyed steps).
type reasons struct {
	named []namedReason
	found [causes]int // the causes of each kind found, named or not
	last  [causes]int // the place in named of each kind's last reason
}

// namedReason is a reason the refusal names, with the kind of its cause.
type namedReason struct {
	Refusal
	kind cause
}

// add is one cause of kind of task, "" for a cause of the whole plan: named while fewer than
// MostReasons of its kind are, counted past them, so reason is called only for a reason named.
func (r *reasons) add(kind cause, task string, reason func() string) {
	r.found[kind]++
	if r.found[kind] > MostReasons {
		return
	}
	r.last[kind] = len(r.named)
	r.named = append(r.named, namedReason{Refusal: Refusal{Task: task, Reason: reason()}, kind: kind})
}

// yet is add of a cause a later release carries out (Supported's): its reason ends notYet.
func (r *reasons) yet(kind cause, task string, reason func() string) {
	r.add(kind, task, func() string { return reason() + notYet })
}

// refusals is the reasons named, in their order, each kind with more than MostReasons followed,
// right after its last reason named, by one reason of the whole plan that counts the rest.
func (r *reasons) refusals() []Refusal {
	if len(r.named) == 0 {
		return nil
	}
	refusals := make([]Refusal, 0, len(r.named)+int(causes))
	for at, named := range r.named {
		refusals = append(refusals, named.Refusal)
		if left := r.found[named.kind] - MostReasons; left > 0 && r.last[named.kind] == at {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
				"Legion leaves out %d more of the %s: this answer names the first %d.", left, causeWords[named.kind], MostReasons)})
		}
	}
	return refusals
}

// Validate is what Legion cannot carry out in any release: a plan with no task; for each task,
// each `needs` word that names no task of the plan, in the block's order (Task.Unresolved, which
// FromBlocks fills), and each step that names a role neither d nor the plan's own role blocks
// define, refused once for each role at the first step that names it; then one cycle of each set
// of tasks that need each other (cycles), named by its first task in id order with the chain from
// that task. It refuses no plan for a step the plan lacks. The other refusals of a plan no release
// carries out need an attribute or a block that Supported refuses in this release, so the release
// that lifts that refusal adds them here. Its reasons are bounded as reasons bounds each kind.
func Validate(p Plan, d Deployment) []Refusal {
	var found reasons
	if len(p.Tasks) == 0 {
		found.add(noTaskBlock, "", func() string { return noTask })
		return found.refusals()
	}
	defined := make(map[string]bool, len(p.RoleBlocks))
	for _, role := range p.RoleBlocks {
		defined[role] = true
	}
	for _, task := range p.Tasks {
		for _, word := range task.Unresolved {
			found.add(unresolvedNeed, task.ID, func() string {
				return fmt.Sprintf("task %s needs %s, which names no task of this plan.", task.ID, quoted(word))
			})
		}
		refused := map[string]bool{}
		for i, step := range task.Steps {
			if _, ok := d.Roles[step.Role]; ok || defined[step.Role] || refused[step.Role] {
				continue
			}
			refused[step.Role] = true
			found.add(undefinedRole, task.ID, func() string {
				return fmt.Sprintf("step %d of task %s names the role %s, which neither the deployment nor the plan defines.", i+1, task.ID, quoted(step.Role))
			})
		}
	}
	sets := cycles(p.Tasks)
	for _, cycle := range sets {
		found.add(needsItself, cycle[0], func() string {
			return fmt.Sprintf("task %s needs itself: %s.", cycle[0], chainOf(cycle))
		})
	}
	return found.refusals()
}

// chainOf is a cycle's chain as its reason names it, each task needing the next and the last the
// first: every need of a cycle of at most MostReasons tasks, and of a longer one its first
// MostReasons - 1 needs, a count of the needs between, and the need that closes it. So a chain
// names at most MostReasons needs, of two task ids each.
func chainOf(cycle []string) string {
	link := func(i int) string { return cycle[i] + " needs " + cycle[(i+1)%len(cycle)] }
	chain := make([]string, 0, MostReasons+1)
	for i := range min(len(cycle), MostReasons) - 1 {
		chain = append(chain, link(i))
	}
	if left := len(cycle) - MostReasons; left > 0 {
		word := " more needs"
		if left == 1 {
			word = " more need"
		}
		chain = append(chain, strconv.Itoa(left)+word)
	}
	return strings.Join(append(chain, link(len(cycle)-1)), ", ")
}

// cycles is one cycle of needs for each set of tasks that need each other: each strongly
// connected component of the tasks' needs that holds a cycle, which is one of more than one task
// or one task that needs itself (components, one pass over the tasks and their needs). The cycle
// is the shortest from the set's first task in id order back to that task, found breadth first
// inside the set (cycleFrom), as Dispatch's spec graph names its loops (findLoops in
// packages/envoy/internal/dispatch/specgraph). So the check takes time linear in the tasks and
// their needs, and a set gives one reason however many cycles run through it, where a search from
// each task of a set would follow each of its needs once for each task, quadratic on a ring of
// tasks, and Dispatch takes an owner's plan of 10,000 of them. Each cycle starts at its set's
// first task, and the cycles are in the order of those tasks' ids. A need is a task id
// (FromBlocks); one that names no task of tasks closes no cycle. A task joins at most one search,
// so the searches follow each need at most once; TestTheCheckOfCyclesTakesTimeLinearInThePlan
// holds the check to its thread's CPU time on a ring of 40,000 tasks.
func cycles(tasks []Task) (found [][]string) {
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
	component, count := components(needs)
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
		path := cycleFrom(needs, component, parent, first[c])
		cycle := make([]string, len(path))
		for i, task := range path {
			cycle[i] = tasks[task].ID
		}
		found = append(found, cycle)
	}
	slices.SortFunc(found, slices.Compare)
	return found
}

// components numbers the strongly connected components of the graph in which task i needs each
// task of needs[i], by Tarjan's algorithm kept on explicit stacks, so a chain of 10,000 needs
// costs no call stack: component[i] is task i's component, of count in all.
func components(needs [][]int) (component []int, count int) {
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
	return component, count
}

// cycleFrom is the shortest cycle from task back to task inside its component: the tasks from
// task to the last before it again, each needing the next, found breadth first with each task's
// needs in the order its block writes them. parent holds -1 for each task of the component;
// cycleFrom marks the tasks it reaches there, and no later call reads those marks, since each
// component is searched once.
func cycleFrom(needs [][]int, component, parent []int, task int) (path []int) {
	parent[task] = task
	queue := []int{task}
	for head := 0; head < len(queue); head++ {
		at := queue[head]
		for _, needed := range needs[at] {
			if needed == task {
				path = []int{at}
				for at != task {
					at = parent[at]
					path = append(path, at)
				}
				slices.Reverse(path)
				return path
			}
			if component[needed] == component[task] && parent[needed] < 0 {
				parent[needed] = at
				queue = append(queue, needed)
			}
		}
	}
	// A component of more than one task leads from each of its tasks back to it, and one of one
	// task holds a cycle only when that task needs itself: the search finds that need in its first
	// round, among the task's own needs, whichever place the block writes it at.
	panic(fmt.Sprintf("plan: task %d leads back to itself by no need inside its component", task))
}

// Supported is what this release does not carry out yet: more than one task, more than one
// task that no task needs (Sinks, over the task ids FromBlocks reads), whose work would each
// merge by itself where this release merges an issue's work as one change, a role block, and,
// for each task, `needs`, `owner`, `part_of` and `merges`, each key but the owner's on
// acceptance, acceptance with no key, a role named at two steps, no implementer, and each step
// of a role that reads the implementer's pull request with no implementer step before it. Its
// steps stand in any other order: the planner at any place, and the tester, the reviewer and the
// acceptance in any order after the implementer. A task that names no steps runs the project's
// default steps, which the configuration loader holds to these same checks, so none of its step
// causes apply. Its reasons are bounded as reasons bounds each kind.
func Supported(p Plan) []Refusal {
	var found reasons
	if len(p.Tasks) > 1 {
		found.yet(manyTasks, "", func() string {
			ids := make([]string, len(p.Tasks))
			for i, task := range p.Tasks {
				ids[i] = task.ID
			}
			return fmt.Sprintf("the plan has %d tasks (%s), and this release runs one task for each issue.", len(p.Tasks), listed(ids))
		})
	}
	if sinks := Sinks(p.Tasks); len(sinks) > 1 {
		found.yet(manySinks, "", func() string {
			return fmt.Sprintf("the plan has %d tasks that no task needs (%s), and the work of each would merge by itself.", len(sinks), listed(sinks))
		})
	}
	for _, role := range p.RoleBlocks {
		found.yet(roleBlock, "", func() string { return "the plan defines the role " + quoted(role) + "." })
	}
	for _, task := range p.Tasks {
		taskReasons(task, &found)
	}
	return found.refusals()
}

// taskReasons adds Supported's causes in one task to found: its attributes first, then its steps
// (stepReasons).
func taskReasons(t Task, found *reasons) {
	if len(t.Needs) > 0 {
		found.yet(taskNeeds, t.ID, func() string { return fmt.Sprintf("task %s needs %s.", t.ID, listed(t.Needs)) })
	}
	if t.Owner != "" {
		found.yet(taskOwner, t.ID, func() string { return fmt.Sprintf("task %s names owner.", t.ID) })
	}
	if t.PartOf != "" {
		found.yet(taskPartOf, t.ID, func() string { return fmt.Sprintf("task %s names part_of.", t.ID) })
	}
	if t.Merges {
		found.yet(taskMerges, t.ID, func() string { return fmt.Sprintf("task %s names merges.", t.ID) })
	}
	if t.NamesSteps() {
		stepReasons(t, found)
	}
}

// readsPullRequest is each role whose step reads the pull request the implementer's step opens:
// the tester and the reviewer judge its head, and the owner's acceptance reads the evidence of it.
// The implementer's move on waits for that pull request (the workflow's guard hasOpenOrMergedPR),
// so such a step with no implementer step before it has no pull request to read, and this release
// refuses it; a later release that gives such a step another pull request to read takes it. The
// role decides, never the step's type.
var readsPullRequest = map[string]bool{"tester": true, "reviewer": true, "acceptance": true}

// stepReasons adds Supported's causes in the steps of t to found, in this order: each step's key,
// in step order; each role named at two steps, at its first two; no implementer; and each step of
// a role that reads a pull request with no implementer step before it, in step order, named by
// its word as the plan writes it.
func stepReasons(t Task, found *reasons) {
	firstAt := map[string]int{}
	var roles []string // each role once, in the order of its first step
	secondAt := map[string]int{}
	hasImplementer := false
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case step.Role == "acceptance" && step.Key == "":
			found.yet(acceptanceNoKey, t.ID, func() string {
				return fmt.Sprintf("step %d of task %s names acceptance with no key, which asks for a fresh agent of the role acceptance.", at, t.ID)
			})
		case step.Key != "" && (step.Role != "acceptance" || step.Key != "owner"):
			found.yet(stepKey, t.ID, func() string { return fmt.Sprintf("step %d of task %s names the key %s.", at, t.ID, quoted(step.Key)) })
		}
		if _, seen := firstAt[step.Role]; !seen {
			firstAt[step.Role] = at
			roles = append(roles, step.Role)
		} else if _, seen := secondAt[step.Role]; !seen {
			secondAt[step.Role] = at
		}
		hasImplementer = hasImplementer || step.Role == "implementer"
	}
	for _, role := range roles {
		if second, ok := secondAt[role]; ok {
			found.yet(roleTwice, t.ID, func() string {
				return fmt.Sprintf("task %s names the role %s at steps %d and %d.", t.ID, quoted(role), firstAt[role], second)
			})
		}
	}
	if !hasImplementer {
		found.yet(noImplementer, t.ID, func() string { return fmt.Sprintf("task %s has no step implementer.", t.ID) })
	}
	for i, step := range t.Steps {
		if step.Role == "implementer" {
			break
		}
		if readsPullRequest[step.Role] {
			found.yet(readsBeforeImplementer, t.ID, func() string {
				word := quoted(step.String())
				return fmt.Sprintf("step %d of task %s names %s, and no step before it names the role implementer, whose pull request %s reads.",
					i+1, t.ID, word, word)
			})
		}
	}
}
