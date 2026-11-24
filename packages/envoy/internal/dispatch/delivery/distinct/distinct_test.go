package distinct

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// references is the 10,943 distinct bare references (#10 to #10952) a 64 KiB pull request body of
// references carries.
func references() []string {
	values := make([]string, 0, 10943)
	for n := 10; len(values) < 10943; n++ {
		values = append(values, "#"+strconv.Itoa(n))
	}
	return values
}

// TestAddingEachValueThreeTimesKeepsItOnce adds each of the 10,943 references three times (twice
// in one call, then all of them in one call) and holds the list to the references, each once, in
// the order first added, and the set to one entry for each. It is also the workload whose steps
// TestAddingCostsAConstantNumberOfStepsAValue counts, so it uses nothing of the slices package.
func TestAddingEachValueThreeTimesKeepsItOnce(t *testing.T) {
	values := references()
	kept := New()
	for _, value := range values {
		kept.Add(value, value)
	}
	kept.Add(values...)
	list := kept.List()
	if len(list) != len(values) || len(kept.seen) != len(values) {
		t.Fatalf("adding %d distinct values three times each kept %d in the list and %d in the set, want %d in each", len(values), len(list), len(kept.seen), len(values))
	}
	for i, value := range values {
		if list[i] != value {
			t.Fatalf("the list holds %q at %d, want %q: the order first added", list[i], i, value)
		}
		if _, held := kept.seen[value]; !held {
			t.Fatalf("the set does not hold %q, which the list does", value)
		}
	}
	if New().List() == nil {
		t.Errorf("an empty Strings lists nil, want an empty list: delivery stores a nil input as one it never read")
	}
}

// stepsAValue is the most steps adding one value may take: the statement blocks of this package
// and of the slices package that run, counted by coverage. Add runs its loop's block and one of
// the two blocks after its lookup for each value, and one more block for each call.
const stepsAValue = 4

// childMargin is how long before the test binary's deadline the counting child must have ended:
// the time the row needs to read its profile and fail by name before Go's alarm panics the binary.
const childMargin = 30 * time.Second

// TestAddingCostsAConstantNumberOfStepsAValue counts the work of adding values rather than timing
// it. It runs TestAddingEachValueThreeTimesKeepsItOnce alone in a child `go test` built with
// coverage of this package and of slices (-coverpkg, -covermode=atomic), and reads the child's
// profile: each statement block of every file of this package and of slices/*.go, with how many
// times it ran. Adding 32,829 values may run at most stepsAValue blocks for each. A scan of the
// list for each value, in any file of this package or through slices, runs a block of its loop once
// for each value already held, about 179 million more blocks for this workload, and fails here by
// name. The row needs the go command, and the child must end childMargin before the test binary's
// deadline (it gets the rest as its own -timeout); a child that cannot build, does not end in time,
// or writes a profile without the blocks the row reads is a failure, never a pass.
func TestAddingCostsAConstantNumberOfStepsAValue(t *testing.T) {
	const added = 3 * 10943
	self := reflect.TypeFor[Strings]().PkgPath()
	profile := filepath.Join(t.TempDir(), "cover.out")
	ctx, timeout := context.Background(), "0"
	if deadline, ok := t.Deadline(); ok {
		end := deadline.Add(-childMargin)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, end)
		defer cancel()
		timeout = max(time.Until(end), time.Millisecond).String()
	}
	child := exec.CommandContext(ctx, "go", "test", "-count=1", "-timeout="+timeout, "-run", "^TestAddingEachValueThreeTimesKeepsItOnce$",
		"-coverpkg=slices,"+self, "-covermode=atomic", "-coverprofile="+profile, ".")
	out, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("UNKNOWN: the counting child go test did not end %s before the test binary's deadline, so nothing was counted: %v\n%s", childMargin, ctx.Err(), out)
	}
	if err != nil {
		t.Fatalf("UNKNOWN: the counting child go test failed, so nothing was counted: %v\n%s", err, out)
	}
	file, err := os.Open(profile)
	if err != nil {
		t.Fatalf("UNKNOWN: the child wrote no profile, so nothing was counted: %v", err)
	}
	defer file.Close()
	var ownBlocks, slicesBlocks, own, inSlices int
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		// file:start,end statements count
		fields := strings.Fields(lines.Text())
		if len(fields) != 3 {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(fields[0], self+"/"):
			ownBlocks++
			own += count
		case strings.HasPrefix(fields[0], "slices/"):
			slicesBlocks++
			inSlices += count
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("UNKNOWN: reading the child's profile: %v", err)
	}
	t.Logf("adding %d values ran %d blocks of this package (%d blocks in the profile) and %d of slices (%d blocks in the profile)", added, own, ownBlocks, inSlices, slicesBlocks)
	if ownBlocks == 0 || slicesBlocks == 0 || own == 0 {
		t.Fatalf("UNKNOWN: the child's profile holds %d blocks of this package, which ran %d times, and %d blocks of slices: a count that saw no block measured nothing", ownBlocks, own, slicesBlocks)
	}
	if most := stepsAValue * added; own+inSlices > most {
		t.Errorf("adding %d values ran %d blocks of this package and %d of slices, want at most %d (%d a value): adding a value scans the values already held", added, own, inSlices, most, stepsAValue)
	}
}
