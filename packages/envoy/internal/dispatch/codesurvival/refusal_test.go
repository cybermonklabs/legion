package codesurvival

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
)

// refusals is k8's branch of 31 days, a change a day, and its three commits: first, day 10's
// change, and second, day 12's, whose patches the tests refuse and serve whole on files[]; and
// third, a commit of day 3 that merged no pull request and adds 12 files of 2 lines each, which
// tests refuse with files[] withholding 3 of its files, leaving its last 2 unlisted, or
// withholding one and serving the next with no patch and no changed lines.
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

// wideBranch is a branch of 31 days, a change a day, and the SHA of one more commit on day 3 that
// merged no pull request and adds as many files as GitHub's files[] lists at most
// (codesurvivaltest.FilesLimit, 3,000), each of one line.
func wideBranch(t *testing.T) (*codesurvivaltest.Branch, string) {
	t.Helper()
	w := newWriter(t)
	var wide string
	w.days(0, 31, func(int) int { return 1 }, func(n int) {
		if n != 3 {
			return
		}
		at := day(n).Add(6 * time.Hour)
		for file := range codesurvivaltest.FilesLimit {
			w.add(fmt.Sprintf("data/wide%04d.txt", file), 1, at, "wide")
		}
		wide = w.h.Push("as many files as files[] lists", at)
	})
	return w.h.Branch(), wide
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

// requireBlocked fails t unless r's first pass blocked days 0 to 3 by the commit sha and measured
// days 4 to 27: each blocked day's code_days row names sha, its count of withheld files files,
// whether its listing left out files whose number no answer names (unlisted) and its reason, its
// status row counts one blocked day and those files, and every window that holds one reads "no
// data", never 0, with its blocked days and their files.
func requireBlocked(t *testing.T, r *rig, sha string, files int, unlisted bool, reason string) {
	t.Helper()
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
		var blocking, why string
		var count int
		var left bool
		if err := r.database.Pool.QueryRow(r.ctx, `select blocked_sha, blocked_files, blocked_unlisted, blocked_reason from code_days where repo = $1 and day = $2`, widgets, day(n)).
			Scan(&blocking, &count, &left, &why); err != nil || blocking != sha || count != files || left != unlisted || why != reason {
			t.Errorf("blocked day %d holds %q, %d files, unlisted %t and %q (%v), want %s, %d, %t and %q", n, blocking, count, left, why, err, sha, files, unlisted, reason)
		}
		for _, row := range r.codeRows(widgetsProject, day(n)) {
			if !strings.Contains(row, "|"+fold.SeriesStatus+"|") || !strings.Contains(row, `"blocked": 1`) || !strings.Contains(row, fmt.Sprintf(`"blocked_files": %d`, files)) {
				t.Errorf("blocked day %d holds the row %q, want a status row with blocked 1 and %d files", n, row, files)
			}
		}
	}
	for start := 0; start <= 3; start++ {
		read := r.read(widgetsProject, "agents", day(start), day(start+7))
		blocked := 4 - start
		if read.survival.Share != nil || read.rework.Share != nil || read.survival.Added != nil ||
			read.status.BlockedDays != blocked || read.status.BlockedFiles != files*blocked {
			t.Errorf("the window from day %d reads survival %s, rework %s, %d blocked days and %d files, want no data, %d days and %d files",
				start, value(read.survival.Share), value(read.rework.Share), read.status.BlockedDays, read.status.BlockedFiles, blocked, files*blocked)
		}
	}
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
// commit, the count 3, no file left unlisted, and why; every window that holds one reads "no data",
// never 0, with the count; days 4 to 27 read what a run whose diff route answered reads. Once the
// double serves the commit whole, the pass a week and a day later, which has fewer than seven days
// waiting, tries the blocked days again and measures them: no row of the five tables holds the
// commit's SHA, and those windows read the whole run's numbers.
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
	requireBlocked(t, r, set.third, 3, false, "diff refused (422), files[] withheld 3 of 12 files")
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

// A files[] that lists GitHub's limit of 3,000 files blocks the days whose 30 days hold its commit
// (R9), though every patch it lists is there: GitHub lists no file past the limit, so such a
// listing may hold only part of the commit, and no answer names how many files it left out. Day
// 3's commit, which merged no pull request, adds 3,000 files; the double refuses its patch with a
// 406 and serves all 3,000 on files[], 30 pages. Days 0 to 3 are blocked by it, each naming the
// commit, no file withheld (blocked_files 0), files left unlisted in a number no answer names
// (blocked_unlisted), and GitHub's limit; every window that holds one reads "no data", never 0;
// days 4 to 27 read what a run whose diff route answered reads.
func TestAFilesListOfGitHubsLimitBlocksTheDaysWhoseThirtyDaysHoldIt(t *testing.T) {
	t.Parallel()
	branch, wide := wideBranch(t)
	whole := widgetsRig(t, branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, branch)
	r.github.Refuse(wide, codesurvivaltest.Refusal{Status: 406, Body: "Sorry, the diff exceeded the maximum number of files"})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[wide], filesReads(requests, wide); reads != 1 || pages != codesurvivaltest.FilesLimit/100 {
		t.Fatalf("the refused patch was read %d times and its files[] in %d pages, want once and in %d", reads, pages, codesurvivaltest.FilesLimit/100)
	}
	requireBlocked(t, r, wide, 0, true, "diff refused (406), files[] withheld 0 of 3000 files and listed 3000 files, GitHub's limit")
	requireSameWindows(t, r, whole, 4, 28)
}

// A files[] whose files add and delete fewer lines than the commit's stats count blocks the days
// whose 30 days hold the commit (R9), though every patch it lists is there: the listing holds
// only part of the commit, and no answer names how many files it left out. Day 3's commit of 12
// files and 24 lines, which merged no pull request, refuses on the diff route, and its files[]
// lists its first 10 files, 20 lines, while its stats count all 24. Days 0 to 3 are blocked by it,
// each naming the commit, no file withheld, files left unlisted in a number no answer names, and
// the lines it holds of the commit's; every window that holds one reads "no data", never 0; days 4
// to 27 read what a run whose diff route answered reads.
func TestAFilesListShortOfTheCommitsStatsBlocksTheDaysWhoseThirtyDaysHoldIt(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.third, codesurvivaltest.Refusal{Status: 422, Body: "diff too large", Unlisted: 2})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[set.third], filesReads(requests, set.third); reads != 1 || pages != 1 {
		t.Fatalf("the refused patch was read %d times and its files[] %d times, want once each", reads, pages)
	}
	requireBlocked(t, r, set.third, 0, true, "diff refused (422), files[] withheld 0 of 10 files and listed 20 of the commit's 24 changed lines")
	requireSameWindows(t, r, whole, 4, 28)
}

