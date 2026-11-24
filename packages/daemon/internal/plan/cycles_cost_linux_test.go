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
