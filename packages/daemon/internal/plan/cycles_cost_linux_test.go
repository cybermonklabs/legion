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

// cycleCheckBound is the most CPU time the check of a ring of 10,000 tasks may take. A check in
// time near-linear in the plan takes a few milliseconds there; one that searches again from every
// need of the ring takes over a minute.
const cycleCheckBound = time.Second

// threadCPU is the CPU time the calling thread has spent, its user and its system time. The
// caller locks its goroutine to that thread, so the reading counts its own work and no other
// test's.
func threadCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_THREAD, &usage); err != nil {
		t.Fatalf("read the thread's CPU time: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// Dispatch's document limits let an owner write a plan of 10,000 tasks, and register_plan checks
// it before the fact lock, where only the request's end and the daemon's own deadline for the check
// (api's planCheckBound) stop it, and a plan that deadline stops is refused. So the check of cycles
// takes time near-linear in the plan: one pass finds the sets of tasks that need each other, and
// one breadth-first search inside each such set that holds a cycle names it. A
// ring of 10,000 tasks, each needing the next, is one such set, and its check stays within
// cycleCheckBound of the thread's CPU time, with the ring's one cycle as its one refusal. The
// fault is a breadth-first search from every need, quadratic on a ring; the control is that
// check, the cycles of the release before, which spent over a minute of CPU on this ring.
func TestARingOfTenThousandTasksIsRefusedInNearLinearTime(t *testing.T) {
	const size = 10_000
	ring := ringOf(size)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := threadCPU(t)
	refusals, err := Validate(t.Context(), ring, Deployment{Roles: BuiltinRoles()})
	spent := threadCPU(t) - before
	if err != nil {
		t.Fatalf("Validate(a ring of %d tasks): %v", size, err)
	}
	chain := make([]string, size)
	for i := range chain {
		chain[i] = fmt.Sprintf("t%05d needs t%05d", i, (i+1)%size)
	}
	want := []Refusal{{Task: "t00000", Reason: "task t00000 needs itself: " + strings.Join(chain, ", ") + "."}}
	if !reflect.DeepEqual(refusals, want) {
		t.Fatalf("Validate(a ring of %d tasks) gave %d refusals, want the one that names the ring from t00000", size, len(refusals))
	}
	if spent > cycleCheckBound {
		t.Fatalf("Validate(a ring of %d tasks) spent %v of CPU, want at most %v", size, spent, cycleCheckBound)
	}
	t.Logf("Validate(a ring of %d tasks) spent %v of CPU", size, spent)
}

// costOfShapeSize is the count of task blocks of each shape TestEachShapeOfALongPlanIsCheckedInNearLinearTime
// reads: the most Dispatch's document limits let one plan hold.
const costOfShapeSize = 10_000

// nestingBlocks is two nestings of parts under block r, which names a sub-owner, of (size-1)/2
// blocks each: a00000 and b00000 are parts of r, and each a<i> and b<i> after them a part of the
// one before, each naming a sub-owner. Each b<i> needs a<i>, the part of the other nesting at its
// depth. Each such need but b00000's, a need of its sibling, is a need of a part of another task:
// b<i> stands outside a00000, the outermost task above a<i> that does not hold it, so want is one
// reason for each, naming a00000, in id order.
func nestingBlocks(size int) (blocks []Block, want []Refusal) {
	depth := (size - 1) / 2
	blocks = append(blocks, partBlock("r", "", "owner", "architect"))
	for _, nesting := range []string{"a", "b"} {
		for i := range depth {
			id, parent := fmt.Sprintf("%s%05d", nesting, i), "r"
			if i > 0 {
				parent = fmt.Sprintf("%s%05d", nesting, i-1)
			}
			attributes := []string{"owner", "architect"}
			if nesting == "b" {
				attributes = append(attributes, "needs", fmt.Sprintf("a%05d", i))
			}
			blocks = append(blocks, partBlock(id, parent, attributes...))
		}
	}
	for i := 1; i < depth; i++ {
		want = append(want, Refusal{Task: fmt.Sprintf("b%05d", i), Reason: fmt.Sprintf(
			"task b%05d needs a%05d, a part of task a00000: a task outside a00000 needs the whole task a00000, and no part of it.", i, i)})
	}
	return blocks, want
}

// deepSiblings is the count of siblings of the deepest part that siblingsOfADeepPart gives. Each
// sibling's refusal names the whole nesting, so the shape keeps its siblings few and gives the
// rest of its blocks to the nesting's depth, which is what the climb of causes cost.
const deepSiblings = 4

// siblingsOfADeepPart is size task blocks: a nesting of parts under block r, which names a
// sub-owner, a00000 a part of r and each a<i> after it a part of the one before, each naming a
// sub-owner; siblings s00000, s00001, … of the deepest a, parts of the a above it; and t, a task of
// the top level. The deepest a needs each sibling, each sibling needs t, and t needs r. So t is
// done only after each sibling starts (t needs r, r is done only after the nesting down to the
// sibling), and each sibling's need of t is refused with that chain, in id order. The deepest a's
// need of a sibling also waits for its own start, through t, but each task above the deepest a
// holds that sibling, so it causes nothing, and once the siblings' needs are left out it waits for
// nothing. So want is one refusal for each sibling.
func siblingsOfADeepPart(size, siblings int) (blocks []Block, want []Refusal) {
	depth := size - siblings - 2
	deepest, above := fmt.Sprintf("a%05d", depth-1), fmt.Sprintf("a%05d", depth-2)
	ids := make([]string, siblings)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%05d", i)
	}
	blocks = append(blocks, partBlock("r", "", "owner", "architect"))
	for i := range depth {
		id, parent := fmt.Sprintf("a%05d", i), "r"
		if i > 0 {
			parent = fmt.Sprintf("a%05d", i-1)
		}
		attributes := []string{"owner", "architect"}
		if id == deepest {
			attributes = append(attributes, "needs", strings.Join(ids, " "))
		}
		blocks = append(blocks, partBlock(id, parent, attributes...))
	}
	for _, id := range ids {
		blocks = append(blocks, partBlock(id, above, "needs", "t"))
	}
	blocks = append(blocks, partBlock("t", "", "needs", "r"))
	chain := []string{"t needs r", "r is done only after its part a00000"}
	for i := 1; i < depth-1; i++ {
		chain = append(chain, fmt.Sprintf("a%05d is done only after its part a%05d", i-1, i))
	}
	for _, id := range ids {
		want = append(want, Refusal{Task: id, Reason: fmt.Sprintf("task %s needs t, which is done only after %s starts: %s and %s is done only after its part %s.",
			id, id, strings.Join(chain, ", "), above, id)})
	}
	return blocks, want
}

// Dispatch's document limits let an owner write a plan of 10,000 task blocks, and a sub-owner the
// parts of its task, and register_plan reads and checks the plan before the fact lock (FromBlocks,
// then Validate), where only the request's end and the daemon's own deadline for the check (api's
// planCheckBound) stop it, and a plan that deadline stops is refused. So the read and the check
// take time near-linear in the plan in each of these shapes:
//   - a chain of 10,000 tasks of the top level, each needing the one before: a plan with no part,
//     whose needs the rules of parts search no wait for;
//   - a chain of 9,999 parts of one task, siblings each needing the one before, whose waits close
//     no cycle, so the rules search no wait for them either;
//   - two nestings of 4,999 parts under one task, each part a part of the one before, each part of
//     the second needing the part of the first at its depth: each such need but the first is a need
//     of a part of another task, named at the top of the first nesting;
//   - a nesting of 9,994 parts under one task, the deepest needing each of its 4 siblings, each
//     sibling needing a task of the top level that needs the nesting's task
//     (siblingsOfADeepPart): each sibling's need is refused with the whole nesting, and each need
//     of the deepest part waits for its start, but the task it is a part of holds the sibling it
//     needs, so the rules search no wait above that task.
//
// Each stays within cycleCheckBound of the thread's CPU time, with its refusals as written. The
// faults are a breadth-first search of the waits from every need, quadratic on a chain; a walk up
// the whole nesting from each block or from each need, cubic on a nesting; and a search of the
// waits from a need's end to the end of each task above its task in turn, which tested whether that
// task holds the need only after its search, quadratic in the depth for each need of the deepest
// part, about 2×10^8 moments for the last shape. The control is the release before, whose rules of
// parts searched the waits from every need, and whose Places walked each block's chain of parents
// to the top, comparing each block with each before it; and, for the last shape, causes in
// waits.go as #5169's review read it, which climbed so.
//
// Not every shape is near-linear: a need whose end waits for its task's start searches its
// component once in each pass, so a plan of many such needs in one large component costs their
// count times its size. The daemon's deadline bounds that cost.
func TestEachShapeOfALongPlanIsCheckedInNearLinearTime(t *testing.T) {
	nesting, nested := nestingBlocks(costOfShapeSize)
	deep, deepWant := siblingsOfADeepPart(costOfShapeSize, deepSiblings)
	for _, tc := range []struct {
		name   string
		blocks []Block
		want   []Refusal
	}{
		{name: "a chain of tasks", blocks: chainBlocks(costOfShapeSize, false)},
		{name: "a chain of parts", blocks: chainBlocks(costOfShapeSize, true)},
		{name: "a nesting of parts", blocks: nesting, want: nested},
		{name: "the siblings of a deep part", blocks: deep, want: deepWant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadCPU(t)
			p, err := FromBlocks("ACME-1", "plan-md", 1, tc.blocks)
			if err != nil {
				t.Fatalf("FromBlocks(%s, %d blocks): %v", tc.name, len(tc.blocks), err)
			}
			refusals, err := Validate(t.Context(), p, Deployment{Roles: BuiltinRoles()})
			spent := threadCPU(t) - before
			if err != nil {
				t.Fatalf("Validate(%s): %v", tc.name, err)
			}
			if len(p.Tasks) != len(tc.blocks) {
				t.Fatalf("FromBlocks(%s) read %d tasks of %d blocks, want each block a task", tc.name, len(p.Tasks), len(tc.blocks))
			}
			if !reflect.DeepEqual(refusals, tc.want) {
				t.Fatalf("Validate(%s) gave %s", tc.name, firstDifference(refusals, tc.want))
			}
			if spent > cycleCheckBound {
				t.Fatalf("FromBlocks and Validate(%s) spent %v of CPU, want at most %v", tc.name, spent, cycleCheckBound)
			}
			t.Logf("FromBlocks and Validate(%s, %d blocks) spent %v of CPU", tc.name, len(tc.blocks), spent)
		})
	}
}

// firstDifference says where got first differs from want, for a failure whose reasons name a whole
// nesting and are too long to print: the first refusal that differs, by its index and task, and the
// text of each reason around the first byte where the two differ.
func firstDifference(got, want []Refusal) string {
	for i := range min(len(got), len(want)) {
		if got[i] == want[i] {
			continue
		}
		if got[i].Task != want[i].Task {
			return fmt.Sprintf("refusal %d of task %q, want one of task %q", i, got[i].Task, want[i].Task)
		}
		at := 0
		for at < len(got[i].Reason) && at < len(want[i].Reason) && got[i].Reason[at] == want[i].Reason[at] {
			at++
		}
		around := func(reason string) string { return reason[max(0, at-80):min(len(reason), at+80)] }
		return fmt.Sprintf("refusal %d of task %s, whose reason differs at byte %d: %q, want %q", i, got[i].Task, at, around(got[i].Reason), around(want[i].Reason))
	}
	return fmt.Sprintf("%d refusals, want %d", len(got), len(want))
}
