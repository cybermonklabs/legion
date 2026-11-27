package plan

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// The two checks a plan passes before Legion takes it. Both run on every plan: register_plan's
// (api's checkPlan), the configuration loader's check of a project's default steps, and taken. Each
// gives a reason for each cause it finds, up to the bound of its kind, so a plan with two causes
// gets two reasons, and the owner reads every cause at once rather than one for each try. Each
// reason names its task where it has one. Each check bounds its own kinds (reasons), and the two
// share none, so a caller that joins their reasons bounds each kind of the refusal: one refusal
// names at most MostReasons causes of each kind and counts the rest, and each reason quotes the
// plan within a bound of bytes: a task id of at most steprecord.TaskIDMax (FromBlocks refuses one
// outside Dispatch's task form), a word of at most MostQuotedBytes (quoted) and a chain of at most
// MostReasons needs, of two ids each (chainOf).

// MostReasons is the most causes of one kind that one refusal of a plan names; one more reason
// counts the rest of that kind (reasons). It bounds a chain as well: a cycle's chain names at most
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

// noTask is the reason a plan with no task block gets. No later release takes such a plan, so it
// is Validate's and does not say "yet".
const noTask = "the plan has no task block, so it gives Legion no work."

// notYet ends each reason of Supported: what a later release carries out.
const notYet = " This release of Legion does not carry it out yet."

// cause is the kind of a cause of a refusal, one for each reason Supported and Validate write.
// reasons bounds each kind apart, so no kind of cause pushes another out of the answer.
type cause int

// The kinds, in the order the checks find them: Supported's, then Validate's. One gives one reason
// in a plan at most (no task block); the others give one for each item.
const (
	roleBlock cause = iota
	taskOwner
	taskPartOf
	acceptanceNoKey
	stepKey
	roleTwice
	noImplementer
	readsBeforeImplementer
	implementationAfterImplementer
	noTaskBlock
	missingParent
	elementNames
	unresolvedNeed
	elementNeed
	outsideIssue
	undefinedRole
	needsItself
	subOwnerSteps
	partNeed
	agentTaken
	causes // how many kinds there are
)

// causeWords is each kind as the reason that counts the rest of it names it.
var causeWords = [causes]string{
	roleBlock:                      "roles the plan defines",
	taskOwner:                      "tasks that name owner",
	taskPartOf:                     "tasks that are a part of another task",
	acceptanceNoKey:                "steps that name acceptance with no key",
	stepKey:                        "steps that name the key owner on a role but acceptance, or acceptance with a key but owner",
	roleTwice:                      "roles a task names at two steps",
	noImplementer:                  "tasks with no step implementer",
	readsBeforeImplementer:         "steps that read a pull request with no implementer step before them",
	implementationAfterImplementer: "steps of the type implementation after the step implementer",
	noTaskBlock:                    "plans with no task block",
	missingParent:                  "task blocks whose parent no typed block of the plan holds",
	elementNames:                   "elements that name steps, owner or merges",
	unresolvedNeed:                 "needs that name no task of this plan",
	elementNeed:                    "needs that name an element",
	outsideIssue:                   "needs that name a task outside the plan's issue",
	undefinedRole:                  "roles that neither the deployment nor the plan defines",
	needsItself:                    "sets of tasks that need each other",
	subOwnerSteps:                  "tasks that name both steps and a sub-owner",
	partNeed:                       "needs that the rules of parts refuse",
	agentTaken:                     "sub-owners whose agent takes the name of another sub-owner's",
}