// A files[] that withholds a file blocks the days whose 30 days hold its commit by the files it
// withheld alone (R9): the lines its files change are held to the commit's stats only where it
// withheld none, so a listing already blocked is not counted again for them. Day 3's commit of 12
// files and 24 lines, which merged no pull request, refuses on the diff route; its files[] lists
// all 12, the first with no patch and its 2 changed lines, the second with no patch and changes 0,
// as GitHub serves a text file it shows no diff of, while its stats count all 24 lines. Days 0 to
// 3 are blocked by it, each naming the commit, the one file withheld, no file left unlisted and no
// count of lines; every window that holds one reads "no data", never 0; days 4 to 27 read what a
// run whose diff route answered reads.
func TestAFilesListThatWithholdsAFileIsBlockedByThatFileAlone(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.third, codesurvivaltest.Refusal{Status: 422, Body: "diff too large", Withheld: 1, Uncounted: 1})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[set.third], filesReads(requests, set.third); reads != 1 || pages != 1 {
		t.Fatalf("the refused patch was read %d times and its files[] %d times, want once each", reads, pages)
	}
	requireBlocked(t, r, set.third, 1, false, "diff refused (422), files[] withheld 1 of 12 files")
	requireSameWindows(t, r, whole, 4, 28)
}

// A files[] that withholds a file and leaves another out blocks the days whose 30 days hold its
// commit, and says that it left files out (R9): the file it withheld is the count it knows, and
// the files it did not list are a number no answer names, so its row states at least that count
// and never an exact one. Day 3's commit of 12 files and 24 lines, which merged no pull request,
// refuses on the diff route; its files[] lists 11 of them, the first with no patch and its 2
// changed lines, 22 lines in all, while its stats count all 24. Days 0 to 3 are blocked by it,
// each naming the commit, the one file withheld, files left unlisted, and the lines it holds of
// the commit's; days 4 to 27 read what a run whose diff route answered reads.
func TestAFilesListThatWithholdsAFileAndLeavesOneOutSaysItLeftFilesOut(t *testing.T) {
	t.Parallel()
	set := refusalBranch(t)
	whole := widgetsRig(t, set.branch)
	whole.passAt(firstPass)
	r := widgetsRig(t, set.branch)
	r.github.Refuse(set.third, codesurvivaltest.Refusal{Status: 422, Body: "diff too large", Withheld: 1, Unlisted: 1})
	requests := r.passAt(firstPass)
	if reads, pages := patchReads(requests)[set.third], filesReads(requests, set.third); reads != 1 || pages != 1 {
		t.Fatalf("the refused patch was read %d times and its files[] %d times, want once each", reads, pages)
	}
	requireBlocked(t, r, set.third, 1, true, "diff refused (422), files[] withheld 1 of 11 files and listed 22 of the commit's 24 changed lines")
	requireSameWindows(t, r, whole, 4, 28)
}
