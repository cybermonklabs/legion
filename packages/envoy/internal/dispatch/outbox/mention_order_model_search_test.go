package outbox

import (
	"fmt"
	"slices"
	"strings"
)

// The order model's search (TestTheMentionCopyOrderModel, mention_order_model_test.go holds the
// model itself): the depth-first walk of every state a scenario reaches, the counterexample it
// names and carries on to s1 taking the event the copy passed, and the fair runs of the liveness
// check.

type moCounterexample struct {
	scenario moScenario
	// comment is the comment's event id.
	comment int
	trace   []moStep
	// after is the outbox carrying on from the copy, every send succeeding and no write made,
	// until s1 takes an event the copy passed.
	after    []moStep
	inWindow bool
}

// took is the events s1 took along the run, each once, in the order it first took them: a session
// drops a second copy of an event by its event id.
func (c moCounterexample) took() []int {
	var took []int
	seen := map[int]bool{}
	for _, step := range append(slices.Clone(c.trace), c.after...) {
		for _, id := range step.took {
			if !seen[id] {
				seen[id] = true
				took = append(took, id)
			}
		}
	}
	return took
}

// observed is whether s1, along the run, takes an event earlier than the comment after it.
func (c moCounterexample) observed() bool {
	took := c.took()
	at := slices.Index(took, c.comment)
	return at >= 0 && slices.ContainsFunc(took[at+1:], func(id int) bool { return id < c.comment })
}

func (c moCounterexample) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario: %s\n", c.scenario)
	steps := append(slices.Clone(c.trace), c.after...)
	for i, step := range steps {
		if i == len(c.trace) {
			b.WriteString("  then, every send succeeding:\n")
		}
		fmt.Fprintf(&b, "  %2d. %s", i+1, step.label)
		if len(step.took) > 0 {
			fmt.Fprintf(&b, " - s1 takes %v", step.took)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  s1 took events %v", c.took())
	return b.String()
}

type moResult struct {
	states, violations, inWindow int
	first                        *moCounterexample
	liveFailures                 int
	liveFirst                    string
	maxSteps                     int
}

// explore walks every state the scenario reaches, depth first.
func (m *moModel) explore() moResult {
	var result moResult
	start := m.start()
	visited := map[moState]struct{}{start: {}}
	type frame struct {
		states []moState
		next   int
	}
	children := func(s moState) []moState {
		var states []moState
		m.successors(s, func(_ moStep, next moState) { states = append(states, next) })
		return states
	}
	stack := []frame{{states: children(start)}}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		if f.next == len(f.states) {
			stack = stack[:len(stack)-1]
			continue
		}
		state := f.states[f.next]
		f.next++
		if _, ok := visited[state]; ok {
			continue
		}
		visited[state] = struct{}{}
		if state.violated {
			result.violations++
			if state.inWindow {
				result.inWindow++
			}
			if result.first == nil {
				path := []moState{start}
				for _, f := range stack {
					path = append(path, f.states[f.next-1])
				}
				result.first = m.counterexample(path)
			}
			continue
		}
		if state.copied {
			continue
		}
		stack = append(stack, frame{states: children(state)})
	}
	result.states = len(visited)
	for _, viaFallback := range []bool{false, true} {
		memo := map[moState]int{}
		for state := range visited {
			if state.copied {
				continue
			}
			steps, why := m.fair(state, viaFallback, memo)
			if steps < 0 {
				result.liveFailures++
				if result.liveFirst == "" {
					road := "the API's send"
					if viaFallback {
						road = "the fallback row"
					}
					result.liveFirst = fmt.Sprintf("scenario: %s; by %s: %s", m.sc, road, why)
				}
			} else if steps > result.maxSteps {
				result.maxSteps = steps
			}
		}
	}
	return result
}

// counterexample names each step of path, a run that ends in a copy passing an earlier event, and
// carries it on to s1 taking that event.
func (m *moModel) counterexample(path []moState) *moCounterexample {
	verbose := *m
	verbose.verbose = true
	c := &moCounterexample{scenario: m.sc, comment: int(path[len(path)-1].c) + 1, inWindow: path[len(path)-1].inWindow}
	for i := 1; i < len(path); i++ {
		var named moStep
		verbose.successors(path[i-1], func(step moStep, next moState) {
			if next == path[i] && named.label == "" {
				named = step
			}
		})
		c.trace = append(c.trace, named)
	}
	s := path[len(path)-1]
	passed := m.passed(&path[len(path)-2])
	for range 20 {
		var step moStep
		ok := false
		for p := range m.env.processes {
			if !ok {
				step, s, ok = verbose.record(s, p)
			}
		}
		if !ok && s.anyDue(true) && !s.pend[s.hold].active {
			choice := make([]int, len(planChains(verbose.batchEvents(&s))))
			for i := range choice {
				choice[i] = -1
			}
			step, s = verbose.applyBatch(s, int(s.hold), choice, s.fails)
			ok = true
		}
		if !ok {
			step, s, ok = verbose.tick(s)
		}
		if !ok {
			break
		}
		c.after = append(c.after, step)
		if slices.ContainsFunc(step.took, func(id int) bool { return passed&(1<<(id-1)) != 0 }) {
			break
		}
	}
	return c
}

// fairStep is one step of a fair run: the deterministic schedule the liveness check follows.
func (m *moModel) fairStep(s moState, viaFallback bool) (moState, string, bool) {
	if viaFallback {
		if _, next, ok := m.expire(s); ok {
			return next, "", true
		}
	}
	for p := range m.env.processes {
		if _, next, ok := m.record(s, p); ok {
			return next, "", true
		}
	}
	if _, next, ok := m.client(s); ok {
		return next, "", true
	}
	if _, next, ok := m.copyRoad(s, viaFallback && s.row); ok {
		return next, "", true
	}
	if s.hold != 0 {
		// The deploy ends, or the hold settles: process A, of this build, holds it.
		next := s
		next.hold = 0
		return next, "", true
	}
	if s.anyDue(true) {
		choice := make([]int, len(planChains(m.batchEvents(&s))))
		for i := range choice {
			choice[i] = -1
		}
		_, next := m.applyBatch(s, 0, choice, s.fails)
		return next, "", true
	}
	if _, next, ok := m.tick(s); ok {
		return next, "", true
	}
	return s, "no step is left and the copy has not gone", false
}

// fair runs the fair schedule from s and answers how many steps the copy took to go, or -1 and
// why when it never goes. memo holds the answer of every state a run passed.
func (m *moModel) fair(s moState, viaFallback bool, memo map[moState]int) (int, string) {
	var trail []moState
	seen := map[moState]struct{}{}
	answer, why := -1, ""
	for {
		if v, ok := memo[s]; ok {
			answer = v
			if v < 0 {
				why = "it reaches a state whose copy never goes"
			}
			break
		}
		if s.copied {
			answer = 0
			break
		}
		if _, ok := seen[s]; ok {
			why = fmt.Sprintf("the run comes back to a state it left, the copy held by event %d", m.v.holder(&s)+1)
			break
		}
		seen[s] = struct{}{}
		trail = append(trail, s)
		next, stuck, ok := m.fairStep(s, viaFallback)
		if !ok {
			why = stuck
			break
		}
		s = next
	}
	for i := len(trail) - 1; i >= 0; i-- {
		if answer >= 0 {
			answer++
		}
		memo[trail[i]] = answer
	}
	return answer, why
}
