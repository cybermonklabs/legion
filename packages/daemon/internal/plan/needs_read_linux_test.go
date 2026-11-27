//go:build linux

package plan

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readBound is the most CPU time of its own thread that FromBlocks may spend reading each plan of
// TestEachNeedOfAPlanIsReadInLinearTime.
const readBound = time.Second

// FromBlocks reads each `needs` word of a task once and looks it up in a set of what each of the
// task's four lists has kept already (resolved), so the read takes time linear in the words. Each
// plan below is read within readBound of the thread's own CPU time, and its task a needs each task
// its words name, once, and keeps each word that names no task, each word into another issue and
// each word that names an element, once: 50,000 words that name no task, each written twice; a
// task that needs each of 50,000 others; and 50,000 words into another issue, each written twice.
// The fault is a read that scans what the task has kept for each word, quadratic in the words of
// one block; the control is resolved with slices.Contains over a kept list in place of its set,
// under which the plan of that list's words fails its bound.
//
// A count of steps cannot hold this read: the scan's loop runs inside slices.Contains, in the
// standard library, where no count of this package runs. So the read is held to 1 s of the
// thread's CPU time. It catches that scan at these sizes; it proves no bound of time in general.
func TestEachNeedOfAPlanIsReadInLinearTime(t *testing.T) {
	// needing is a plan of a task a whose `needs` writes words, then a task of each id of ids.
	needing := func(words []string, ids []string) []Block {
		blocks := []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"needs": strings.Join(words, " ")}}}
		for _, id := range ids {
			blocks = append(blocks, Block{ID: id, Type: TaskBlockType})
		}
		return blocks
	}
	outside := numbered("dispatch://ACME-2/artifact/plan-md#b-o", 50_000)
	for _, tc := range []struct {
		name                       string
		blocks                     []Block
		needs, unresolved, outside []string
	}{
		{name: "50,000 words that name no task, each twice", blocks: needing(append(numbered("x", 50_000), numbered("x", 50_000)...), nil), unresolved: numbered("x", 50_000)},
		{name: "one task needing each of 50,000 others", blocks: needing(numbered("t", 50_000), numbered("t", 50_000)), needs: numbered("t", 50_000)},
		{name: "50,000 words into another issue, each twice", blocks: needing(append(outside, outside...), nil), outside: outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadCPU(t)
			p, err := FromBlocks("ACME-1", "plan-md", 1, tc.blocks)
			spent := threadCPU(t) - before
			if err != nil {
				t.Fatalf("FromBlocks(%s): %v", tc.name, err)
			}
			got := p.Tasks[0]
			if !reflect.DeepEqual(got.Needs, tc.needs) || !reflect.DeepEqual(got.Unresolved, tc.unresolved) || !reflect.DeepEqual(got.OutsideIssue, tc.outside) {
				t.Fatalf("FromBlocks(%s): a needs %d tasks and keeps %d words and %d outside, want %d, %d and %d", tc.name,
					len(got.Needs), len(got.Unresolved), len(got.OutsideIssue), len(tc.needs), len(tc.unresolved), len(tc.outside))
			}
			if spent > readBound {
				t.Fatalf("FromBlocks(%s) spent %v of its thread's CPU, want at most %v", tc.name, spent, readBound)
			}
			t.Logf("FromBlocks(%s) spent %v of its thread's CPU", tc.name, spent)
		})
	}
}

// cyclesBound is the most CPU time of its own thread that cycles may spend on each plan of
// TestTheCheckOfCyclesTakesTimeLinearInThePlan.
const cyclesBound = time.Second

// Dispatch takes an owner's plan of 10,000 tasks in a ring, and register_plan reads one of 40,000
// (api's TestRegisterPlanAnswersAPlanOfManyCausesWithinABound). A search from each task of a set
// of tasks that need each other, which named the shortest cycle through each one, follows each
// need of the set once for each of its tasks, quadratic on a ring. So the check of cycles takes
// time linear in the plan: one pass finds the sets (components), and one breadth-first search
// from the first task of each set that holds a cycle names it (cycleFrom), so a task joins at most
// one search. Each plan below gives its cycles, the ring's one cycle through every task, the
// complete graph's shortest cycle from its first task, and one cycle for each task that needs
// itself, within cyclesBound of the thread's own CPU time. The fault is a search from every task
// of a set; the control is cycles with that search planted, under which the ring of 40,000 tasks
// follows each of its needs 40,000 times. Only the ring holds the fault: on the complete graph
// and on the tasks that need themselves each search ends in its first round, so the planted search
// costs little there. The bound catches that search at this size; it proves no bound of time in
// general.
func TestTheCheckOfCyclesTakesTimeLinearInThePlan(t *testing.T) {
	ring := make([]Task, 40_000)
	ringCycle := make([]string, len(ring))
	for i := range ring {
		ring[i] = Task{ID: fmt.Sprintf("t%05d", i), Needs: []string{fmt.Sprintf("t%05d", (i+1)%len(ring))}}
		ringCycle[i] = ring[i].ID
	}
	complete := make([]Task, 300)
	for i := range complete {
		complete[i].ID = fmt.Sprintf("t%03d", i)
		for j := range complete {
			if j != i {
				complete[i].Needs = append(complete[i].Needs, fmt.Sprintf("t%03d", j))
			}
		}
	}
	itself := make([]Task, 10_000)
	itselfCycles := make([][]string, len(itself))
	for i := range itself {
		itself[i] = Task{ID: fmt.Sprintf("t%05d", i), Needs: []string{fmt.Sprintf("t%05d", i)}}
		itselfCycles[i] = []string{itself[i].ID}
	}
	for _, tc := range []struct {
		name  string
		tasks []Task
		want  [][]string
	}{
		{"a ring of 40,000 tasks", ring, [][]string{ringCycle}},
		{"300 tasks each needing all the others", complete, [][]string{{"t000", "t001"}}},
		{"10,000 tasks each needing itself", itself, itselfCycles},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadCPU(t)
			found, _, err := cycles(tc.tasks, stepsOf(t.Context()))
			spent := threadCPU(t) - before
			if err != nil {
				t.Fatalf("cycles(%s): %v", tc.name, err)
			}
			if !reflect.DeepEqual(found, tc.want) {
				t.Fatalf("cycles(%s) found %d cycles, want %d: %s", tc.name, len(found), len(tc.want), firstOther(found, tc.want))
			}
			if spent > cyclesBound {
				t.Fatalf("cycles(%s) spent %v of its thread's CPU, want at most %v", tc.name, spent, cyclesBound)
			}
			t.Logf("cycles(%s) spent %v of its thread's CPU", tc.name, spent)
		})
	}
}

// firstOther names the first cycle found that differs from the one wanted at its place, each by
// its first five tasks and its length, so a failure of a 40,000-task ring stays readable.
func firstOther(found, want [][]string) string {
	short := func(cycle []string) string {
		return fmt.Sprintf("%v (%d tasks)", cycle[:min(len(cycle), 5)], len(cycle))
	}
	for i := range max(len(found), len(want)) {
		switch {
		case i >= len(found):
			return fmt.Sprintf("cycle %d is missing, want %s", i, short(want[i]))
		case i >= len(want):
			return fmt.Sprintf("cycle %d is %s, want none", i, short(found[i]))
		case !reflect.DeepEqual(found[i], want[i]):
			return fmt.Sprintf("cycle %d is %s, want %s", i, short(found[i]), short(want[i]))
		}
	}
	return "every cycle as wanted"
}
