package codesurvival

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// refusals is k8's branch of 31 days, a change a day, and its three commits: first, day 10's
// change, and second, day 12's, whose patches the tests refuse and serve whole on files[]; and
// third, a commit of day 3 that merged no pull request and changes 12 files, which a test refuses
// with 3 of its files withheld on files[].
type refusals struct {
	branch               *codesurvivaltest.Branch
	first, second, third string
}

var refusalsOnce struct {
	once sync.Once
	set  *refusals
}

// refusalBranch is refusals, built on the first call.
func refusalBranch(t *testing.T) *refusals {
	t.Helper()
	refusalsOnce.once.Do(func() {
		w := newWriter(t)
		set := &refusals{}
		w.days(0, 31, func(int) int { return 1 }, func(n int) {
			if n != 3 {
				return
			}
			at := day(n).Add(6 * time.Hour)
			for file := range 12 {
				w.add(fmt.Sprintf("data/table%02d.txt", file), 2, at, "data")
			}
			set.third = w.h.Push("twelve tables", at)
		})
		set.first, set.second = w.tips[10], w.tips[12]
		set.branch = w.h.Branch()
		refusalsOnce.set = set
	})
	if refusalsOnce.set == nil {
		t.Fatal("the refusals' branch was not built: see the test that built it")
	}
	return refusalsOnce.set
}

// requireSameWindows fails t unless every 7-day window of got's measured days from day from reads
// the numbers want's reads, and at least one reads a number.
func requireSameWindows(t *testing.T, got, want *rig, from, to int) {
	t.Helper()
	numbers := 0
	for start := from; start+7 <= to; start++ {
		for _, group := range []string{"agents", "others"} {
			a, b := got.read(widgetsProject, group, day(start), day(start+7)), want.read(widgetsProject, group, day(start), day(start+7))
			if value(a.survival.Share) != value(b.survival.Share) || value(a.rework.Share) != value(b.rework.Share) ||
				value(a.survival.Added) != value(b.survival.Added) {
				t.Errorf("the %s window from day %d reads survival %s, rework %s over %s lines, want %s, %s over %s as the diff route reads them", group, start,
					value(a.survival.Share), value(a.rework.Share), value(a.survival.Added), value(b.survival.Share), value(b.rework.Share), value(b.survival.Added))
			}
			if a.survival.Share != nil {
				numbers++
			}
		}
	}
	if numbers == 0 {
		t.Fatal("no window read a number")
	}
}

// filesReads is how many pages of sha's files[] were read among requests.
func filesReads(requests []codesurvivaltest.Request, sha string) int {
	pages := 0
	for _, request := range requests {
		if read, ok := request.Files(); ok && read == sha {
			pages++
		}
	}
	return pages
}

// A patch the diff route refuses is read from the commit's files[] and measured as the diff route
// reads it (k8, R2): the double answers 406 for day 10's change and serves every file's patch on
// files[]. The run reads the patch once and files[] once, day 10 is measured, and every window
// reads what a run whose diff route answered reads.
func TestARefusalIsRecoveredFromFiles(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.first, codesurvivaltest.Refusal{Status: 406, Body: "Sorry, the diff exceeded the maximum number of files"})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[set.first], filesReads(requests, set.first); reads != 1 || pages != 1 {
		t.Fatalf("the refused patch was read %d times and its files[] %d times, want once each", reads, pages)
	}
	if state := r.days(widgets)[day(10)]; state != survival.DayMeasured {
		t.Fatalf("day 10 is %q, want measured", state)
	}
	requireSameWindows(t, r, whole, 4, 28)
}

