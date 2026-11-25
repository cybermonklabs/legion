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
// it before the fact lock, where nothing but the request's end bounds the check's cost. So the
// check of cycles takes time near-linear in the plan: one pass finds the sets of tasks that need
// each other, and one breadth-first search inside each such set that holds a cycle names it. A
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

// chainBlocks is size task blocks, each needing the one before it, the first needing none: one
// chain of needs and no cycle. With parts, block f names a sub-owner and the chain is size-1 parts
// of f, f00000, f00001, …, siblings each needing the one before, as a part may need its siblings;
// without, the chain is size tasks of the top level, t00000, t00001, …. No block names steps.
func chainBlocks(size int, parts bool) []Block {
	prefix, parent := "t", ""
	var blocks []Block
	if parts {
		prefix, parent = "f", "f"
		blocks = append(blocks, partBlock("f", "", "owner", "architect"))
		size--
	}
	for i := range size {
		id := fmt.Sprintf("%s%05d", prefix, i)
		if i == 0 {
			blocks = append(blocks, partBlock(id, parent))
			continue
		}
		blocks = append(blocks, partBlock(id, parent, "needs", fmt.Sprintf("%s%05d", prefix, i-1)))
	}
	return blocks
}

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

// Dispatch's document limits let an owner write a plan of 10,000 task blocks, and a sub-owner the
// parts of its task, and register_plan reads and checks the plan before the fact lock (FromBlocks,
// then Validate), where nothing but the request's end bounds the cost. So the read and the check
// take time near-linear in the plan, whatever its shape:
//   - a chain of 10,000 tasks of the top level, each needing the one before: a plan with no part,
//     whose needs the rules of parts search no wait for;
//   - a chain of 9,999 parts of one task, siblings each needing the one before, whose waits close
//     no cycle, so the rules search no wait for them either;
//   - two nestings of 4,999 parts under one task, each part a part of the one before, each part of
//     the second needing the part of the first at its depth: each such need but the first is a need
//     of a part of another task, named at the top of the first nesting.
//
// Each stays within cycleCheckBound of the thread's CPU time, with its refusals as written. The
// faults are a breadth-first search of the waits from every need, quadratic on a chain, and a walk
// up the whole nesting from each block or from each need, cubic on a nesting; the control is the
// release before, whose rules of parts searched the waits from every need, and whose Places walked
// each block's chain of parents to the top, comparing each block with each before it.
func TestEachShapeOfALongPlanIsCheckedInNearLinearTime(t *testing.T) {
	nesting, nested := nestingBlocks(costOfShapeSize)
	for _, tc := range []struct {
		name   string
		blocks []Block
		want   []Refusal
	}{
		{name: "a chain of tasks", blocks: chainBlocks(costOfShapeSize, false)},
		{name: "a chain of parts", blocks: chainBlocks(costOfShapeSize, true)},
		{name: "a nesting of parts", blocks: nesting, want: nested},
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
				t.Fatalf("Validate(%s) gave %d refusals, want %d", tc.name, len(refusals), len(tc.want))
			}
			if spent > cycleCheckBound {
				t.Fatalf("FromBlocks and Validate(%s) spent %v of CPU, want at most %v", tc.name, spent, cycleCheckBound)
			}
			t.Logf("FromBlocks and Validate(%s, %d blocks) spent %v of CPU", tc.name, len(tc.blocks), spent)
		})
	}
}
