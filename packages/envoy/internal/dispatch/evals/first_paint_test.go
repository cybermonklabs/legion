package evals

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The first paint of a stale cut (firstPaintOf, FoldFirstPaint, FoldFirstPaintsAtBoot): a move of
// a default repository marks the cuts it moves stale, and a read of a marked eval of such a cut
// folds its day rows until the stored rows the read reads are folded again; the first-paint batch
// folds those a first paint reads, before the rest of the refold, so from its commit a first paint
// reads the stored figures of every eval of the cut, each the fold of the rows that stand.

// paintRead is one read of a cut: its name, its window and its compare range (nil for none).
type paintRead struct {
	name    string
	window  Range
	compare *Range
}

// firstPaintReads is every offered read a first paint of a cut whose figures are built through
// through names (firstPaintOf): a window of each of FigureLengths ending on each of the last
// FigureLead+1 end days, with no compare range and with a compare range ending each of FigureLengths
// days before the window's end, of each of FigureLengths in turn.
func firstPaintReads(through time.Time) []paintRead {
	var reads []paintRead
	for back := range FigureLead + 1 {
		end := through.AddDate(0, 0, -back)
		for index, n := range FigureLengths {
			window := rangeOf(n, end)
			reads = append(reads, paintRead{name: fmt.Sprintf("%d days to %s", n, end.Format(time.DateOnly)), window: window})
			for turn, m := range FigureLengths {
				compare := rangeOf(FigureLengths[(index+turn)%len(FigureLengths)], end.AddDate(0, 0, -m))
				reads = append(reads, paintRead{name: fmt.Sprintf("%d days to %s against %s", n, end.Format(time.DateOnly), daysText(compare)), window: window, compare: &compare})
			}
		}
	}
	return reads
}

// readsOf is shapes as paintReads.
func readsOf(shapes []readShape) []paintRead {
	reads := make([]paintRead, len(shapes))
	for index, shape := range shapes {
		reads[index] = paintRead{name: shape.name, window: shape.window, compare: &shape.compare}
	}
	return reads
}

// movePierToQuay writes seedPierAndQuay's history with a logged eval of each project whose readings
// of acme/web are rows of its cut of it, builds it whole, and moves PIER's default to acme/quay,
// which a page alone publishes (pageAlone): its commit marks PIER's and QUAY's cuts of acme/quay
// stale, their first paints and their refolds to begin, and leaves some stored figures of the evals
// the marks name other than the fold of the rows that stand. It answers the evals each mark names,
// by cut.
func movePierToQuay(t *testing.T, r *refresherRig) map[staleCut][]string {
	t.Helper()
	day := seedPierAndQuay(t, r)
	for _, project := range []string{"PIER", "QUAY"} {
		r.logReviews(t, project, day, webRepo)
	}
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.pageAlone(t)
	marked := markedEvals(r.staleMarks(t))
	if cuts := slices.SortedFunc(maps.Keys(marked), compareStaleCuts); !slices.Equal(cuts, []staleCut{{project: "PIER", repo: quayRepo}, {project: "QUAY", repo: quayRepo}}) {
		t.Fatalf("PIER's move to %s marked the cuts %v, and the check needs PIER's and QUAY's %s alone", quayRepo, cuts, quayRepo)
	}
	r.expectMarks(t, "the page that published PIER's move to "+quayRepo, marked)
	if staleOf := staleFiguresOf(t, r.storedFigures(t), r.truthFigures(t)); len(staleOf) == 0 {
		t.Fatalf("the move left every stored figure the fold of the rows that stand, and the check needs it to move some")
	}
	return marked
}

