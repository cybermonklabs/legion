//go:build linux

package plan

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readBound is the most CPU time of its own thread that FromBlocks may spend reading each plan of
// TestEachNeedOfAPlanIsReadInLinearTime. The read takes about 10 ms and 60 ms of it on those
// plans, and the read that scanned the needs a task had kept took 10.6 s and 5.58 s.
const readBound = time.Second

// threadCPU is the CPU time the calling thread has spent, its user and its system time. The caller
// locks its goroutine to that thread, so the reading counts its own work and no other test's, and
// a busy machine, which makes the thread wait, adds nothing to it.
func threadCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_THREAD, &usage); err != nil {
		t.Fatalf("read the thread's CPU time: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// FromBlocks reads each `needs` word of a task once and looks it up in a set of the ids and of the
// words the task has kept already (resolved), so the read takes time linear in the words. Each
// plan below is read within readBound of the thread's own CPU time, and its task a needs each task
// its words name, once, and keeps each word that names no task, once: 50,000 words that name no
// task, each written twice, and a task that needs each of 50,000 others. The fault is a read that
// scans what the task has kept for each word, quadratic in the words of one block; the control is
// resolved with slices.Contains over the needs and the words kept in place of its two sets.
//
// A count of steps cannot hold this read, as one holds the check of cycles. resolved makes one
// lookup for each word, by a set or by a scan, and the scan's loop runs inside slices.Contains, in
// the standard library, where no count of this package runs: a count the read kept would read the
// same with the scan as without it. So the read is held to 1 s of the thread's CPU time: in this
// pull request's runs the right read took 10 to 80 ms of it on these plans, and the control 3.9 to
// 15.8 s. It catches that scan at these sizes; it proves no bound of time in general.
func TestEachNeedOfAPlanIsReadInLinearTime(t *testing.T) {
	// needing is a plan of a task a whose `needs` writes words, then a task of each id of ids.
	needing := func(words []string, ids []string) []Block {
		blocks := []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"needs": strings.Join(words, " ")}}}
		for _, id := range ids {
			blocks = append(blocks, Block{ID: id, Type: TaskBlockType})
		}
		return blocks
	}
	for _, tc := range []struct {
		name              string
		blocks            []Block
		needs, unresolved []string
	}{
		{name: "50,000 words that name no task, each twice", blocks: needing(append(numbered("x", 50_000), numbered("x", 50_000)...), nil), unresolved: numbered("x", 50_000)},
		{name: "one task needing each of 50,000 others", blocks: needing(numbered("t", 50_000), numbered("t", 50_000)), needs: numbered("t", 50_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadCPU(t)
			p, err := FromBlocks("plan-md", planDocument, 1, tc.blocks)
			spent := threadCPU(t) - before
			if err != nil {
				t.Fatalf("FromBlocks(%s): %v", tc.name, err)
			}
			if got := p.Tasks[0]; !reflect.DeepEqual(got.Needs, tc.needs) || !reflect.DeepEqual(got.Unresolved, tc.unresolved) {
				t.Fatalf("FromBlocks(%s): a needs %d tasks and keeps %d words, want %d and %d", tc.name, len(got.Needs), len(got.Unresolved), len(tc.needs), len(tc.unresolved))
			}
			if spent > readBound {
				t.Fatalf("FromBlocks(%s) spent %v of its thread's CPU, want at most %v", tc.name, spent, readBound)
			}
			t.Logf("FromBlocks(%s) spent %v of its thread's CPU", tc.name, spent)
		})
	}
}

// parentsBound is the most CPU time of its own thread that FromBlocks may spend reading each plan
// of TestEachParentOfAPlanIsReadInLinearTime.
const parentsBound = time.Second

// A task block names the block it stands under (`parent`), and Places reads each block's place
// from its chain of parents. A plan document of 1 MiB holds a chain of about 14,000 task blocks,
// each under the one before, once Dispatch creates `parent`. Places takes time linear in the
// blocks: one walk settles each block (cycleOf), and each place is read once. Each plan below is
// read within parentsBound of the thread's own CPU time: a chain of 4,000 blocks under a task with
// no sub-owner, each an element of it; a ring of 4,000, which FromBlocks refuses naming its first
// 20 blocks and counting the rest; and a chain of 14,000. The fault is a walk from every block to
// the top that scans the chain it has walked at each step, cubic in the chain: about 1.1×10¹⁰
// comparisons at 4,000 and 4.6×10¹¹ at 14,000. The control is Places with that walk planted,
// which spends far more than parentsBound on the first plan and stops there, so the test never
// starts the chain of 14,000 under it. The bound catches that walk at these sizes; it proves no
// bound of time in general.
func TestEachParentOfAPlanIsReadInLinearTime(t *testing.T) {
	// chain is the task a, then n task blocks t0 … t<n-1>, t0 under a and each other under the one
	// before; in a ring, t0 stands under the last instead.
	chain := func(n int, ring bool) []Block {
		blocks := []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer"}}}
		for i := range n {
			above := "a"
			switch {
			case ring && i == 0:
				above = fmt.Sprintf("t%d", n-1)
			case i > 0:
				above = fmt.Sprintf("t%d", i-1)
			}
			blocks = append(blocks, withParent(Block{ID: fmt.Sprintf("t%d", i), Type: TaskBlockType}, above))
		}
		return blocks
	}
	// The ring's walk from t0, its first block in the document, goes up through t3999, t3998 and so
	// on, so the reason names t0 and the 19 blocks above it, and counts the other 3,980.
	named := []string{"t0"}
	for i := 3_999; len(named) < MostReasons; i-- {
		named = append(named, fmt.Sprintf("t%d", i))
	}
	ringReason := "task blocks " + strings.Join(named, ", ") + " and 3980 more stand under each other, so none of them has a place in the plan"
	for _, tc := range []struct {
		name     string
		blocks   []Block
		elements int
		err      string
	}{
		{name: "a chain of 4,000", blocks: chain(4_000, false), elements: 4_000},
		{name: "a ring of 4,000", blocks: chain(4_000, true), err: ringReason},
		{name: "a chain of 14,000", blocks: chain(14_000, false), elements: 14_000},
	} {
		// One plan after another, not as subtests: under the control the first plan fails the test
		// and no later one runs.
		runtime.LockOSThread()
		before := threadCPU(t)
		p, err := FromBlocks("plan-md", planDocument, 1, tc.blocks)
		spent := threadCPU(t) - before
		runtime.UnlockOSThread()
		switch {
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Fatalf("FromBlocks(%s) = %v, want the error %q", tc.name, err, tc.err)
		case tc.err == "" && err != nil:
			t.Fatalf("FromBlocks(%s): %v", tc.name, err)
		case tc.err == "" && (len(p.Tasks) != 1 || p.Tasks[0].ID != "a" || len(p.Elements) != tc.elements):
			t.Fatalf("FromBlocks(%s) holds %d tasks and %d elements, want task a alone and %d elements", tc.name, len(p.Tasks), len(p.Elements), tc.elements)
		case spent > parentsBound:
			t.Fatalf("FromBlocks(%s) spent %v of its thread's CPU, want at most %v", tc.name, spent, parentsBound)
		}
		t.Logf("FromBlocks(%s) spent %v of its thread's CPU", tc.name, spent)
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
			found := cycles(tc.tasks)
			spent := threadCPU(t) - before
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
