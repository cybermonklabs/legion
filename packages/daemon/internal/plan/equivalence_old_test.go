package plan

// Throwaway: the rules of parts as b7b6e1fd75ac held them (waits.go and cycleNeeds), copied under
// old names, against this head's needReasons, over random plans. Never merged.

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

type oldWaits struct {
	ids   []string
	task  map[string]Task
	parts map[string][]string
}

func newOldWaits(tasks []Task) oldWaits {
	w := oldWaits{task: make(map[string]Task, len(tasks)), parts: map[string][]string{}}
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

func (w oldWaits) above(id string) []string {
	var tasks []string
	seen := map[string]bool{id: true}
	for at := w.task[id].PartOf; at != "" && !seen[at]; at = w.task[at].PartOf {
		seen[at] = true
		tasks = append(tasks, at)
	}
	return tasks
}

func (w oldWaits) holds(task, id string) bool {
	return id == task || slices.Contains(w.above(id), task)
}

func (w oldWaits) outsideOf(id, need string) (string, bool) {
	outside := ""
	for _, above := range w.above(need) {
		if w.holds(above, id) {
			break
		}
		outside = above
	}
	return outside, outside != ""
}

func (w oldWaits) partOfAnother(id, need string) (string, bool) {
	of := w.task[need].PartOf
	if of == "" || of == w.task[id].PartOf || of == id || w.holds(need, id) {
		return "", false
	}
	if outside, ok := w.outsideOf(id, need); ok {
		return fmt.Sprintf("task %s needs %s, a part of task %s: a task outside %s needs the whole task %s, and no part of it.", id, need, outside, outside, outside), true
	}
	return fmt.Sprintf("task %s needs %s, a part of task %s: a part needs only the other parts of its own task, or a whole task of the top level.", id, need, of), true
}

func (w oldWaits) next(node waitNode, cut map[waitEdge]bool) []waitNode {
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

func oldShortestPath[N comparable](from, to N, next func(N) []N) []N {
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

func (w oldWaits) waitPath(from, to waitNode, cut map[waitEdge]bool) ([]string, bool) {
	path := oldShortestPath(from, to, func(node waitNode) []waitNode { return w.next(node, cut) })
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

func (w oldWaits) needReasons(onCycle map[waitEdge]bool) map[waitEdge]string {
	cut := maps.Clone(onCycle)
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

func oldCycleNeeds(tasks []Task) map[waitEdge]bool {
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
	component, _, _ := components(needs, func() error { return nil })
	onCycle := map[waitEdge]bool{}
	for i, task := range tasks {
		for _, id := range task.Needs {
			if needed, ok := at[id]; ok && component[needed] == component[i] {
				onCycle[waitEdge{task.ID, id}] = true
			}
		}
	}
	return onCycle
}

// randomPlan is n tasks; each is a part of an earlier task (in creation order) with probability
// pPart, of a task the plan does not hold with probability pMissing, else at the top; each needs
// each task (itself among them) with probability pNeed, and a name no task holds now and then.
func randomPlan(r *rand.Rand, n int, pPart, pMissing, pNeed float64) []Task {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("k%02d", r.IntN(100))
		for slices.Contains(ids[:i], ids[i]) {
			ids[i] = fmt.Sprintf("k%02d", r.IntN(100))
		}
	}
	tasks := make([]Task, n)
	for i, id := range ids {
		tasks[i] = Task{ID: id}
		switch x := r.Float64(); {
		case i > 0 && x < pPart:
			tasks[i].PartOf = ids[r.IntN(i)]
		case x < pPart+pMissing:
			tasks[i].PartOf = "zz"
		}
		for _, need := range ids {
			if r.Float64() < pNeed {
				tasks[i].Needs = append(tasks[i].Needs, need)
			}
		}
		if r.Float64() < 0.05 {
			tasks[i].Needs = append(tasks[i].Needs, "gone")
		}
		r.Shuffle(len(tasks[i].Needs), func(a, b int) { tasks[i].Needs[a], tasks[i].Needs[b] = tasks[i].Needs[b], tasks[i].Needs[a] })
	}
	return tasks
}

func TestNeedReasonsAnswerAsTheReleaseBefore(t *testing.T) {
	nop := func() error { return nil }
	r := rand.New(rand.NewPCG(1, 2))
	plans, refused, differ := 0, 0, 0
	for round := range 200_000 {
		n := 2 + r.IntN(11)
		pPart := []float64{0, 0.3, 0.6, 0.9}[round%4]
		pNeed := []float64{0.05, 0.15, 0.3}[round%3]
		tasks := randomPlan(r, n, pPart, 0.05, pNeed)
		plans++
		_, onCycle, err := cycles(tasks, nop)
		if err != nil {
			t.Fatal(err)
		}
		if want := oldCycleNeeds(tasks); !reflect.DeepEqual(onCycle, want) {
			t.Fatalf("plan %+v: onCycle = %v, the release before %v", tasks, onCycle, want)
		}
		got, err := newWaits(tasks).needReasons(onCycle, nop)
		if err != nil {
			t.Fatal(err)
		}
		want := newOldWaits(tasks).needReasons(oldCycleNeeds(tasks))
		if len(want) > 0 {
			refused++
		}
		if !reflect.DeepEqual(got, want) {
			differ++
			if differ <= 5 {
				t.Errorf("plan %+v:\n got  %v\n want %v", tasks, got, want)
			}
		}
	}
	t.Logf("%d plans, %d with a refused need, %d differ", plans, refused, differ)
	if differ > 0 {
		t.Fatalf("%d of %d plans differ", differ, plans)
	}
}