// expectReads makes each of reads of each cut of marked, and checks that each eval of the cut's
// mark is stated from the stored figures where stored answers true for the cut, and from its day
// rows where it answers false, every eval of each read the fold of its day rows.
func (r *refresherRig) expectReads(t *testing.T, what string, marked map[staleCut][]string, reads []paintRead, stored func(staleCut) bool) {
	t.Helper()
	for _, cut := range slices.SortedFunc(maps.Keys(marked), compareStaleCuts) {
		fromStored := stored(cut)
		for _, read := range reads {
			stated, folded, fromFigures := r.listValues(t, cut.project, []string{cut.repo}, read.window, read.compare)
			for _, id := range slices.Sorted(maps.Keys(folded)) {
				if (marked[cut] == nil || slices.Contains(marked[cut], id)) && fromFigures[id] != fromStored {
					t.Errorf("%s: the read of %s's %s over %s states %s, which its mark names, from the stored figures %t, want %t",
						what, cut.project, cut.repo, read.name, id, fromFigures[id], fromStored)
				}
				if stated[id] != folded[id] {
					t.Errorf("%s: the read of %s's %s over %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
						what, cut.project, cut.repo, read.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
				}
			}
		}
	}
}

// figureLineKey is the cut, the eval and the day of the stored figures row line names (rangeLine,
// dayFigureLine): a range row's end day, or a day row's day, and whether it is a range row.
func figureLineKey(t *testing.T, line string) (staleCut, string, time.Time, bool) {
	t.Helper()
	var kind, project, repo, deployRepo, day, evalID string
	var days int
	fields, format := []any{&kind, &project, &repo, &deployRepo, &day, &evalID}, "%s %s %q %q %s %s"
	ranged := strings.HasPrefix(line, "range ")
	if ranged {
		fields, format = []any{&kind, &project, &repo, &deployRepo, &days, &day, &evalID}, "%s %s %q %q %d %s %s"
	}
	if _, err := fmt.Sscanf(line, format, fields...); err != nil {
		t.Fatalf("read the stored figures row %q: %v", line, err)
	}
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatalf("read the day of the stored figures row %q: %v", line, err)
	}
	return staleCut{project: project, repo: repo}, evalID, at, ranged
}

