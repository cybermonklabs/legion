package plan

import (
	"fmt"
	"maps"
	"math/bits"
	"slices"
)

// waits is the tasks of a plan as the rule of a task's start and end reads them: a task starts once
// each task it needs is done and, for a part, once its task has started (it stands at its parts),
// and a task is done only after it started and each of its parts is done. A need is a task id
// (FromBlocks); one that names no task of the plan is Validate's to refuse, and waits on nothing.
//
// An owner or a sub-owner writes the plan, and register_plan checks it before the fact lock, so no
// read here walks the plan once for each task or each need: parts nest as deep as the plan has
// tasks, and a walk up the whole nesting for each need cost the depth times the needs. names is
// each task id in id order, then each PartOf that names no task of the plan, in id order, which the
// rule reads as a task with no needs and no task above it; at is each name's index in names. up[k][i]
// is the index of the name 2^k steps above name i through PartOf, -1 above the top, and depth[i]
// the count of names above name i, so holds and outsideOf climb the nesting by halves.
type waits struct {
	ids   []string
	task  map[string]Task
	parts map[string][]string
	names []string
	at    map[string]int
	up    [][]int
	depth []int
}

// newWaits is the waits of tasks, their ids and each task's parts in id order, with the index of
// the nesting that holds and outsideOf climb. Places refuses parents in a cycle; in a plan built by
// hand with a cycle of PartOf, the PartOf that closes it is read as none, so every climb ends.
func newWaits(tasks []Task) waits {
	w := waits{task: make(map[string]Task, len(tasks)), parts: map[string][]string{}}
	for _, task := range tasks {
		w.task[task.ID] = task
		if task.PartOf != "" {
			w.parts[task.PartOf] = append(w.parts[task.PartOf], task.ID)
		}
	}
	for _, parts := range w.parts {
		slices.Sort(parts)
	}
	w.ids = slices.Sorted(maps.Keys(w.task))
	w.names = slices.Clone(w.ids)
	for _, of := range slices.Sorted(maps.Keys(w.parts)) {
		if _, known := w.task[of]; !known {
			w.names = append(w.names, of)
		}
	}
	w.at = make(map[string]int, len(w.names))
	for i, name := range w.names {
		w.at[name] = i
	}
	parent := make([]int, len(w.names))
	for i, name := range w.names {
		parent[i] = -1
		if of := w.task[name].PartOf; of != "" {
			parent[i] = w.at[of]
		}
	}
	// Each name's depth, by one walk up from it to a name whose depth is known, so each name is
	// walked once. A name met twice on one walk closes a cycle of PartOf, cut at the last name of
	// the walk.
	w.depth = make([]int, len(w.names))
	for i := range w.depth {
		w.depth[i] = -1
	}
	onWalk := make([]bool, len(w.names))
	var walk []int
	for i := range w.names {
		walk = walk[:0]
		at := i
		for at >= 0 && w.depth[at] < 0 && !onWalk[at] {
			onWalk[at] = true
			walk = append(walk, at)
			at = parent[at]
		}
		if at >= 0 && onWalk[at] {
			parent[walk[len(walk)-1]], at = -1, -1
		}
		depth := -1
		if at >= 0 {
			depth = w.depth[at]
		}
		for j := len(walk) - 1; j >= 0; j-- {
			depth++
			w.depth[walk[j]], onWalk[walk[j]] = depth, false
		}
	}
	w.up = [][]int{parent}
	for k := 1; k < bits.Len(uint(len(w.names))); k++ {
		half, up := w.up[k-1], make([]int, len(w.names))
		for i, above := range half {
			up[i] = -1
			if above >= 0 {
				up[i] = half[above]
			}
		}
		w.up = append(w.up, up)
	}
	return w
}

// climb is the index of the name steps above name i, -1 where the nesting above i holds fewer.
func (w waits) climb(i, steps int) int {
	for k := 0; steps > 0 && i >= 0 && k < len(w.up); k++ {
		if steps&1 == 1 {
			i = w.up[k][i]
		}
		steps >>= 1
	}
	if steps > 0 {
		return -1
	}
	return i
}

// holds says whether task holds id: id is task, or a part of it, directly or through its tasks.
func (w waits) holds(task, id string) bool {
	if id == task {
		return true
	}
	t, known := w.at[task]
	i, ok := w.at[id]
	if !known || !ok || w.depth[i] <= w.depth[t] {
		return false
	}
	return w.climb(i, w.depth[i]-w.depth[t]) == t
}

// outsideOf is the task that id stands outside where its need need is a part of it: the outermost
// task above need that does not hold id. ok is false where need is no part, or where each task
// above need holds id. The tasks above need that hold id are the outermost ones, since a task above
// one that holds id holds it too, so the climb takes each step that leads to a task that does not.
func (w waits) outsideOf(id, need string) (string, bool) {
	from, known := w.at[need]
	if !known {
		return "", false
	}
	outside := from
	for k := len(w.up) - 1; k >= 0; k-- {
		if above := w.up[k][outside]; above >= 0 && !w.holds(w.names[above], id) {
			outside = above
		}
	}
	if outside == from {
		return "", false
	}
	return w.names[outside], true
}