// reasons is the reasons of one refusal of a plan, as Supported and Validate find them: of each
// kind of cause, the first MostReasons in the order found, and past them one more reason that
// counts the rest of that kind, right after the kind's last reason named. The reasons named keep
// the order the checks find them in. Every cause of either check is added here with its kind, so
// each kind a plan has is named at least once, and a plan whose blocks give a cause for each of
// their items (each `needs` word that names no task, each role nobody defines, each step's refused
// key, each step that reads a pull request with no implementer before it) gets a refusal of at most
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
// with the chain from that task (chainOf); then the causes of the tasks with a sub-owner, of their
// parts, and of each need of a part (partRefusals), a task of the top level's among them, which
// read the needs on those cycles from the same pass, so a need on a cycle gets the cycle's reason
// alone. It refuses no plan for a step the plan lacks. Its reasons are bounded as reasons bounds
// each kind. Its one error is ctx's, once ctx ends before the checks of cycles and of parts are
// done: an owner or a sub-owner writes the plan, and register_plan checks it before the fact lock,
// where only the request's own end and the daemon's own deadline for the check (api's
// planCheckBound) can stop it.
func Validate(ctx context.Context, p Plan, d Deployment) ([]Refusal, error) {
	var found reasons
	if len(p.Tasks) == 0 {
		found.add(noTaskBlock, "", func() string { return noTask })
		return found.refusals(), nil
	}
	defined := make(map[string]bool, len(p.RoleBlocks))
	for _, role := range p.RoleBlocks {
		defined[role] = true
	}
	for _, task := range p.Tasks {
		if task.MissingParent != "" {
			found.add(missingParent, task.ID, func() string {
				return fmt.Sprintf("task block %s names %s as its parent, which no typed block of version %d of the plan holds. "+
					"Name a typed block of this version in parent, or leave parent out.", task.ID, quoted(task.MissingParent), p.Version)
			})
		}
	}
	elements := make(map[string]Element, len(p.Elements))
	for _, element := range p.Elements {
		elements[element.ID] = element
		if len(element.Names) == 0 {
			continue
		}
		found.add(elementNames, element.ID, func() string {
			under := fmt.Sprintf("%s, which names no sub-owner", element.Under)
			if element.Under != element.Of {
				under = fmt.Sprintf("%s, an element of %s", element.Under, element.Of)
			}
			return fmt.Sprintf("task block %s is an element of task %s: it stands under %s. "+
				"Legion does not carry out an element, so an element names no steps, owner or merges, and %s names %s.",
				element.ID, element.Of, under, element.ID, wordList(element.Names))
		})
	}
	for _, task := range p.Tasks {
		for _, word := range task.Unresolved {
			found.add(unresolvedNeed, task.ID, func() string {
				return fmt.Sprintf("task %s needs %s, which names no task of this plan.", task.ID, quoted(word))
			})
		}
		for _, word := range task.ElementNeeds {
			found.add(elementNeed, task.ID, func() string {
				return fmt.Sprintf("task %s needs %s, which is an element of task %s. "+
					"Legion does not carry out an element, so a task cannot need one.", task.ID, quoted(word), elements[neededID(word)].Of)
			})
		}
		for _, word := range task.OutsideIssue {
			found.add(outsideIssue, task.ID, func() string {
				if other, _ := issueOf(word); other != "" {
					return fmt.Sprintf("task %s needs %s, which names another issue, %s: "+
						"a task can need only a task of this plan, and an issue waits for another issue by a blocked_by link between the two.", task.ID, quoted(word), other)
				}
				return fmt.Sprintf("task %s needs %s, which names no issue: "+
					"a task can need only a task of this plan, and a reference to one starts with the plan's issue key.", task.ID, quoted(word))
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ended := stepsOf(ctx)
	sets, onCycle, err := cycles(p.Tasks, ended)
	if err != nil {
		return nil, err
	}
	for _, cycle := range sets {
		found.add(needsItself, cycle[0], func() string {
			return fmt.Sprintf("task %s needs itself: %s.", cycle[0], chainOf(cycle))
		})
	}
	if err := partRefusals(p.Tasks, onCycle, ended, &found); err != nil {
		return nil, err
	}
	return found.refusals(), nil
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

// partRefusals adds to found Validate's causes of the tasks with a sub-owner, of their parts, and
// of each need of a part, in task id order, a task's own causes together, one for each cause:
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
func partRefusals(tasks []Task, onCycle map[waitEdge]bool, ended func() error, found *reasons) error {
	w := newWaits(tasks)
	refused, err := w.needReasons(onCycle, ended)
	if err != nil {
		return err
	}
	named := map[claim.Agent]string{}
	for _, id := range w.ids {
		task := w.task[id]
		if task.Owner != "" && task.NamesSteps() {
			found.add(subOwnerSteps, id, func() string {
				return fmt.Sprintf("task %s names both steps and the sub-owner %s, and a task with a sub-owner has no steps: its work is its parts.", id, quoted(task.Owner))
			})
		}
		for _, need := range task.Needs {
			if reason, ok := refused[waitEdge{id, need}]; ok {
				found.add(partNeed, id, func() string { return reason })
			}
		}
		if task.Owner == "" {
			continue
		}
		agent := SubOwnerAgent(id)
		if first, taken := named[agent]; taken {
			found.add(agentTaken, id, func() string {
				return fmt.Sprintf("task %s names a sub-owner, and its agent takes the name %s, which the agent of the sub-owner of task %s takes too: give one of the two tasks another id.",
					id, agent, first)
			})
			continue
		}
		named[agent] = id
	}
	return nil
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
// their needs, and a set gives one reason however many cycles run through it, where a search from
// each task of a set would follow each of its needs once for each task, quadratic on a ring of
// tasks, and Dispatch takes an owner's plan of 10,000 of them. Each cycle starts at its set's
// first task, and the cycles are in the order of those tasks' ids. onCycle is each need that lies
// on a cycle, from the same pass: a need between two tasks of one component, which holds a cycle
// through each need inside it, a task's need of itself among them. The rules of parts give such a
// need no reason of their own (partRefusals), since its cycle's reason names it. A need is a task
// id (FromBlocks); one that names no task of tasks closes no cycle. A task joins at most one
// search, so the searches follow each need at most once;
// TestTheCheckOfCyclesTakesTimeLinearInThePlan holds the check to its thread's CPU time on a ring
// of 40,000 tasks. cycles reads ended at each step (stepsOf) and answers its error once it has one.
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
	// task holds a cycle only when that task needs itself: the search finds that need in its first
	// round, among the task's own needs, whichever place the block writes it at.
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
// and each step after the implementer's whose role d gives the type implementation, since Legion's
// retro is done by the implementer. A plan of several tasks, and a task's `needs`, are taken: each
// task runs once the tasks it needs are done. So is a plan in which the work of several tasks
// merges, each by itself (Merging). Any other key is taken: it names the agent of its steps
// (AgentOf). Its steps stand in any other order: the planner at any place, the tester, the
// reviewer and the acceptance in any order after the implementer, and a role of the deployment at
// any place. A task that names no steps runs the project's default steps, which the configuration
// loader holds to these same checks, so none of its step causes apply. Its reasons are bounded as
// reasons bounds each kind.
func Supported(p Plan, d Deployment) []Refusal {
	var found reasons
	for _, role := range p.RoleBlocks {
		found.yet(roleBlock, "", func() string { return "the plan defines the role " + quoted(role) + "." })
	}
	for _, task := range p.Tasks {
		taskReasons(task, d, &found)
	}
	return found.refusals()
}

// taskReasons adds Supported's causes in one task to found: its attributes first, then its steps
// (stepReasons).
func taskReasons(t Task, d Deployment, found *reasons) {
	if t.Owner != "" {
		found.yet(taskOwner, t.ID, func() string { return fmt.Sprintf("task %s names owner.", t.ID) })
	}
	if t.PartOf != "" {
		found.yet(taskPartOf, t.ID, func() string { return fmt.Sprintf("task %s is a part of task %s.", t.ID, t.PartOf) })
	}
	if t.NamesSteps() {
		stepReasons(t, d, found)
	}
}

// readsPullRequest is each role whose step reads the pull request the implementer's step opens:
// the tester and the reviewer judge its head, and the owner's acceptance reads the evidence of it.
// The implementer's move on waits for that pull request (the workflow's guard hasOpenOrMergedPR),
// so such a step with no implementer step before it has no pull request to read, and this release
// refuses it; a later release that gives such a step another pull request to read takes it. A
// role of the deployment reports pass or fail and reads no pull request, so it stands anywhere.
// The role decides, never the step's type.
var readsPullRequest = map[string]bool{"tester": true, "reviewer": true, "acceptance": true}

// stepReasons adds Supported's causes in the steps of t to found, in this order: each step that
// names the key owner on a role but acceptance, or acceptance with a key but owner or with none,
// in step order; each role named at two steps, at its first two; no implementer; each step of a
// role that reads a pull request with no implementer step before it, in step order, named by its
// word as the plan writes it; and each step after the implementer's whose role d gives the type
// implementation, in step order. The owner does the accepting step in this release, so a key on
// it but owner would name an agent Legion does not run it in.
func stepReasons(t Task, d Deployment, found *reasons) {
	firstAt := map[string]int{}
	var roles []string // each role once, in the order of its first step
	secondAt := map[string]int{}
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case step.Role == "acceptance" && step.Key == "":
			found.yet(acceptanceNoKey, t.ID, func() string {
				return fmt.Sprintf("step %d of task %s names acceptance with no key, which asks for a fresh agent of the role acceptance.", at, t.ID)
			})
		// The key owner and the role acceptance go together: either one without the other is refused.
		case step.Key != "" && (step.Key == "owner") != (step.Role == "acceptance"):
			found.yet(stepKey, t.ID, func() string { return fmt.Sprintf("step %d of task %s names the key %s.", at, t.ID, quoted(step.Key)) })
		}
		if _, seen := firstAt[step.Role]; !seen {
			firstAt[step.Role] = at
			roles = append(roles, step.Role)
		} else if _, seen := secondAt[step.Role]; !seen {
			secondAt[step.Role] = at
		}
	}
	for _, role := range roles {
		if second, ok := secondAt[role]; ok {
			found.yet(roleTwice, t.ID, func() string {
				return fmt.Sprintf("task %s names the role %s at steps %d and %d.", t.ID, quoted(role), firstAt[role], second)
			})
		}
	}
	implementerAt := firstAt["implementer"] // the place of the first step of the implementer, 0 with none
	if implementerAt == 0 {
		found.yet(noImplementer, t.ID, func() string { return fmt.Sprintf("task %s has no step implementer.", t.ID) })
	}
	for i, step := range t.Steps {
		at := i + 1
		switch {
		case (implementerAt == 0 || at < implementerAt) && readsPullRequest[step.Role]:
			found.yet(readsBeforeImplementer, t.ID, func() string {
				word := quoted(step.String())
				return fmt.Sprintf("step %d of task %s names %s, and no step before it names the role implementer, whose pull request %s reads; a role of this deployment that reports pass or fail needs none.",
					at, t.ID, word, word)
			})
		case implementerAt > 0 && at > implementerAt && step.Role != "implementer" && d.Roles[step.Role].Type == TypeImplementation:
			found.yet(implementationAfterImplementer, t.ID, func() string {
				return fmt.Sprintf("task %s names %s at step %d, a step of the type implementation after the step implementer at step %d, and Legion's retro is done by the implementer in this release.",
					t.ID, step.Role, at, implementerAt)
			})
		}
	}
}
