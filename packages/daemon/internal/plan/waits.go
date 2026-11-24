package plan

import (
	"fmt"
	"maps"
	"slices"
)

// waits is the tasks of a plan as the rule of a task's start and end reads them: a task starts once
// each task it needs is done and, for a part, once its task has started (it stands at its parts),
// and a task is done only after it started and each of its parts is done. A need is a task id
// (FromBlocks); one that names no task of the plan is Validate's to refuse, and waits on nothing.
type waits struct {
	ids   []string
	task  map[string]Task
	parts map[string][]string
}

// newWaits is the waits of tasks, their ids and each task's parts in id order.
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
	return w
}

// above is each task that id is a part of, the nearest first. Places refuses parents in a cycle, and
// the walk stops at a task it has passed, so it ends for any plan.
func (w waits) above(id string) []string {
	var tasks []string
	seen := map[string]bool{id: true}
	for at := w.task[id].PartOf; at != "" && !seen[at]; at = w.task[at].PartOf {
		seen[at] = true
		tasks = append(tasks, at)
	}
	return tasks
}

// holds says whether task holds id: id is task, or a part of it, directly or through its tasks.
func (w waits) holds(task, id string) bool {
	return id == task || slices.Contains(w.above(id), task)
}

// outsideOf is the task that id stands outside where its need need is a part of it: the outermost
// task above need that does not hold id. ok is false where need is no part, or where each task
// above need holds id.
func (w waits) outsideOf(id, need string) (string, bool) {
	outside := ""
	for _, above := range w.above(need) {
		if w.holds(above, id) {
			break
		}
		outside = above
	}
	return outside, outside != ""
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
// it (why), and ok false where to is not reached.
func (w waits) waitPath(from, to waitNode, cut map[waitEdge]bool) ([]string, bool) {
	path := shortestPath(from, to, func(node waitNode) []waitNode { return w.next(node, cut) })
	if path == nil {
		return nil, false
	}
	var words []string
	for i := 1; i < len(path); i++ {
		if said := why(path[i-1], path[i]); said != "" {
			words = append(words, said)
		}
	}
	return words, true
}

// needReasons is the reason of each need that the rules of parts refuse, keyed by the task and the
// need, at most one for each need. found is the plan's cycles of needs (cycles), whose needs
// Validate names in the reason of their cycle, so they get none here and are left out of every wait
// below. Then each need of a part of another task (partOfAnother), in task id order and each task's
// needs in its block's order, gets that reason and is left out too. Then each need of a task that
// is done only after that task itself starts, so the task never starts and the tasks above it are
// never done. The need that causes the wait is named, not each need the wait then holds back, in
// two passes over the needs, each in task id order and each task's needs in its block's order:
//  1. A part's need of a task it is a part of, directly or through its tasks; a part's need of a
//     task outside a task above it whose end waits for that task's end; and a task's need of a part
//     of its own, which starts only once the task has started.
//  2. Each other need still done only after its task starts, with the needs the first pass named
//     left out, so a plan this function answers nothing for holds no task that never starts.
//
// A need of a task it is a part of takes the reason of S3: the task is done only after each of its
// parts. Each other reason of a wait names the need and the waits from its end to the task's start.
func (w waits) needReasons(found [][]string) map[waitEdge]string {
	cut := map[waitEdge]bool{}
	for _, cycle := range found {
		for i, id := range cycle {
			cut[waitEdge{id, cycle[(i+1)%len(cycle)]}] = true
		}
	}
	reasons := map[waitEdge]string{}
	for _, id := range w.ids {
		for _, need := range w.task[id].Needs {
			edge := waitEdge{id, need}
			if cut[edge] {
				continue
			}
			if reason, ok := w.partOfAnother(id, need); ok {
				reasons[edge], cut[edge] = reason, true
			}
		}
	}
	// causes says whether id's need is a cause of the first pass. A need of id itself closes a cycle
	// of needs, which is cut before this reads it.
	causes := func(id, need string) bool {
		if w.holds(id, need) {
			return true
		}
		for _, above := range w.above(id) {
			if need == above {
				return true
			}
			if _, ends := w.waitPath(waitNode{need, false}, waitNode{above, false}, cut); ends && !w.holds(above, need) {
				return true
			}
		}
		return false
	}
	for pass := 1; pass <= 2; pass++ {
		for _, id := range w.ids {
			for _, need := range w.task[id].Needs {
				edge := waitEdge{id, need}
				if _, known := w.task[need]; !known || cut[edge] {
					continue
				}
				words, never := w.waitPath(waitNode{need, false}, waitNode{id, true}, cut)
				if !never || pass == 1 && !causes(id, need) {
					continue
				}
				if slices.Contains(w.above(id), need) {
					reasons[edge] = fmt.Sprintf("task %s needs %s, which it is a part of, and %s is done only after each of its parts.", id, need, need)
				} else {
					reasons[edge] = fmt.Sprintf("task %s needs %s, which is done only after %s starts: %s.", id, need, id, wordList(words))
				}
				cut[edge] = true
			}
		}
	}
	return reasons
}