// partOfAnother is the reason of id's need need where need is a part of another task, which no task
// needs: need is a part (its PartOf is not ""), and not of the task id is a part of (a sibling) nor
// of id itself. A need of a task that id is a part of, directly or through its tasks, is left to
// the waits (needReasons), with S3's reason of that need. A task outside the task need is a part of
// needs the whole task, named at the outermost task above need that does not hold id (outsideOf);
// a part whose task stands under that task needs only the other parts of its own task, or a whole
// task of the top level. ok is false for each other need.
func (w waits) partOfAnother(id, need string) (string, bool) {
	of := w.task[need].PartOf
	if of == "" || of == w.task[id].PartOf || of == id || w.holds(need, id) {
		return "", false
	}
	if outside, ok := w.outsideOf(id, need); ok {
		return fmt.Sprintf("task %s needs %s, a part of task %s: a task outside %s needs the whole task %s, and no part of it.", id, need, outside, outside, outside), true
	}
	return fmt.Sprintf("task %s needs %s, a part of task %s: a part needs only the other parts of its own task, or a whole task of the top level.", id, need, of), true
}

// waitNode is one moment of a task the rule of its start and end reads: its start, or its end.
type waitNode struct {
	task  string
	start bool
}

// waitEdge is one need of a plan: the task, and the task it needs.
type waitEdge = [2]string

// node is the index of a moment among the moments of names: twice its task's index for its end,
// and one more for its start. ok is false for a task that is no name.
func (w waits) node(n waitNode) (int, bool) {
	i, ok := w.at[n.task]
	if n.start {
		return 2*i + 1, ok
	}
	return 2 * i, ok
}

// next is each moment node waits for. cut are the needs left out, each refused by another cause.
// The end of a task waits for its start and for the end of each of its parts, in id order; the
// start of a task waits for the end of each task it needs, in its block's order, and the start of a
// part for the start of its task.
func (w waits) next(node waitNode, cut map[waitEdge]bool) []waitNode {
	if !node.start {
		nodes := []waitNode{{node.task, true}}
		for _, part := range w.parts[node.task] {
			nodes = append(nodes, waitNode{part, false})
		}
		return nodes
	}
	var nodes []waitNode
	for _, need := range w.task[node.task].Needs {
		if _, known := w.task[need]; known && !cut[waitEdge{node.task, need}] {
			nodes = append(nodes, waitNode{need, false})
		}
	}
	if of := w.task[node.task].PartOf; of != "" {
		nodes = append(nodes, waitNode{of, true})
	}
	return nodes
}

// cyclesOfWaits is the strongly connected component of each moment of names (node) in the waits
// with cut left out (components): the moments that wait for each other, through needs and the
// edges of parts. It reads ended once for each moment and each wait.
func (w waits) cyclesOfWaits(cut map[waitEdge]bool, ended func() error) ([]int, error) {
	waitsFor := make([][]int, 2*len(w.names))
	for _, name := range w.names {
		for _, start := range []bool{false, true} {
			from, _ := w.node(waitNode{name, start})
			for _, to := range w.next(waitNode{name, start}, cut) {
				if at, ok := w.node(to); ok {
					waitsFor[from] = append(waitsFor[from], at)
				}
			}
		}
	}
	component, _, err := components(waitsFor, ended)
	return component, err
}

// why is the words of the wait of from for to, as a reason writes them: "" for an end's wait for
// its own start, which a reason does not need to say.
func why(from, to waitNode) string {
	switch {
	case !from.start && to.start:
		return ""
	case !from.start:
		return fmt.Sprintf("%s is done only after its part %s", from.task, to.task)
	case !to.start:
		return fmt.Sprintf("%s needs %s", from.task, to.task)
	default:
		return fmt.Sprintf("%s, a part of %s, starts only once %s has started", from.task, to.task, to.task)
	}
}

// waitPath is the fewest waits from from to to (shortestPath over next), each as the words that say
// it (why), and ok false where to is not reached. The search keeps to the moments of component c of
// component (cyclesOfWaits), which holds both ends: every moment of a path between two moments of
// one component lies in it, so the path is the one a search of every moment finds.
func (w waits) waitPath(from, to waitNode, cut map[waitEdge]bool, component []int, c int, ended func() error) ([]string, bool, error) {
	inside := func(node waitNode) bool {
		at, ok := w.node(node)
		return ok && component[at] == c
	}
	path, err := shortestPath(from, to, func(node waitNode) []waitNode {
		return slices.DeleteFunc(w.next(node, cut), func(next waitNode) bool { return !inside(next) })
	}, ended)
	if err != nil || path == nil {
		return nil, false, err
	}
	var words []string
	for i := 1; i < len(path); i++ {
		if said := why(path[i-1], path[i]); said != "" {
			words = append(words, said)
		}
	}
	return words, true, nil
}

