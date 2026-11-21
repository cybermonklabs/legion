//go:build linux

package poolsettingtest

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// childEnv names, in a child SameOnEveryMachine starts, the test that child runs.
const childEnv = "POOLSETTINGTEST_CHILD"

// childLine is what a child prints.
var childLine = regexp.MustCompile(`numcpu=(\d+) gomaxprocs=(\d+) maxconns=(\d+)`)

// SameOnEveryMachine checks that the pool a store opens holds want connections whatever machine it
// runs on: pgxpool's default, max(4, runtime.NumCPU()), made it the node's CPU count. self is this
// test binary, os.Args[0], which the caller names so that .github/scripts/testcontainers_links.py
// counts its test binary among those that start themselves. open opens the store and answers its
// pool's MaxConns, closing the store. The calling test calls it as its whole body.
//
// It starts self again twice, running the calling test: on one CPU of the set this process may run
// on, with GOMAXPROCS 1, and on that whole set, with GOMAXPROCS 64. runtime.NumCPU is read from
// the process's CPU affinity when it starts, so the first child is started from a thread whose
// affinity is that one CPU, which the child inherits. Each child prints its NumCPU, GOMAXPROCS and
// the MaxConns open answers, which must be want both times. Where this process may run on more
// than one CPU, the all-CPU child must see them all, which shows the two children saw two
// machines; on one CPU there is no second machine to compare, and the test says so.
func SameOnEveryMachine(t *testing.T, self string, want int32, open func(t *testing.T) int32) {
	t.Helper()
	if os.Getenv(childEnv) == t.Name() {
		fmt.Printf("numcpu=%d gomaxprocs=%d maxconns=%d\n", runtime.NumCPU(), runtime.GOMAXPROCS(0), open(t))
		return
	}
	var allowed unix.CPUSet
	if err := unix.SchedGetaffinity(0, &allowed); err != nil {
		t.Fatalf("read this process's CPU affinity: %v", err)
	}
	first := -1
	for cpu := range len(allowed) * 64 {
		if allowed.IsSet(cpu) {
			first = cpu
			break
		}
	}
	if first < 0 {
		t.Fatalf("this process's CPU affinity names no CPU")
	}
	oneCPU, one, oneMax := child(t, self, first, 1)
	allCPUs, _, allMax := child(t, self, -1, 64)
	if oneCPU != 1 || one != 1 {
		t.Fatalf("the one-CPU child saw NumCPU %d and GOMAXPROCS %d, want 1 and 1: the affinity did not reach it", oneCPU, one)
	}
	if oneMax != int(want) || allMax != int(want) {
		t.Fatalf("Open's pool holds %d connections on 1 CPU and %d on %d CPUs, want %d on both", oneMax, allMax, allCPUs, want)
	}
	if count := allowed.Count(); count > 1 && allCPUs != count {
		t.Fatalf("the all-CPU child saw NumCPU %d, want the %d CPUs this process may run on", allCPUs, count)
	} else if count == 1 {
		t.Logf("this process may run on one CPU, so the two children saw the same machine; both pools hold %d", want)
	}
}

// child runs t's test in self again, on CPU cpu alone when cpu is not -1, with GOMAXPROCS
// gomaxprocs, and answers the NumCPU, GOMAXPROCS and MaxConns it printed.
func child(t *testing.T, self string, cpu, gomaxprocs int) (numCPU, procs, maxConns int) {
	t.Helper()
	run := exec.Command(self, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	run.Env = append(os.Environ(), childEnv+"="+t.Name(), "GOMAXPROCS="+strconv.Itoa(gomaxprocs))
	var out []byte
	var err error
	func() {
		// The child inherits the affinity of the thread that starts it, so the start runs on a
		// locked thread whose affinity is set for it alone, and that thread is restored before it
		// is unlocked.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if cpu >= 0 {
			var was, one unix.CPUSet
			if err := unix.SchedGetaffinity(0, &was); err != nil {
				t.Fatalf("read this thread's affinity: %v", err)
			}
			one.Set(cpu)
			if err := unix.SchedSetaffinity(0, &one); err != nil {
				t.Fatalf("set this thread's affinity to CPU %d: %v", cpu, err)
			}
			defer func() {
				if err := unix.SchedSetaffinity(0, &was); err != nil {
					t.Errorf("restore this thread's affinity: %v", err)
				}
			}()
		}
		out, err = run.CombinedOutput()
	}()
	if err != nil {
		t.Fatalf("the child: %v\n%s", err, out)
	}
	found := childLine.FindSubmatch(out)
	if found == nil {
		t.Fatalf("the child printed no pool size:\n%s", out)
	}
	numCPU, _ = strconv.Atoi(string(found[1]))
	procs, _ = strconv.Atoi(string(found[2]))
	maxConns, _ = strconv.Atoi(string(found[3]))
	return numCPU, procs, maxConns
}