// An answer past 32 MiB is read from the commit's files[] in its place (k8, R2): the double
// answers 40 MiB for day 12's change on the diff route; the run refuses the rest, reads files[]
// once, and every window reads what a run whose diff route answered reads.
func TestAnOversizeAnswerIsRecoveredFromFiles(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.second, codesurvivaltest.Refusal{Oversize: true})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[set.second], filesReads(requests, set.second); reads != 1 || pages != 1 {
		t.Fatalf("the oversize patch was read %d times and its files[] %d times, want once each", reads, pages)
	}
	if state := r.days(widgets)[day(12)]; state != survival.DayMeasured {
		t.Fatalf("day 12 is %q, want measured", state)
	}
	requireSameWindows(t, r, whole, 4, 28)
	lines := r.logs.lines(t, "codesurvival: run")
	if last := lines[len(lines)-1]; last["patches_files"] != float64(1) || last["bytes"].(float64) < 32<<20 {
		t.Errorf("the run logged %v, want one files[] page and the 32 MiB it read of the oversize answer", last)
	}
}

// A file whose patch neither route gives blocks the days whose 30 days hold its commit (k8, R9, the
// day-state table's row 9): day 3's commit, which merged no pull request, refuses on the diff route
// and withholds 3 of its 12 files on files[], so days 0 to 3 are blocked by it, each naming the
// commit, the count 3 and why; every window that holds one reads "no data", never 0, with the
// count; days 4 to 27 read what a run whose diff route answered reads. Once the double serves the
// commit whole, the pass a week and a day later, which has fewer than seven days waiting, tries
// the blocked days again and measures them: no row of the five tables holds the commit's SHA, and
// those windows read the whole run's numbers.
func TestAWithheldFileBlocksTheDaysWhoseThirtyDaysHoldIt(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	whole.passAt(weekLater)
	whole.passAt(weekLater.Add(24 * time.Hour))
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.third, codesurvivaltest.Refusal{Status: 422, Body: "diff too large", Withheld: 3})
	r.passAt(firstPass)

	states := r.days(widgets)
	for n := range 28 {
		want := survival.DayMeasured
		if n <= 3 {
			want = survival.DayBlocked
		}
		if states[day(n)] != want {
			t.Errorf("day %d is %q, want %q", n, states[day(n)], want)
		}
	}
	for n := range 4 {
		var sha, reason string
		var files int
		if err := r.database.Pool.QueryRow(r.ctx, `select blocked_sha, blocked_files, blocked_reason from code_days where repo = $1 and day = $2`, widgets, day(n)).
			Scan(&sha, &files, &reason); err != nil || sha != set.third || files != 3 || reason != "diff refused (422), files[] withheld 3 of 12 files" {
			t.Errorf("blocked day %d holds %q, %d files and %q (%v), want the third commit, 3 and the refusal", n, sha, files, reason, err)
		}
		for _, row := range r.codeRows(widgetsProject, day(n)) {
			if !strings.Contains(row, "|status|") || !strings.Contains(row, `"blocked": 1`) || !strings.Contains(row, `"blocked_files": 3`) {
				t.Errorf("blocked day %d holds the row %q, want a status row with blocked 1 and 3 files", n, row)
			}
		}
	}
	for start := 0; start <= 3; start++ {
		read := r.read(widgetsProject, "agents", day(start), day(start+7))
		blocked := 4 - start
		if read.survival.Share != nil || read.rework.Share != nil || read.survival.Added != nil ||
			read.status.BlockedDays != blocked || read.status.BlockedFiles != 3*blocked {
			t.Errorf("the window from day %d reads survival %s, rework %s, %d blocked days and %d files, want no data, %d days and %d files",
				start, value(read.survival.Share), value(read.rework.Share), read.status.BlockedDays, read.status.BlockedFiles, blocked, 3*blocked)
		}
	}
	requireSameWindows(t, r, whole, 4, 28)

	r.passAt(weekLater)
	r.github.Refuse(set.third, codesurvivaltest.Refusal{})
	again := weekLater.Add(24 * time.Hour)
	r.passAt(again)
	measured := r.measuredAt(widgets)
	for n := range 4 {
		if !measured[day(n)].Equal(again) {
			t.Errorf("day %d was measured at %s, want by the retry at %s", n, measured[day(n)], again)
		}
	}
	for _, row := range r.dump(resultTables...) {
		if strings.Contains(row, set.third) {
			t.Errorf("a result holds the commit that blocked the days once they are measured: %s", row)
		}
	}
	requireSameWindows(t, r, whole, 0, 28)
}