// needReasons is the reason of each need that the rules of parts refuse, keyed by the task and the
// need, at most one for each need. onCycle is each need on a cycle of needs (cycles), which
// Validate names in the reason of its set's cycle, so they get none here and are left out of every
// wait below. Then each need of a part of another task (partOfAnother), in task id order and each
// task's needs in its block's order, gets that reason and is left out too. Then each need of a task
// that is done only after that task itself starts, so the task never starts and the tasks above it
// are never done. The need that causes the wait is named, not each need the wait then holds back,
// in two passes over the needs, each in task id order and each task's needs in its block's order:
//  1. A part's need of a task it is a part of, directly or through its tasks; a part's need of a
//     task outside a task above it whose end waits for that task's end; and a task's need of a part
//     of its own, which starts only once the task has started.
//  2. Each other need still done only after its task starts, with the needs the first pass named
//     left out, so a plan this function answers nothing for holds no task that never starts.
//
// A need of a task it is a part of takes the reason of S3: the task is done only after each of its
// parts. Each other reason of a wait names the need and the waits from its end to the task's start.
//
// A need whose end the task's start waits for lies on a cycle of waits with that start, so the two
// lie in one component of the waits as they stand before either pass (cyclesOfWaits), and a need
// whose two moments lie in two components is searched for no wait at all. That is every need of a
// plan with no part: with the cycles of needs left out, only a part's wait for its task's start
// and a task's end's wait for its parts close a cycle of waits. A search runs inside one component
// alone, so its cost is that component's, and a chain of needs costs one pass over the plan. The
// passes only leave needs out, so a later wait lies inside a component of the first. It reads ended
// once for each need and each moment it reaches, and answers its error once it has one.
func (w waits) needReasons(onCycle map[waitEdge]bool, ended func() error) (map[waitEdge]string, error) {
	cut := make(map[waitEdge]bool, len(onCycle))
	maps.Copy(cut, onCycle)
	reasons := map[waitEdge]string{}
	for _, id := range w.ids {
		for _, need := range w.task[id].Needs {
			if err := ended(); err != nil {
				return nil, err
			}
			edge := waitEdge{id, need}
			if cut[edge] {
				continue
			}
			if reason, ok := w.partOfAnother(id, need); ok {
				reasons[edge], cut[edge] = reason, true
			}
		}
	}
	component, err := w.cyclesOfWaits(cut, ended)
	if err != nil {
		return nil, err
	}
	componentOf := func(node waitNode) int {
		at, _ := w.node(node)
		return component[at]
	}
	// waitsFor is the wait of from for to inside their one component, and ok false where they lie
	// in two, where no wait of either pass joins them.
	waitsFor := func(from, to waitNode) ([]string, bool, error) {
		c := componentOf(from)
		if componentOf(to) != c {
			return nil, false, nil
		}
		return w.waitPath(from, to, cut, component, c, ended)
	}
	// causes says whether id's need is a cause of the first pass. A need of id itself closes a cycle
	// of needs, which is cut before this reads it. The end of a task above id waits for need's end
	// only where it lies in the component of need's end, and the tasks above id whose ends lie there
	// are the nearest ones: each end on the way from such a task's end down to id's end, and on to
	// need's end, lies in it too.
	causes := func(id, need string) (bool, error) {
		if w.holds(id, need) || w.holds(need, id) {
			return true, nil
		}
		end := componentOf(waitNode{need, false})
		for above := w.up[0][w.at[id]]; above >= 0; above = w.up[0][above] {
			name := w.names[above]
			if componentOf(waitNode{name, false}) != end {
				break
			}
			_, ends, err := waitsFor(waitNode{need, false}, waitNode{name, false})
			if err != nil {
				return false, err
			}
			if ends && !w.holds(name, need) {
				return true, nil
			}
		}
		return false, nil
	}
	for pass := 1; pass <= 2; pass++ {
		for _, id := range w.ids {
			for _, need := range w.task[id].Needs {
				if err := ended(); err != nil {
					return nil, err
				}
				edge := waitEdge{id, need}
				if _, known := w.task[need]; !known || cut[edge] {
					continue
				}
				words, never, err := waitsFor(waitNode{need, false}, waitNode{id, true})
				if err != nil {
					return nil, err
				}
				if !never {
					continue
				}
				if pass == 1 {
					cause, err := causes(id, need)
					if err != nil {
						return nil, err
					}
					if !cause {
						continue
					}
				}
				if w.holds(need, id) {
					reasons[edge] = fmt.Sprintf("task %s needs %s, which it is a part of, and %s is done only after each of its parts.", id, need, need)
				} else {
					reasons[edge] = fmt.Sprintf("task %s needs %s, which is done only after %s starts: %s.", id, need, id, wordList(words))
				}
				cut[edge] = true
			}
		}
	}
	return reasons, nil
}