// expectPaintTruth checks that every stored row of the first paint of through (firstPaintOf) of
// each eval marked names, in its cut (every eval of it where it names none), holds truth's
// figures, and that truth holds each of them: the stored rows a first paint reads of the evals a
// mark names are the fold of the rows that stand. It fails where it finds no such row, which would
// make the check hold of nothing.
func expectPaintTruth(t *testing.T, what string, through time.Time, marked map[staleCut][]string, stored, truth map[string]figureRow) {
	t.Helper()
	paint := firstPaintOf(through)
	of := func(line string) bool {
		cut, evalID, day, ranged := figureLineKey(t, line)
		evals, held := marked[cut]
		switch {
		case !held || evals != nil && !slices.Contains(evals, evalID):
			return false
		case ranged:
			return paint.holds(day)
		}
		return !day.Before(paint.days.From) && day.Before(paint.days.To)
	}
	checked := 0
	var wrong []string
	for _, line := range slices.Sorted(maps.Keys(truth)) {
		if !of(line) {
			continue
		}
		checked++
		if row, held := stored[line]; !held {
			wrong = append(wrong, "missing "+line)
		} else if !row.same(truth[line]) {
			wrong = append(wrong, "stale "+line)
		}
	}
	for _, line := range slices.Sorted(maps.Keys(stored)) {
		if _, held := truth[line]; !held && of(line) {
			wrong = append(wrong, "extra "+line)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: the first paint through %s holds no row of the evals the marks %v name, and the check needs some", what, through.Format(time.DateOnly), marked)
	}
	if len(wrong) > 0 {
		t.Errorf("%s: %d of the %d rows of the first paint through %s are not the fold of the day rows that stand, first ones:\n\t%s",
			what, len(wrong), checked, through.Format(time.DateOnly), strings.Join(wrong[:min(len(wrong), 12)], "\n\t"))
	}
}

// foldFirstPaints folds the first paint of each cut of marked (FoldFirstPaint), as the boot and the
// refresher's first batches fold them, and checks that each folded the first paint of through.
func (r *refresherRig) foldFirstPaints(t *testing.T, marked map[staleCut][]string, through time.Time) {
	t.Helper()
	for _, cut := range slices.SortedFunc(maps.Keys(marked), compareStaleCuts) {
		paint, err := FoldFirstPaint(t.Context(), r.database.Pool, r.build, cut.project, cut.repo)
		if err != nil {
			t.Fatalf("fold the first paint of %s's cut of %s: %v", cut.project, cut.repo, err)
		}
		if !paint.Folded || !paint.Through.Equal(through) || paint.Evals == 0 {
			t.Fatalf("the first paint of %s's cut of %s folded %+v, want the first paint through %s of its evals", cut.project, cut.repo, paint, through.Format(time.DateOnly))
		}
	}
}

// TestAFirstPaintIsReadFromTheStoredFiguresOnceItsBatchCommits: over seedPierAndQuay's history,
// PIER's default moves to acme/quay and a page alone publishes it (movePierToQuay), which marks
// PIER's and QUAY's cuts of acme/quay stale, their first paints owed, and leaves some stored
// figures of the evals the marks name folded from the rows before the move. Every read a first
// paint of either cut names (firstPaintReads: each window of FigureLengths ending on each of the
// last three end days, with no compare range and with one ending each of FigureLengths days before
// it) states those evals from their day rows until the first-paint batch of the cut commits; once
// it has, with no later batch of the refold, every such read states them from the stored figures,
// each the fold of its day rows, and every stored row of the first paint is the fold of the rows
// that stand (expectPaintTruth). A read of a window ending a day before the earliest a first paint
// names, or of a first paint's window with a compare range no first paint names (pastShapes),
// still states them from their day rows, each the fold of those rows. Red where a mark with no
// first paint is read as one whose first paint is folded: the reads before the batch then state
// the stored figures of the rows before the move, which differ from the fold of the rows that
// stand. Red too where the first paint leaves out a window end a first paint names, the bench's
// first paint ending today among them, or a compare range one window back of one, or folds too
// few of its days; and where a read serves the stored figures of a past window the first paint
// did not fold.
func TestAFirstPaintIsReadFromTheStoredFiguresOnceItsBatchCommits(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
	reads := firstPaintReads(through)
	never := func(staleCut) bool { return false }
	r.expectReads(t, "before the first-paint batch", marked, reads, never)
	r.foldFirstPaints(t, marked, through)
	for cut, mark := range r.staleMarks(t) {
		if mark.generation != 1 || mark.ready == nil || !mark.ready.Equal(through) || mark.next != nil {
			t.Errorf("the first-paint batch left %s's cut of %s marked at %+v, want generation 1 with its first paint folded through %s and its refold to begin",
				cut.project, cut.repo, mark, through.Format(time.DateOnly))
		}
	}
	expectPaintTruth(t, "after the first-paint batch", through, marked, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the first-paint batch", marked, reads, func(staleCut) bool { return true })
	r.expectReads(t, "after the first-paint batch, of the past", marked, readsOf(pastShapes()), never)
	t.Logf("%d reads of each of %d cuts of a first paint, %v marked", len(reads), len(marked), marked)
}

// TestAWriteAfterTheFirstPaintKeepsItsStoredFiguresTheFoldOfTheRows: over movePierToQuay's move,
// once the first-paint batch of both marked cuts has folded and before any later batch of their
// refold, acme/quay#1, a pull request whose rows the move moved into PIER's cut of acme/quay, is
// opened two hours earlier, and the page that reads the change writes its day rows (pageAlone):
// the write folds the figures of the days it moved in every cut its rows reach, the stale ones
// among them (foldFiguresInTx), so it rewrites stored rows of PIER's first paint, every stored row
// of both first paints is the fold of the rows that stand (expectPaintTruth), and a first paint of
// each cut states every eval from the stored figures, each the fold of its day rows. Red where a
// write folds the figures of its project's own rows alone, or leaves a stale cut to its refold:
// the first paint then states PIER's pr-open-to-merge from figures of the rows before the write.
func TestAWriteAfterTheFirstPaintKeepsItsStoredFiguresTheFoldOfTheRows(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
	r.foldFirstPaints(t, marked, through)
	painted := r.storedFigures(t)
	day := refresherToday.AddDate(0, 0, -1)
	if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
		Repo: quayRepo, Number: 1, Title: "PIER-1: change the pier", URL: "https://github.com/" + quayRepo + "/pull/1", Author: "dev-ana",
		CreatedAt: new(day.Add(6 * time.Hour)), MergedAt: new(day.Add(9 * time.Hour)), Sessions: []string{},
		Attribution: &delivery.AttributionInputs{TitleKeys: []string{"PIER-1"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
	}); err != nil {
		t.Fatalf("open %s#1 two hours earlier: %v", quayRepo, err)
	}
	r.pageAlone(t)
	pier := staleCut{project: "PIER", repo: quayRepo}
	if mark := r.staleMarks(t)[pier]; mark.ready == nil || mark.next != nil {
		t.Fatalf("the page left PIER's cut of %s marked at %+v, and the check needs its first paint folded and its refold to begin", quayRepo, mark)
	}
	expectPaintTruth(t, "after the write", through, marked, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the write", marked, firstPaintReads(through), func(staleCut) bool { return true })
	written := 0
	for _, line := range writtenFigures(painted, r.storedFigures(t)) {
		if cut, evalID, _, _ := figureLineKey(t, line); cut == pier && slices.Contains(marked[pier], evalID) {
			written++
		}
	}
	if written == 0 {
		t.Errorf("the write of %s#1's day rows rewrote no stored row of the evals PIER's mark of %s names", quayRepo, quayRepo)
	}
	t.Logf("the write rewrote %d stored rows of the evals PIER's mark names in its stale cut of %s", written, quayRepo)
}

// TestANewDaysFirstPaintIsFoldedFirstAndReadOnceItIs: over movePierToQuay's move, once the
// first-paint batch of both marked cuts has folded the first paint of today's last end day, a new
// UTC day comes and the figures walk folds the new last end day of every cut (walkFigures, alone
// of the pass): a read of a window ending on it, with a compare range one window back, no stored
// row of the old first paint, states the evals each mark names from their day rows. The pass of
// the new day under an owed wake folds the new day's first paint of the first cut, PIER's, before
// any batch of its refold, and stops; from then on those reads of PIER's cut state every eval from
// the stored figures, and those of QUAY's still from the day rows, until the next pass folds its
// first paint too. Each read answers the fold of its day rows. Red where a mark whose first paint
// was folded on an earlier day owes none: the new day's reads then fold the day rows until the
// refold, newest end day first, has come down past their compare ranges.
func TestANewDaysFirstPaintIsFoldedFirstAndReadOnceItIs(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
	r.foldFirstPaints(t, marked, through)
	r.now = r.now.AddDate(0, 0, 1)
	next := through.AddDate(0, 0, 1)
	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the refresher's pass of the new day: %v", err)
	}
	if _, err := pass.walkFigures(ctx); err != nil {
		t.Fatalf("walk the figures of the new day: %v", err)
	}
	r.expectBuilt(t, "the figures walk of the new day", refresherToday.AddDate(0, 0, 1))
	var reads []paintRead
	for _, n := range []int{7, 28} {
		compare := rangeOf(n, next.AddDate(0, 0, -n))
		reads = append(reads, paintRead{name: fmt.Sprintf("the live %d days just after the new midnight", n), window: rangeOf(n, next), compare: &compare})
	}
	pier, quay := staleCut{project: "PIER", repo: quayRepo}, staleCut{project: "QUAY", repo: quayRepo}
	r.expectReads(t, "before the new day's first paint", marked, reads, func(staleCut) bool { return false })
	r.owedPass(t, &gapBatch{})
	marks := r.staleMarks(t)
	if mark := marks[pier]; mark.ready == nil || !mark.ready.Equal(next) || mark.next != nil {
		t.Fatalf("the new day's first pass left PIER's cut of %s marked at %+v, want its first paint folded through %s first, its refold to begin", quayRepo, mark,
			next.Format(time.DateOnly))
	}
	if mark := marks[quay]; mark.ready == nil || !mark.ready.Equal(through) {
		t.Fatalf("the new day's first pass left QUAY's cut of %s marked at %+v, and the check needs its first paint of %s still owed", quayRepo, mark, through.Format(time.DateOnly))
	}
	r.expectReads(t, "once PIER's first paint of the new day is folded", marked, reads, func(cut staleCut) bool { return cut == pier })
	r.owedPass(t, &gapBatch{})
	if mark := r.staleMarks(t)[quay]; mark.ready == nil || !mark.ready.Equal(next) || mark.next != nil {
		t.Fatalf("the new day's second pass left QUAY's cut of %s marked at %+v, want its first paint folded through %s, its refold to begin", quayRepo, mark,
			next.Format(time.DateOnly))
	}
	r.expectReads(t, "once both first paints of the new day are folded", marked, reads, func(staleCut) bool { return true })
}

// TestTheBootFoldsTheFirstPaintOfEveryMarkBeforeItServes: over movePierToQuay's move, with each
// mark naming every eval as the boot's rename pass marks a cut (evals null), and no first paint
// folded, a read of a first paint of either cut states no eval from the stored figures. The boot's
// fold (FoldFirstPaintsAtBoot), with no process holding the refresher's lease, takes it and folds
// the first paint of both marks, every eval of each, waiting for no one: no mark owes a first paint
// once it returns, every stored row of both first paints is the fold of the rows that stand, and a
// first paint of each cut states every eval from the stored figures, each the fold of its day rows.
// Red where the boot folds nothing, or skips a mark that names every eval.
func TestTheBootFoldsTheFirstPaintOfEveryMarkBeforeItServes(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	r.exec(t, "name every eval of each mark, as the rename pass marks a cut", `update eval_figure_cuts_stale set evals = null`)
	every := map[staleCut][]string{}
	for cut := range marked {
		every[cut] = nil
	}
	through := refresherToday.AddDate(0, 0, FigureLead)
	r.expectReads(t, "before the boot", every, readsOf(moveShapes()), func(staleCut) bool { return false })
	folded, err := FoldFirstPaintsAtBoot(ctx, pool, storetest.Leases(t, pool), r.build)
	if err != nil || folded.Marks != len(every) || folded.Evals == 0 || folded.Waited {
		t.Fatalf("the boot's fold answered %+v (%v), want the first paints of %d marks folded with no wait", folded, err, len(every))
	}
	if owed, err := owedFirstPaints(ctx, pool); err != nil || len(owed) != 0 {
		t.Fatalf("after the boot's fold the marks %v owe a first paint (%v), want none", owed, err)
	}
	for cut, mark := range r.staleMarks(t) {
		if mark.ready == nil || !mark.ready.Equal(through) || mark.evals != nil {
			t.Errorf("the boot left %s's cut of %s marked at %+v, want its first paint of every eval folded through %s", cut.project, cut.repo, mark, through.Format(time.DateOnly))
		}
	}
	expectPaintTruth(t, "after the boot", through, every, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the boot", every, firstPaintReads(through), func(staleCut) bool { return true })
}

// TestABootWaitsForTheFirstPaintsTheProcessHoldingTheLeaseFolds: over movePierToQuay's move, while
// another session holds the refresher's lease, as another process's refresher pass holds it, the
// boot's fold (FoldFirstPaintsAtBoot) neither folds nor skips: given a context that ends, it
// answers the context's error once it ends, having waited, the first paints still owed; given one
// that does not, it is still waiting seconds later, the first paints still owed, and once the
// holder folds them (FoldFirstPaint, as its pass folds them first) and gives the lease back, it
// answers, having waited and folded none itself, with no mark owing a first paint. Red where a
// boot that finds the lease held goes on at once: the server would then serve a first paint the
// fold of the day rows.
func TestABootWaitsForTheFirstPaintsTheProcessHoldingTheLeaseFolds(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	held, release, released := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	holder, waiter := storetest.Leases(t, pool), storetest.Leases(t, pool)
	go func() {
		var folded error
		duty.Exclusive(context.Background(), holder, RefresherLease, time.Time{}, 0, func(ctx context.Context) {
			close(held)
			<-release
			for _, cut := range slices.SortedFunc(maps.Keys(marked), compareStaleCuts) {
				if _, err := FoldFirstPaint(ctx, pool, r.build, cut.project, cut.repo); err != nil {
					folded = err
					return
				}
			}
		})
		released <- folded
	}()
	select {
	case <-held:
	case <-time.After(time.Minute):
		t.Fatal("the other session took no lease in a minute")
	}
	owes := func(what string) {
		t.Helper()
		if owed, err := owedFirstPaints(ctx, pool); err != nil || len(owed) != len(marked) {
			t.Fatalf("%s: the marks %v owe a first paint (%v), want the %d marked", what, owed, err, len(marked))
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*firstPaintBootWait)
	folded, err := FoldFirstPaintsAtBoot(bounded, pool, waiter, r.build)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !folded.Waited || folded.Marks != 0 {
		t.Fatalf("the boot's fold bounded by %s while another session held the lease answered %+v (%v), want a wait and no fold, ended by its deadline", 2*firstPaintBootWait, folded, err)
	}
	owes("after the bounded boot")
	type answer struct {
		folded FirstPaintsAtBoot
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		folded, err := FoldFirstPaintsAtBoot(ctx, pool, waiter, r.build)
		answered <- answer{folded, err}
	}()
	select {
	case got := <-answered:
		t.Fatalf("the boot's fold answered %+v (%v) while another session held the lease and the first paints were owed, want it to wait", got.folded, got.err)
	case <-time.After(3 * firstPaintBootWait):
	}
	owes("while the boot waits")
	free()
	if err := <-released; err != nil {
		t.Fatalf("the lease holder's fold of the first paints: %v", err)
	}
	var got answer
	select {
	case got = <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the boot's fold had not answered a minute after the lease holder folded the first paints and gave the lease back")
	}
	if got.err != nil || !got.folded.Waited || got.folded.Marks != 0 {
		t.Fatalf("the boot's fold answered %+v (%v), want a wait and no fold of its own", got.folded, got.err)
	}
	if owed, err := owedFirstPaints(ctx, pool); err != nil || len(owed) != 0 {
		t.Fatalf("once the boot's fold answered the marks %v owe a first paint (%v), want none", owed, err)
	}
}

// TestAReadOfAStaleCutReadsOnlyTheStoredRowsItsGenerationFoldedAgain: of a cut whose figures are
// built through two days after today and whose mark names every eval, a read reads the stored
// figures (builtCut.serves) exactly where every stored row it reads is one the mark's generation
// has folded again: with the first paint of that day folded, the bench's first paint (84 days to
// today against the 84 before), the live view's 85 days to tomorrow against the 85 before, the
// same just after the next midnight, a day later against the compare range its page kept, and 7
// days to today against the week before; with no first paint and no refold, none; with the first
// paint alone, no window ending a day before the earliest a first paint names, no compare range
// no first paint names, and no window of a first paint's day older than the first paint's days;
// each of those once the refold, newest end day first, has come down past its compare range's end
// and its window's first day, and none of them before; a first paint of an earlier day none of the
// later day's windows; and an eval no mark names every read. Red where the first paint holds the
// windows ending through-1 and through alone, which leaves the bench's first paint to the fold of
// the day rows; where a read of the past is served before the refold has folded its ranges and
// days; and where a first paint of an earlier day answers the windows of a later one.
func TestAReadOfAStaleCutReadsOnlyTheStoredRowsItsGenerationFoldedAgain(t *testing.T) {
	t.Parallel()
	today := refresherToday
	through := today.AddDate(0, 0, FigureLead)
	day := func(offset int) time.Time { return today.AddDate(0, 0, offset) }
	painted := func(at time.Time, next *time.Time) builtCut {
		paint := firstPaintOf(at)
		return builtCut{through: through, every: true, paint: &paint, next: next}
	}
	cases := []struct {
		what    string
		cut     builtCut
		eval    string
		window  Range
		compare *Range
		want    bool
	}{
		{"the bench's first paint", painted(through, nil), "", rangeOf(84, today), new(rangeOf(84, day(-84))), true},
		{"the live view's first paint", painted(through, nil), "", rangeOf(85, day(1)), new(rangeOf(85, day(1-84))), true},
		{"the live view just after a midnight", painted(through, nil), "", rangeOf(85, day(2)), new(rangeOf(85, day(1-84))), true},
		{"7 days to today", painted(through, nil), "", rangeOf(7, today), new(rangeOf(7, day(-7))), true},
		{"7 days to today with no compare range", painted(through, nil), "", rangeOf(7, today), nil, true},
		{"the bench's first paint with no first paint folded", builtCut{through: through, every: true}, "", rangeOf(84, today), new(rangeOf(84, day(-84))), false},
		{"7 days to yesterday", painted(through, nil), "", rangeOf(7, day(-1)), new(rangeOf(7, day(-8))), false},
		{"7 days to yesterday once the refold has folded yesterday", painted(through, new(day(-2))), "", rangeOf(7, day(-1)), new(rangeOf(7, day(-8))), true},
		{"7 days to yesterday before the refold has folded yesterday", painted(through, new(day(-1))), "", rangeOf(7, day(-1)), new(rangeOf(7, day(-8))), false},
		{"28 days to today against a week long past", painted(through, nil), "", rangeOf(28, today), new(rangeOf(7, day(-11))), false},
		{"28 days to today against a week the refold has passed", painted(through, new(day(-12))), "", rangeOf(28, today), new(rangeOf(7, day(-11))), true},
		{"84 days one window back", painted(through, nil), "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), false},
		{"84 days one window back, the refold past its window alone", painted(through, new(day(-100))), "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), false},
		{"84 days one window back, the refold past both", painted(through, new(day(-168))), "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), true},
		{"85 days ending on a first paint's compare end, with no compare range", painted(through, nil), "", rangeOf(85, day(-83)), nil, false},
		{"85 days ending on a first paint's compare end, the refold past its first day", painted(through, new(day(-169))), "", rangeOf(85, day(-83)), nil, true},
		{"the live view's first paint with yesterday's first paint", painted(through.AddDate(0, 0, -1), nil), "", rangeOf(85, day(2)), new(rangeOf(85, day(2-85))), false},
		{"an eval no mark names", builtCut{through: through, stale: map[string]bool{"EVAL/marked": true}}, "EVAL/other", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), true},
		{"an eval a mark names", builtCut{through: through, stale: map[string]bool{"EVAL/marked": true}}, "EVAL/marked", rangeOf(84, today), new(rangeOf(84, day(-84))), false},
	}
	for _, tc := range cases {
		eval := cmp.Or(tc.eval, "EVAL/marked")
		if got := tc.cut.serves(eval, tc.window, tc.compare); got != tc.want {
			t.Errorf("%s: a read of %s against %s is served from the stored figures %t, want %t", tc.what, daysText(tc.window), comparedText(tc.compare), got, tc.want)
		}
	}
}

// daysText is r's days as a message names them.
func daysText(r Range) string {
	return r.From.Format(time.DateOnly) + ".." + r.To.Format(time.DateOnly)
}

// comparedText is a compare range as a message names it, "none" where there is none.
func comparedText(r *Range) string {
	if r == nil {
		return "none"
	}
	return daysText(*r)
}
