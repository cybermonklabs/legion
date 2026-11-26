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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The first paint of a stale cut (firstPaintOf, FoldFirstPaint, FoldFirstPaintsAtBoot): a move of
// a default repository marks the cuts it moves stale, and a read of a marked eval of such a cut
// folds its day rows until the stored rows the read reads are folded again; the first-paint batch
// folds those a first paint reads, before the rest of the refold, so from its commit a first paint
// reads the stored figures of every eval of the cut, each the fold of the rows that stand. The rest
// of the refold (refoldStaleCuts), the refold of a fold key a register write moved, and the figures
// walk (walkFigures) fold in transactions of a batch of end days each, which the tests below bound
// by the end days each transaction wrote (endsWritten).

// paintRead is one read of a cut: its name, its window and its compare range (nil for none).
type paintRead struct {
	name    string
	window  Range
	compare *Range
}

// firstPaintReads is every offered read of the current window a first paint of a cut whose figures
// are built through through names (firstPaintOf): a window of each of FigureLengths ending on each
// of the last FigureLead+1 end days, with no compare range and with a compare range ending each of
// FigureLengths days before the window's end, of each of FigureLengths in turn.
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

// windowBackReads is every offered read one window back a first paint of a cut whose figures are
// built through through names (firstPaintOf): a window of 84 or 85 days, the 84-day preset and the
// live view's 85 days, that ends where a compare range of 84 or 85 days of a read of the current
// window ends, 84 or 85 days before one of the last FigureLead+1 end days, with no compare range and
// with a compare range of each of FigureLengths ending its window's length before it.
func windowBackReads(through time.Time) []paintRead {
	longest := []int{figureSpan - 1, figureSpan}
	var reads []paintRead
	named := map[string]bool{}
	add := func(read paintRead) {
		if !named[read.name] {
			named[read.name] = true
			reads = append(reads, read)
		}
	}
	for back := range FigureLead + 1 {
		for _, n := range longest {
			end := through.AddDate(0, 0, -back-n)
			for _, m := range longest {
				window := rangeOf(m, end)
				add(paintRead{name: fmt.Sprintf("%d days to %s", m, end.Format(time.DateOnly)), window: window})
				for _, length := range FigureLengths {
					compare := rangeOf(length, end.AddDate(0, 0, -m))
					add(paintRead{name: fmt.Sprintf("%d days to %s against %s", m, end.Format(time.DateOnly), daysText(compare)), window: window, compare: &compare})
				}
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
// which a page alone publishes (pageAlone): its commit marks PIER's and QUAY's own cuts ("") and
// their cuts of acme/quay stale, folds the first paint of each and leaves their refolds to begin
// (expectMarks). It answers the evals each mark names, by cut.
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
	moves := []staleCut{{project: "PIER"}, {project: "PIER", repo: quayRepo}, {project: "QUAY"}, {project: "QUAY", repo: quayRepo}}
	if cuts := slices.SortedFunc(maps.Keys(marked), compareStaleCuts); !slices.Equal(cuts, moves) {
		t.Fatalf("PIER's move to %s marked the cuts %v, and the check needs PIER's and QUAY's own rows and %s alone", quayRepo, cuts, quayRepo)
	}
	r.expectMarks(t, "the page that published PIER's move to "+quayRepo, marked)
	return marked
}

// renameLegionDefault writes the delivery and Legion histories (seedHistories), gives the Legion
// project acme/legion as its default repository, builds it whole, and renames acme/legion to
// acme/legion-next by the boot's rename pass (store.RewriteRenamedRepositories), which marks the
// project's cuts of the old and the new name stale for every eval, their first paints owed: the new
// name's stored figures hold none of the project's ':default' rows, which its read now folds, and
// the old name's hold them. It answers the marks, by cut, each naming every eval (nil).
func renameLegionDefault(t *testing.T, r *refresherRig) map[staleCut][]string {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	_, legion := r.seedHistories(t)
	project := legion.Project
	r.exec(t, "give the Legion project a default repository", `update projects set default_repo = $2 where key = $1`, project, "acme/legion")
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	r.exec(t, "declare the rename of acme/legion", `insert into repo_projects (repo, project, created_by, renamed_to)
		values ('acme/legion', $1, '{"kind":"user","id":"alice"}', 'acme/legion-next')`, project)
	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil || pass.Failures != 0 {
		t.Fatalf("the boot's rename pass = %+v (%v), want no failure", pass, pass.Err())
	}
	marked := map[staleCut][]string{}
	for cut, mark := range r.staleMarks(t) {
		if mark.ready != nil || mark.evals != nil {
			t.Fatalf("the rename pass left %s's cut of %s marked at %s, want every eval named and its first paint owed", cut.project, cut.repo, mark)
		}
		marked[cut] = nil
	}
	if want := []staleCut{{project: project, repo: "acme/legion"}, {project: project, repo: "acme/legion-next"}}; !slices.Equal(slices.SortedFunc(maps.Keys(marked), compareStaleCuts), want) {
		t.Fatalf("the rename pass marked the cuts %v, want %v", slices.SortedFunc(maps.Keys(marked), compareStaleCuts), want)
	}
	return marked
}

// expectReads makes each of reads of each cut of marked, of its one repository or of none for the
// project's own rows, and checks that each eval of the cut's mark is stated from the stored figures
// where stored answers true for the cut, and from its day rows where it answers false, every eval
// of each read the fold of its day rows. It reads each cut's state once, over every range of reads
// (readListState), and makes each read of it as listValues makes one (listState.values).
func (r *refresherRig) expectReads(t *testing.T, what string, marked map[staleCut][]string, reads []paintRead, stored func(staleCut) bool) {
	t.Helper()
	ranges := make([]Range, 0, 2*len(reads))
	for _, read := range reads {
		ranges = append(ranges, read.window)
		if read.compare != nil {
			ranges = append(ranges, *read.compare)
		}
	}
	for _, cut := range slices.SortedFunc(maps.Keys(marked), compareStaleCuts) {
		fromStored := stored(cut)
		var repos []string
		if cut.repo != "" {
			repos = []string{cut.repo}
		}
		state := r.readListState(t, cut.project, repos, ranges...)
		for _, read := range reads {
			stated, folded, fromFigures := state.values(t, read.window, read.compare)
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
// make the check hold of nothing, and logs how many range rows and day rows it checked.
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
		return dayIn(day, paint.days)
	}
	checked, ranged := 0, 0
	var wrong []string
	for _, line := range slices.Sorted(maps.Keys(truth)) {
		if !of(line) {
			continue
		}
		checked++
		if strings.HasPrefix(line, "range ") {
			ranged++
		}
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
	t.Logf("%s: the first paint through %s holds %d rows of the evals the marks name, %d of ranges over %d end days and %d of days over %d days",
		what, through.Format(time.DateOnly), checked, ranged, len(paint.picked), checked-ranged, rangeDays(paint.days))
}

// foldFirstPaints folds the first paint of each cut of marked (FoldFirstPaint), as the boot and the
// refresher's first batches fold them, checks that each folded the first paint of through, and
// logs how many statements each sent and how many stored rows they wrote in all.
func (r *refresherRig) foldFirstPaints(t *testing.T, marked map[staleCut][]string, through time.Time) {
	t.Helper()
	sent := &statementTimes{on: true, total: map[string]time.Duration{}, count: map[string]int{}}
	traced := storetest.TracedStore(t, r.database, sent)
	before := r.storedFigures(t)
	for _, cut := range slices.SortedFunc(maps.Keys(marked), compareStaleCuts) {
		from := sent.statements()
		paint, err := FoldFirstPaint(t.Context(), traced.Pool, r.build, cut.project, cut.repo)
		if err != nil {
			t.Fatalf("fold the first paint of %s's cut of %s: %v", cut.project, cut.repo, err)
		}
		if !paint.Folded || !paint.Through.Equal(through) || paint.Evals == 0 {
			t.Fatalf("the first paint of %s's cut of %s folded %+v, want the first paint through %s of its evals", cut.project, cut.repo, paint, through.Format(time.DateOnly))
		}
		t.Logf("the first paint of %s's cut %q folded %d evals in %d statements", cut.project, cut.repo, paint.Evals, sent.statements()-from)
	}
	t.Logf("the first paints of %d cuts wrote %d stored rows", len(marked), len(writtenFigures(before, r.storedFigures(t))))
}

// TestAFirstPaintIsReadFromTheStoredFiguresOnceItsBatchCommits: over the delivery and Legion
// histories, the boot's rename pass renames the Legion project's published default
// (renameLegionDefault), which marks its cuts of the old and the new name stale for every eval,
// their first paints owed, and leaves the new name's stored figures without the project's
// ':default' rows that its read now folds. Every read a first paint of either cut names
// (firstPaintReads: each window of FigureLengths ending on each of the last three end days, with no
// compare range and with one ending each of FigureLengths days before it) states every eval from
// its day rows until the first-paint batch of the cut commits; once it has, with no later batch of
// the refold, every such read states them from the stored figures, each the fold of its day rows,
// and every stored row of the first paint is the fold of the rows that stand (expectPaintTruth). A
// read of a window ending a day before the earliest a first paint names, or of a first paint's
// window with a compare range no first paint names (pastShapes), still states them from their day
// rows, each the fold of those rows; a read one window back that a first paint names
// (windowBackReads: 84 or 85 days ending where a compare range of 84 or 85 days of the current
// window ends) states them from their day rows before the batch and from the stored figures once
// it has committed. Red where a mark with no first paint is read as one whose first paint is
// folded: the reads before the batch then state the stored figures folded before the rename, which
// differ from the fold of the rows that stand. Red too where the first paint leaves out a window end
// a first paint names, the bench's first paint ending today among them, or a compare range one
// window back of one, or the ranges and days of a read one window back, or folds too few of its
// days; and where a read serves the stored figures of a past window the first paint did not fold.
func TestAFirstPaintIsReadFromTheStoredFiguresOnceItsBatchCommits(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	marked := renameLegionDefault(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
	reads := append(firstPaintReads(through), windowBackReads(through)...)
	never := func(staleCut) bool { return false }
	r.expectReads(t, "before the first-paint batch", marked, reads, never)
	r.foldFirstPaints(t, marked, through)
	marks := r.staleMarks(t)
	if cuts := slices.SortedFunc(maps.Keys(marks), compareStaleCuts); !slices.Equal(cuts, slices.SortedFunc(maps.Keys(marked), compareStaleCuts)) {
		t.Fatalf("the first-paint batches left the cuts %v marked, want the %d the rename marked", cuts, len(marked))
	}
	for cut, mark := range marks {
		if mark.generation != 1 || mark.ready == nil || !mark.ready.Equal(through) || mark.next != nil {
			t.Errorf("the first-paint batch left %s's cut of %s marked at %s, want generation 1 with its first paint folded through %s and its refold to begin",
				cut.project, cut.repo, mark, through.Format(time.DateOnly))
		}
	}
	expectPaintTruth(t, "after the first-paint batch", through, marked, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the first-paint batch", marked, reads, func(staleCut) bool { return true })
	r.expectReads(t, "after the first-paint batch, of the past", marked, readsOf(pastShapes()), never)
	t.Logf("%d reads of each of %d cuts of a first paint", len(reads), len(marked))
}

// TestAMovesCommitFoldsTheFirstPaintOfEveryCutItMarks: over seedPierAndQuay's history, PIER's
// default moves to acme/quay, and a page alone publishes it (movePierToQuay), with no refresher
// pass after it: its commit marks every cut the move moves rows of, PIER's and QUAY's own rows
// ("") among them, with its first paint folded through the last end day (expectMarks), every stored
// row of each first paint is the fold of the rows that stand under the new defaults
// (expectPaintTruth), and every read a first paint of each cut names (firstPaintReads, and
// windowBackReads one window back) states every eval from the stored figures, each the fold of its
// day rows. Red where the commit leaves the own rows unmarked, folded as before or not at all, and
// where it folds no first paint: the reads then fold the day rows, or, served the stored figures
// anyway, state the figures of the defaults before the move.
func TestAMovesCommitFoldsTheFirstPaintOfEveryCutItMarks(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
	expectPaintTruth(t, "right after the move's commit", through, marked, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "right after the move's commit", marked, append(firstPaintReads(through), windowBackReads(through)...), func(staleCut) bool { return true })
}

// TestAMovesCommitFoldsNoDayOutsideItsFirstPaints: over seedPierAndQuay's history, once it is built
// whole, a stored day row of PIER's pr-open-to-merge 400 days back, which no day row holds, is put
// in PIER's own cut and in its cut of acme/quay, as a write long past might have left one: a fold of
// the cut's history would find it and delete it. PIER's default then moves to acme/quay and a page
// alone publishes it (movePierToQuay's move): its commit marks both cuts, naming pr-open-to-merge,
// and folds their first paints, and leaves both rows standing, and every stored row it writes or
// deletes is one of the first paint of a cut it marked (firstPaintOf): it reads none of the cuts'
// history. Once the refresher settles, its refold has come down to the rows 400 days back and
// deleted them, and the stored figures are the fold of the rows that stand. Red where the commit
// folds again every day of the project's delivery evals, as it did before the first paints: the
// stamp read of every day finds the rows, which it deletes.
func TestAMovesCommitFoldsNoDayOutsideItsFirstPaints(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	seedPierAndQuay(t, r)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	evalID, longPast := EvalID("PIER", SlugPROpenToMerge), refresherToday.AddDate(0, 0, -400)
	cuts := []staleCut{{project: "PIER"}, {project: "PIER", repo: quayRepo}}
	for _, cut := range cuts {
		r.exec(t, "put a stored day row long past", `insert into eval_day_figures (project_key, repo, day, eval_id, fold_key, figures, rows_stamp, built_by)
			values ($1, $2, $3, $4, 1, '\x00', 1, 'test')`, cut.project, cut.repo, longPast, evalID)
	}
	before := r.storedFigures(t)
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.pageAlone(t)
	marks := r.staleMarks(t)
	through := refresherToday.AddDate(0, 0, FigureLead)
	for _, cut := range cuts {
		if mark, held := marks[cut]; !held || !slices.Contains(mark.evals, evalID) || mark.ready == nil || !mark.ready.Equal(through) {
			t.Fatalf("the move's commit left %s's cut %q marked %t at %s, and the check needs it to name %s with its first paint folded", cut.project, cut.repo, held, mark, evalID)
		}
	}
	after := r.storedFigures(t)
	for _, cut := range cuts {
		if _, held := after[dayFigureLine(cut.project, cut.repo, "", longPast, evalID)]; !held {
			t.Errorf("the move's commit deleted the stored day row of %s 400 days back in %s's cut %q: it read the cut's history", evalID, cut.project, cut.repo)
		}
	}
	paint := firstPaintOf(through)
	written := writtenFigures(before, after)
	if len(written) == 0 {
		t.Errorf("the move's commit wrote no stored row, and the check of what it wrote needs its first paints' rows")
	}
	for _, line := range written {
		cut, _, day, ranged := figureLineKey(t, line)
		_, marked := marks[cut]
		if !marked || ranged && !paint.holds(day) || !ranged && (day.Before(paint.days.From) || !day.Before(paint.days.To)) {
			t.Errorf("the move's commit wrote or deleted %s, no row of the first paint of a cut it marked", line)
		}
	}
	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the passes after the move settled with the cuts %v stale", marks)
	}
	expectTruth(t, "once the refresher settled", r.storedFigures(t), r.truthFigures(t))
}

// TestAWriteAfterTheFirstPaintKeepsItsStoredFiguresTheFoldOfTheRows: over movePierToQuay's move,
// whose commit folded the first paint of every cut it marked, and before any batch of their
// refold, acme/quay#1, a pull request whose rows the move moved into PIER's cut of acme/quay, is
// opened two hours earlier, and the page that reads the change writes its day rows (pageAlone):
// the write folds the figures of the days it moved in every cut its rows reach, the stale ones
// among them (foldFiguresInTx), so it rewrites stored rows of PIER's first paint, every stored row
// of every first paint is the fold of the rows that stand (expectPaintTruth), and a first paint of
// each cut states every eval from the stored figures, each the fold of its day rows. Red where a
// write folds the figures of its project's own rows alone, or leaves a stale cut to its refold:
// the first paint then states PIER's pr-open-to-merge from figures of the rows before the write.
func TestAWriteAfterTheFirstPaintKeepsItsStoredFiguresTheFoldOfTheRows(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
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

// TestANewDaysFirstPaintIsFoldedFirstAndReadOnceItIs: over movePierToQuay's move, whose commit
// folded the first paint of today's last end day of every cut it marked, a new UTC day comes and
// the figures walk folds the new last end day of every cut (walkFigures, alone of the pass): a read
// of a window ending on it, with a compare range one window back, no stored row of the old first
// paint, states the evals each mark names from their day rows. Each pass of the new day under an
// owed wake folds the new day's first paint of the next cut in the byte order of their keys, before
// any batch of a refold, and stops; those reads of each cut state every eval from the stored figures
// once its first paint is folded, and from the day rows before. Each read answers the fold of its
// day rows. Red where a mark whose first paint was folded on an earlier day owes none: the new
// day's reads then fold the day rows until the refold, newest end day first, has come down past
// their compare ranges.
func TestANewDaysFirstPaintIsFoldedFirstAndReadOnceItIs(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	marked := movePierToQuay(t, r)
	through := refresherToday.AddDate(0, 0, FigureLead)
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
	cuts := slices.SortedFunc(maps.Keys(marked), compareStaleCuts)
	r.expectReads(t, "before the new day's first paints", marked, reads, func(staleCut) bool { return false })
	for folded := range cuts {
		r.owedPass(t, &gapBatch{})
		marks := r.staleMarks(t)
		for index, cut := range cuts {
			want := through
			if index <= folded {
				want = next
			}
			if mark := marks[cut]; mark.ready == nil || !mark.ready.Equal(want) || mark.next != nil {
				t.Fatalf("pass %d of the new day left %s's cut %q marked at %s, want its first paint folded through %s, its refold to begin", folded+1, cut.project, cut.repo,
					mark, want.Format(time.DateOnly))
			}
		}
		r.expectReads(t, fmt.Sprintf("after pass %d of the new day", folded+1), marked, reads, func(cut staleCut) bool { return slices.Index(cuts, cut) <= folded })
	}
}

// TestTheBootFoldsTheFirstPaintOfEveryMarkBeforeItServes: over movePierToQuay's move, with each
// mark naming every eval as the boot's rename pass marks a cut (evals null), and its first paint
// owed, a read of a first paint of each cut states no eval from the stored figures. The boot's
// fold (FoldFirstPaintsAtBoot), with no process holding the refresher's lease, takes it and folds
// the first paint of every mark, every eval of each, waiting for no one: no mark owes a first paint
// once it returns, every stored row of every first paint is the fold of the rows that stand, and a
// first paint of each cut states every eval from the stored figures, each the fold of its day rows.
// Red where the boot folds nothing, or skips a mark that names every eval.
func TestTheBootFoldsTheFirstPaintOfEveryMarkBeforeItServes(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	r.exec(t, "name every eval of each mark and owe its first paint, as the rename pass marks a cut", `update eval_figure_cuts_stale set evals = null, ready_through = null`)
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
	marks := r.staleMarks(t)
	if cuts := slices.SortedFunc(maps.Keys(marks), compareStaleCuts); !slices.Equal(cuts, slices.SortedFunc(maps.Keys(every), compareStaleCuts)) {
		t.Fatalf("the boot left the cuts %v marked, want the %d the move marked", cuts, len(every))
	}
	for cut, mark := range marks {
		if mark.ready == nil || !mark.ready.Equal(through) || mark.evals != nil {
			t.Errorf("the boot left %s's cut of %s marked at %+v, want its first paint of every eval folded through %s", cut.project, cut.repo, mark, through.Format(time.DateOnly))
		}
	}
	expectPaintTruth(t, "after the boot", through, every, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the boot", every, firstPaintReads(through), func(staleCut) bool { return true })
}

// markReads is a tracer (storetest.TracedStore) that counts the reads of the stale marks a boot's
// fold makes on the pool it traces (owedFirstPaints, one at the start of each of its rounds), and
// closes the channel each after answered once that many more of them have begun.
type markReads struct {
	mu    sync.Mutex
	reads int
	waits []markWait
}

// markWait is one wait of markReads.after: the count of reads it waits for, and its channel.
type markWait struct {
	at      int
	reached chan struct{}
}

// after is a channel closed once n more reads of the marks than have begun so far have begun.
func (m *markReads) after(n int) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	reached := make(chan struct{})
	m.waits = append(m.waits, markWait{at: m.reads + n, reached: reached})
	return reached
}

func (m *markReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "from eval_figure_cuts_stale order by") {
		return ctx
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	m.waits = slices.DeleteFunc(m.waits, func(wait markWait) bool {
		if m.reads < wait.at {
			return false
		}
		close(wait.reached)
		return true
	})
	return ctx
}

func (m *markReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestABootWaitsForTheFirstPaintsTheProcessHoldingTheLeaseFolds: over movePierToQuay's move, its
// marks' first paints owed again, while another session holds the refresher's lease, as another
// process's refresher pass holds it, the boot's fold (FoldFirstPaintsAtBoot) neither folds nor
// skips: one the test cancels once it has read the marks a second time, a round of
// firstPaintBootWait after its first, answers the context's error, having waited, the first paints
// still owed; another, once it has read the marks a second time, has not answered, the first paints
// still owed, and once the holder folds them (FoldFirstPaint, as its pass folds them first) and
// gives the lease back, it answers, having waited and folded none itself, with no mark owing a
// first paint. Red where a boot that finds the lease held goes on at once: the server would then
// serve a first paint the fold of the day rows.
func TestABootWaitsForTheFirstPaintsTheProcessHoldingTheLeaseFolds(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	marked := movePierToQuay(t, r)
	r.exec(t, "owe each mark's first paint", `update eval_figure_cuts_stale set ready_through = null`)
	held, release, released := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	holder, waiter := storetest.Leases(t, pool), storetest.Leases(t, pool)
	go func() {
		var folded error
		duty.Exclusive(context.Background(), nil, holder, RefresherLease, time.Time{}, 0, func(ctx context.Context) {
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
	testwait.Receive(t, "the other session's take of the refresher's lease", held)
	owes := func(what string) {
		t.Helper()
		if owed, err := owedFirstPaints(ctx, pool); err != nil || len(owed) != len(marked) {
			t.Fatalf("%s: the marks %v owe a first paint (%v), want the %d marked", what, owed, err, len(marked))
		}
	}
	reads := &markReads{}
	booting := storetest.TracedStore(t, r.database, reads).Pool
	type answer struct {
		folded FirstPaintsAtBoot
		err    error
	}
	boot := func(ctx context.Context) <-chan answer {
		answered := make(chan answer, 1)
		go func() {
			folded, err := FoldFirstPaintsAtBoot(ctx, booting, waiter, r.build)
			answered <- answer{folded, err}
		}()
		return answered
	}
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	second := reads.after(2)
	answered := boot(cancelled)
	testwait.Receive(t, "the second read of the marks by the boot the test then cancels", second)
	cancel()
	got := testwait.Receive(t, "the answer of the boot the test cancelled", answered)
	if !errors.Is(got.err, context.Canceled) || !got.folded.Waited || got.folded.Marks != 0 {
		t.Fatalf("the boot's fold the test cancelled while another session held the lease answered %+v (%v), want a wait and no fold, ended by the cancel", got.folded, got.err)
	}
	owes("after the cancelled boot")
	second = reads.after(2)
	answered = boot(ctx)
	testwait.Receive(t, "the boot's second read of the marks while another session holds the lease", second)
	select {
	case got := <-answered:
		t.Fatalf("the boot's fold answered %+v (%v) while another session held the lease and the first paints were owed, want it to wait", got.folded, got.err)
	default:
	}
	owes("while the boot waits")
	free()
	if err := testwait.Receive(t, "the lease holder's fold of the first paints", released); err != nil {
		t.Fatalf("the lease holder's fold of the first paints: %v", err)
	}
	got = testwait.Receive(t, "the boot's answer once the lease holder folded the first paints and gave the lease back", answered)
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
// same just after the next midnight, a day later against the compare range its page kept, 7 days
// to today against the week before, and one window back, where a compare range of 84 or 85 days of
// those reads ends, the bench's 84 days against the 84 before, the live view's 85 against the 85
// before, and 85 days with no compare range; with no first paint and no refold, none; with the
// first paint alone, no window ending a day before the earliest a first paint names, no compare
// range no first paint names, and no window two windows back; each of those once the refold,
// newest end day first, has come down past its compare range's end and its window's first day, and
// none of them before; a first paint of an earlier day none of the later day's windows; and an eval
// no mark names every read. Red where the first paint holds the windows ending through-1 and
// through alone, which leaves the bench's first paint to the fold of the day rows; where it holds
// no range or day of a read one window back, which leaves those reads to the fold of the day rows
// until the refold has come down past them; where a read of the past is served before the refold
// has folded its ranges and days; and where a first paint of an earlier day answers the windows of
// a later one.
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
		{"84 days one window back", painted(through, nil), "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), true},
		{"the bench's first paint one window back", painted(through, nil), "", rangeOf(84, day(-84)), new(rangeOf(84, day(-168))), true},
		{"the live view one window back", painted(through, nil), "", rangeOf(85, day(-84)), new(rangeOf(85, day(-169))), true},
		{"85 days ending on a first paint's compare end, with no compare range", painted(through, nil), "", rangeOf(85, day(-83)), nil, true},
		{"84 days one window back with no first paint folded", builtCut{through: through, every: true}, "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), false},
		{"84 days one window back, the refold past both", painted(through, new(day(-168))), "", rangeOf(84, day(-83)), new(rangeOf(84, day(-167))), true},
		{"84 days two windows back", painted(through, nil), "", rangeOf(84, day(-167)), new(rangeOf(84, day(-251))), false},
		{"84 days two windows back, the refold past its window alone", painted(through, new(day(-184))), "", rangeOf(84, day(-167)), new(rangeOf(84, day(-251))), false},
		{"84 days two windows back, the refold past both", painted(through, new(day(-252))), "", rangeOf(84, day(-167)), new(rangeOf(84, day(-251))), true},
		{"85 days two windows back, with no compare range", painted(through, nil), "", rangeOf(85, day(-167)), nil, false},
		{"85 days two windows back, the refold past its first day", painted(through, new(day(-253))), "", rangeOf(85, day(-167)), nil, true},
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

// endsWritten is, by the transaction that wrote them (the xmin of their rows), the end days of the
// stored range rows of each cut a transaction wrote between before and after: what each
// transaction of a refold or of a figures walk folded, where every row it folds moved since before.
// A deleted row names no transaction and is left out.
func endsWritten(t *testing.T, before, after map[string]figureRow) map[string]map[staleCut]map[time.Time]bool {
	t.Helper()
	written := map[string]map[staleCut]map[time.Time]bool{}
	for _, line := range writtenFigures(before, after) {
		row, held := after[line]
		if !held || !strings.HasPrefix(line, "range ") {
			continue
		}
		cut, _, end, _ := figureLineKey(t, line)
		if written[row.xmin] == nil {
			written[row.xmin] = map[staleCut]map[time.Time]bool{}
		}
		if written[row.xmin][cut] == nil {
			written[row.xmin][cut] = map[time.Time]bool{}
		}
		written[row.xmin][cut][end] = true
	}
	return written
}

// endDaysText is days, in order, as a message names them.
func endDaysText(days map[time.Time]bool) string {
	var named []string
	for _, day := range slices.SortedFunc(maps.Keys(days), time.Time.Compare) {
		named = append(named, day.Format(time.DateOnly))
	}
	return strings.Join(named, " ")
}

// TestAFoldKeyMoveIsFoldedAgainInBatchesNoneOfThemTheWholeHistory: over the logged history
// (seedLoggedQuantile: EVAL's review-latency, with readings on the three days before today, of
// EVAL's own cut and of acme/web's), built whole, a register write folds the eval by its largest
// value, which moves its fold key, and one pass with batches of two end days runs: each transaction
// that wrote stored ranges of a cut wrote those of its first paint alone or those of two end days
// at most, no mark is left, and the stored figures are the fold of the rows that stand under the
// new key. Red where the figures' read of the change log folds a key move again in a transaction of
// its own: that transaction writes the ranges of every end day of the eval's history, five in each
// cut, two of them outside the first paint.
func TestAFoldKeyMoveIsFoldedAgainInBatchesNoneOfThemTheWholeHistory(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	r.batch = 2
	before := r.storedFigures(t)
	if _, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-latency", Caller: alice,
		Series: []SeriesChange{{Series: "minutes", SeriesComputationChange: SeriesComputationChange{Agg: Set(AggMax)}}}}); err != nil {
		t.Fatalf("fold review-latency by its largest value: %v", err)
	}
	r.pass(t)
	paint := firstPaintOf(refresherToday.AddDate(0, 0, FigureLead))
	written := endsWritten(t, before, r.storedFigures(t))
	if len(written) == 0 {
		t.Fatal("the pass after the key move wrote no stored range, and the check needs the eval's ranges folded again")
	}
	for xmin, byCut := range written {
		for cut, days := range byCut {
			painted := true
			for day := range days {
				painted = painted && paint.holds(day)
			}
			if !painted && len(days) > r.batch {
				t.Errorf("the pass after the key move wrote, in transaction %s, the stored ranges of %s's cut %q ending on %d end days (%s), neither its first paint's nor a batch's %d at most",
					xmin, cut.project, cut.repo, len(days), endDaysText(days), r.batch)
			}
		}
	}
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the pass after the key move left the cuts %v marked, want its refold done", marks)
	}
	expectTruth(t, "after the pass after the key move", r.storedFigures(t), r.truthFigures(t))
}

// TestEachCutsRefoldBeginsWithABatchOfOneEndDay: over the logged history (seedLoggedQuantile),
// built whole, both of EVAL's cuts, its own rows and acme/web's, are marked stale for every eval
// with their first paints folded, and every stored range of the project is put a second older, so
// the refold rewrites each end day it folds; a pass's refold, in batches of two end days and with
// no budget, then folds both cuts, the own cut first: in each cut the first batch, the one that
// wrote its last end day, wrote that end day alone, and each later one two end days at most, no
// mark is left, and the stored figures are the fold of the rows that stand. Red where a cut's refold
// begins at the size the refold of the cut before it reached: acme/web's first batch then writes
// its last two end days.
func TestEachCutsRefoldBeginsWithABatchOfOneEndDay(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	r.seedLoggedQuantile(t)
	r.settle(t)
	through := refresherToday.AddDate(0, 0, FigureLead)
	cuts := []staleCut{{project: r.project}, {project: r.project, repo: "acme/web"}}
	for _, cut := range cuts {
		r.exec(t, "mark a cut stale for every eval with its first paint folded", `insert into eval_figure_cuts_stale (project_key, repo, generation, ready_through, next_day, evals)
			values ($1, $2, 1, $3, null, null)`, cut.project, cut.repo, through)
	}
	r.exec(t, "put every stored range a second older", `update eval_range_figures set oldest_updated_at = oldest_updated_at - interval '1 second' where project_key = $1`, r.project)
	before := r.storedFigures(t)
	pass := r.registry.newRefresh(r.build, r.now, 2)
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	if left, err := pass.refoldStaleCuts(ctx); err != nil || left {
		t.Fatalf("the refold answered a mark left %t (%v), want both cuts folded", left, err)
	}
	written := endsWritten(t, before, r.storedFigures(t))
	for _, cut := range cuts {
		batches := 0
		for xmin, byCut := range written {
			days := byCut[cut]
			if len(days) == 0 {
				continue
			}
			batches++
			if days[through] && len(days) != 1 {
				t.Errorf("the first batch of the refold of %s's cut %q, transaction %s, wrote the stored ranges ending on %s, want its last end day %s alone",
					cut.project, cut.repo, xmin, endDaysText(days), through.Format(time.DateOnly))
			}
			if len(days) > pass.batch {
				t.Errorf("a batch of the refold of %s's cut %q, transaction %s, wrote the stored ranges ending on %s, more than a batch's %d end days",
					cut.project, cut.repo, xmin, endDaysText(days), pass.batch)
			}
		}
		if batches < 2 {
			t.Errorf("the refold of %s's cut %q wrote stored ranges in %d transactions, and the check needs its end days in more than one batch", cut.project, cut.repo, batches)
		}
	}
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the refold left the cuts %v marked, want none", marks)
	}
	expectTruth(t, "once the refold is done", r.storedFigures(t), r.truthFigures(t))
}

// TestAFiguresWalkBatchFoldsWhatItsBudgetHolds: over the logged history (seedLoggedQuantile), built
// whole, EVAL's figures walk is set back to its first end day, two days before today, and every
// stored range of the project from it on is put a second older, so the walk rewrites each end day
// it folds again; the pass's figures walk, under a budget no end day fits, then folds them one end
// day a transaction, each of the five end days from it through the last, has none left, and leaves
// the stored figures the fold of the rows that stand. Red where a batch of the walk folds every end
// day the gap's batch leaves it, however long that takes: one transaction then writes all five.
func TestAFiguresWalkBatchFoldsWhatItsBudgetHolds(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	r.seedLoggedQuantile(t)
	r.settle(t)
	first, through := refresherToday.AddDate(0, 0, -2), refresherToday.AddDate(0, 0, FigureLead)
	r.exec(t, "set EVAL's figures walk back to its first end day", `update eval_days_walks set next_day = $1 where family = $2 and project_key = $3`, first, walkFigures, r.project)
	r.exec(t, "put its stored ranges from that day on a second older", `update eval_range_figures set oldest_updated_at = oldest_updated_at - interval '1 second'
		where project_key = $1 and end_day >= $2`, r.project, first)
	before := r.storedFigures(t)
	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	pass.refoldBudget = time.Nanosecond
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	if left, err := pass.walkFigures(ctx); err != nil || left {
		t.Fatalf("the figures walk answered end days left %t (%v), want none", left, err)
	}
	ends := map[time.Time]bool{}
	for xmin, byCut := range endsWritten(t, before, r.storedFigures(t)) {
		days := map[time.Time]bool{}
		for _, of := range byCut {
			maps.Copy(days, of)
		}
		maps.Copy(ends, days)
		if len(days) > 1 {
			t.Errorf("a batch of the figures walk, transaction %s, wrote the stored ranges ending on %s, more than the one end day its budget holds", xmin, endDaysText(days))
		}
	}
	if want := rangeDays(Range{From: first, To: through}) + 1; len(ends) != want {
		t.Errorf("the figures walk wrote the stored ranges ending on %s, want the %d end days from %s through %s", endDaysText(ends), want, first.Format(time.DateOnly), through.Format(time.DateOnly))
	}
	expectTruth(t, "once the figures walk is done", r.storedFigures(t), r.truthFigures(t))
}

// remarkAtDelete is a tracer (storetest.TracedStore) that, at the first delete of a stale cut's
// mark sent on the pool it traces, marks that cut again on database, at its next generation with
// its first paint owed, as the boot's rename pass of another process may mark it between the read
// of the mark by the last batch of the cut's refold and that batch's delete.
type remarkAtDelete struct {
	database *store.Store
	once     sync.Once
	cut      staleCut
	marked   bool
	err      error
}

func (m *remarkAtDelete) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(strings.TrimSpace(data.SQL), "delete from eval_figure_cuts_stale") || len(data.Args) < 2 {
		return ctx
	}
	m.once.Do(func() {
		project, _ := data.Args[0].(string)
		repo, _ := data.Args[1].(string)
		m.cut, m.marked = staleCut{project: project, repo: repo}, true
		_, m.err = m.database.Pool.Exec(ctx, `update eval_figure_cuts_stale set generation = generation + 1, ready_through = null, next_day = null
			where project_key = $1 and repo = $2`, project, repo)
	})
	return ctx
}

func (m *remarkAtDelete) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestARefoldGoesOnWithACutMarkedAgainBeforeItsLastBatchCommits: over movePierToQuay's move, whose
// commit folded the first paint of every cut it marked, a pass's refold reaches the first end day
// of a cut, and before its batch deletes the cut's mark the cut is marked again, at its next
// generation with its first paint owed, as the boot's rename pass of another process marks it
// (remarkAtDelete): the delete, of the generation the batch read, takes no row, and the refold goes
// on with the cut, its first paint and every end day again, so it answers no mark left with none
// standing, and the stored figures are the fold of the rows that stand. Red where a batch that
// reached the cut's first end day reports the cut done whatever its delete took: the refold then
// answers no mark left while the cut's new mark stands, and no pass is asked for it.
func TestARefoldGoesOnWithACutMarkedAgainBeforeItsLastBatchCommits(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	movePierToQuay(t, r)
	remark := &remarkAtDelete{database: r.database}
	traced := New(storetest.TracedStore(t, r.database, remark), r.documents, r.registry.events, r.registry.attributor)
	pass := traced.newRefresh(r.build, r.now, r.batch)
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	left, err := pass.refoldStaleCuts(ctx)
	if err != nil || remark.err != nil {
		t.Fatalf("the refold failed (%v), or the second mark of a cut did (%v)", err, remark.err)
	}
	if !remark.marked {
		t.Fatal("no batch of the refold deleted a mark, and the check needs one to")
	}
	if marks := r.staleMarks(t); left || len(marks) != 0 {
		t.Errorf("the refold, %s's cut %q marked again before its last batch committed, answered a mark left %t with the marks %v standing, want none left and none standing",
			remark.cut.project, remark.cut.repo, left, marks)
	}
	expectTruth(t, "once the refold is done", r.storedFigures(t), r.truthFigures(t))
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
