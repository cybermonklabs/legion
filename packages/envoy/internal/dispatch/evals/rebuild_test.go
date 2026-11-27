package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// rebuild_test.go holds the checks of the restarts (dispatch://INFRA-2916): a release, a lost
// change cursor, and the delivery settings the rows stand built under gone, each rebuilt aside and
// flipped once (rebuild.go).

// release makes the rig's next passes a new build's, run by a process of its own: a release, which
// a process whose first pass finds the rows served by another build rebuilds.
func (r *refresherRig) release(build string) {
	r.build, r.seeds = build, &seeding{}
}

// loseChangeCursor moves the refresher's change cursor past the change log's head, a sequence that
// went backwards (delivery.CursorLost), as a log restored or reset under it leaves it.
func (r *refresherRig) loseChangeCursor(t *testing.T) {
	t.Helper()
	r.exec(t, "lose the change cursor", `update eval_days_cursor set position = position + 1000000 where name = $1`, changesCursor)
}

// rebuildNext is the first day the building rebuild's walk of family (project's) has not built,
// true, or, where no rebuild is building or it holds no such walk, the walk's record in the open,
// false: nil where it is to begin or has no record.
func (r *refresherRig) rebuildNext(t *testing.T, family, project string) (*time.Time, bool) {
	t.Helper()
	var next *time.Time
	var rebuilding bool
	if err := r.database.Pool.QueryRow(t.Context(), `
		with held as (
			select w.next_day from eval_rebuild_walks w join eval_rebuilds b on b.id = w.rebuild_id and b.state = 'building'
			where w.family = $1 and w.project_key = $2
		)
		select case when exists (select 1 from held) then (select next_day from held)
			else (select next_day from eval_days_walks where family = $1 and project_key = $2) end,
			exists (select 1 from held)
	`, family, project).Scan(&next, &rebuilding); err != nil {
		t.Fatalf("read the %s walk of %s: %v", family, project, err)
	}
	return next, rebuilding
}

// restartCases is the two restarts that are no change of an input: a release, and a lost change
// cursor. Each begins the restart; the pass after it reads it.
var restartCases = []struct {
	name  string
	begin func(t *testing.T, r *refresherRig)
}{
	{"a release", func(t *testing.T, r *refresherRig) { r.release("release-build") }},
	{"a lost change cursor", func(t *testing.T, r *refresherRig) { r.loseChangeCursor(t) }},
}

// writtenQuery reads the transaction that last wrote each stored day row, member and stored figure,
// by its key: a write moves its xmin, and a row it makes or deletes comes or goes.
const writtenQuery = `
	select 'day ' || project_key || ' ' || eval_id || ' ' || writer || ' ' || deploy_repo || ' ' || series || ' ' || repo || ' ' || day, xmin::text from eval_days
	union all
	select 'member ' || project_key || ' ' || family || ' ' || deploy_repo || ' ' || row_key, xmin::text from eval_day_members
	union all
	select 'range ' || project_key || ' ' || repo || ' ' || deploy_repo || ' ' || days || ' ' || end_day || ' ' || eval_id, xmin::text from eval_range_figures
	union all
	select 'figure ' || project_key || ' ' || repo || ' ' || deploy_repo || ' ' || day || ' ' || eval_id, xmin::text from eval_day_figures`

// movedKeys is the keys of before and after whose rows a write made, deleted or moved, the first
// ten, sorted, and how many there are.
func movedKeys(before, after map[string]string) ([]string, int) {
	var moved []string
	for key, xmin := range after {
		if was, held := before[key]; !held || was != xmin {
			moved = append(moved, key)
		}
	}
	for key := range before {
		if _, held := after[key]; !held {
			moved = append(moved, key)
		}
	}
	slices.Sort(moved)
	return moved[:min(len(moved), 10)], len(moved)
}

// allRepositorySeries is, of each of days, the `<eval> <series>` of every all-repositories row
// project holds of it, sorted.
func allRepositorySeries(rows map[string]DayRow, project string, days ...time.Time) map[time.Time][]string {
	held := map[time.Time][]string{}
	for _, row := range rows {
		if row.ProjectKey != project || row.Repo != "" || row.DeployRepo != "" || !slices.ContainsFunc(days, row.Day.Equal) {
			continue
		}
		held[row.Day] = append(held[row.Day], row.EvalID+" "+row.Series)
	}
	for day := range held {
		slices.Sort(held[day])
	}
	return held
}

// TestAChangeDuringARestartKeepsTheDaysZeroRows: a page that
// writes a day while a restart runs judges the day's zero rows by what the stored rows are whole
// through (eval_days_serving), not by the running build's walk. Over the settled Legion history of
// EVAL, a restart begins (a release; a lost change cursor) and its first pass ends, walking one day
// a pass, its rebuild building and short of the 3rd day; then EVAL-3's close moves from the 3rd day
// to the 7th, and the pages that read it, with no walk after them (pageAlone), write both days.
// Each still holds a row of every series it held before: the 3rd, which holds no input any more,
// its zero rows, its work time n 0; the 7th its zero rows beside EVAL-3's input. Before, the page
// judged zero rows by the walk begun again, which had computed neither day, and deleted them, so
// those series read "no data" until the walk came back. Once the passes settle, EVAL's Legion rows
// and figures are a whole walk's over the moved close.
func TestAChangeDuringARestartKeepsTheDaysZeroRows(t *testing.T) {
	t.Parallel()
	for _, restart := range restartCases {
		t.Run(restart.name, func(t *testing.T) {
			r := newRefresherRig(t)
			history := r.seedLegionEval(t)
			r.settle(t)
			r.batch = 1
			restart.begin(t, r)
			r.pass(t)
			left, joined := legionHistoryDay(3), legionHistoryDay(7)
			if next, rebuilding := r.rebuildNext(t, walkLegion, "EVAL"); !rebuilding || next == nil || next.After(left) {
				t.Fatalf("after the restart's first pass EVAL's Legion walk stands at %v, rebuilding %t, and the check needs a rebuild building that has not built %s",
					next, rebuilding, left.Format(time.DateOnly))
			}
			before := allRepositorySeries(r.dayRows(t), "EVAL", left, joined)
			if len(before[left]) == 0 || len(before[joined]) == 0 {
				t.Fatalf("EVAL holds the series %v, and the check needs rows on both days", before)
			}
			row := history.rows["EVAL-3"]
			row.ClosedAt = new(joined.Add(9 * time.Hour))
			history.rows["EVAL-3"] = row
			r.writeFacts(t, history, "EVAL-3")
			report := r.pageAlone(t)
			after := allRepositorySeries(r.dayRows(t), "EVAL", left, joined)
			for _, day := range []time.Time{left, joined} {
				if missing := slices.DeleteFunc(slices.Clone(before[day]), func(series string) bool { return slices.Contains(after[day], series) }); len(missing) > 0 {
					t.Errorf("the page wrote %s during the restart and left no row of %v there, which it held before: a zero row gone reads \"no data\"",
						day.Format(time.DateOnly), missing)
				}
			}
			if n := r.workTime(t)[left.Format(time.DateOnly)]; n != 0 {
				t.Errorf("EVAL's work time counts %d issues on %s, which EVAL-3 left, want 0", n, left.Format(time.DateOnly))
			}
			t.Logf("the page wrote %v; the day EVAL-3 left holds %v and the day it joined %v", report.sorted(), after[left], after[joined])

			r.settle(t)
			walk := newRefresherRig(t)
			walked := walk.seedLegionEval(t)
			walked.rows["EVAL-3"] = row
			walk.writeFacts(t, walked, "EVAL-3")
			walk.settle(t)
			r.expectSameLegionAsWalk(t, restart.name, walk)
		})
	}
}

// TestARestartOverStoredRowsServesThemUnchangedAndWritesNothing (07:59Z, 1): a restart over stored
// rows whose values its rebuild does not move serves them unchanged through the rebuild, writes
// none of them and logs no change, so no page gets a delta. Over the settled delivery and Legion
// histories, walked two days a pass, each restart (a release; a lost change cursor) runs pass after
// pass until it has nothing left to do, within 100 passes: after every pass a reader reads the
// stored day rows, members and figures it read before the restart, no row, member or figure was
// written (its xmin stands), the restart began one rebuild, which flipped, and the change log holds
// no eval_days change after the restart began. Before, a release rewrote every computed row whose
// built_by was another build's, and its figures walk folded every figure again in the open.
func TestARestartOverStoredRowsServesThemUnchangedAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, restart := range restartCases {
		t.Run(restart.name, func(t *testing.T) {
			r := newRefresherRig(t)
			r.batch = 2
			r.seedHistories(t)
			r.settle(t)
			r.pass(t)
			prints := &commitPrints{t: t, rig: r.rig}
			print, written := prints.read(), r.xmins(t, writtenQuery)
			seq := r.latestSeq(t)
			restart.begin(t, r)
			passes := 0
			for left := true; left; passes++ {
				if passes == 100 {
					t.Fatalf("100 passes of the restart left work to do, the rebuild of %q among it", r.pendingRebuild(t))
				}
				report, _ := r.pass(t)
				if got := prints.read(); got != print {
					moved, n := movedKeys(written, r.xmins(t, writtenQuery))
					t.Fatalf("after pass %d of the restart a reader reads other rows than before it: %d written, first %v", passes+1, n, moved)
				}
				left = report.walksLeft
			}
			if flipped := r.count(t, `select count(*) from eval_rebuilds where state = 'flipped'`); flipped != 1 || r.pendingRebuild(t) != "" {
				t.Errorf("the restart flipped %d rebuilds and left %q building, want the one rebuild it began, flipped", flipped, r.pendingRebuild(t))
			}
			if moved, n := movedKeys(written, r.xmins(t, writtenQuery)); n > 0 {
				t.Errorf("the restart wrote %d stored rows, members or figures whose values stood, first %v", n, moved)
			}
			if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
				t.Fatalf("drain the change log's outbox: %v", err)
			}
			if changes := r.evalDayChanges(t, seq); len(changes) > 0 {
				t.Errorf("the restart logged the eval_days changes %v, and no value moved", changes)
			}
			if stale := r.count(t, `select count(*) from eval_days where writer = 'computation' and built_by <> $1`, testBuild); stale != 0 {
				t.Errorf("%d computed rows whose values stood record another build than %s, the build that wrote their values", stale, testBuild)
			}
			t.Logf("%s: %d passes, every one reading the rows it read before, writing none and logging no change", restart.name, passes)
		})
	}
}

// corruptDays moves three days of the main project's pr-open-to-merge rows, a member of its
// delivery family and a day of its deploy-frequency rows away from what their input builds, as a
// build whose rules differ leaves them, and lets a pass fold the figures of the days they hold.
func (r *refresherRig) corruptDays(t *testing.T, project string) {
	t.Helper()
	r.exec(t, "move three days of pr-open-to-merge", `
		update eval_days set n = n + 5, sum = coalesce(sum, 0) + 5
		where project_key = $1 and eval_id = $2 and writer = 'computation' and repo = '' and day in (
			select distinct day from eval_days where project_key = $1 and eval_id = $2 and n > 0 order by day desc limit 3)
	`, project, EvalID(project, SlugPROpenToMerge))
	r.exec(t, "move a day of deploy-frequency", `
		update eval_days set n = n + 2, sum = coalesce(sum, 0) + 2
		where project_key = $1 and eval_id = $2 and writer = 'computation' and repo = '' and day = (
			select max(day) from eval_days where project_key = $1 and eval_id = $2 and n > 0)
	`, project, EvalID(project, SlugDeployFrequency))
	r.exec(t, "move a member", `
		update eval_day_members set repo = repo || '-moved'
		where (project_key, family, deploy_repo, row_key) = (
			select project_key, family, deploy_repo, row_key from eval_day_members where project_key = $1 and family = $2
			order by day desc, row_key limit 1)
	`, project, KindDelivery)
	r.pass(t)
}

// TestARebuildThatMovesValuesLandsInOneCommit (07:59Z, 1): a release whose rebuild moves values
// lands them as one delta, when the rebuild finishes. Over the settled delivery history, walked two
// days a pass, three days of pr-open-to-merge, a day of deploy-frequency and a member are moved
// away from what their input builds and their figures folded; then a new build's passes run until
// they have nothing left to do. They commit every day row, member and stored figure they write in
// one transaction; a reader between any two commits of them reads the rows before or the rows
// after, never a state between; the eval_days changes they log are one per eval, each with the
// rebuild's mark, the moved ones among them; and the database then holds the rows, members and
// figures of a second database that walked the same history whole (expectSameAsWalk). Before, the
// new build's walk wrote each day in a commit of its own.
func TestARebuildThatMovesValuesLandsInOneCommit(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRigByIssue(t, 2)
	main := deliveries.Project
	r.corruptDays(t, main)
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	seq := r.latestSeq(t)
	wrote := recordWrites(t, r.rig)
	prints := &commitPrints{t: t, rig: r.rig}
	before := prints.read()
	r.registry = New(storetest.TracedStore(t, r.database, prints), r.documents, r.registry.events, r.registry.attributor)
	r.release("release-build")
	passes, _ := r.settle(t)
	after := prints.read()

	transactions := wrote()
	if len(transactions) != 1 {
		t.Errorf("the restart wrote day rows, members or figures in %d transactions, want one: %v", len(transactions), transactions)
	}
	if before == after {
		t.Errorf("the restart left the rows as they stood, and its rebuild moves them")
	}
	states := map[string]bool{}
	for _, seen := range prints.seen {
		states[seen] = true
	}
	delete(states, before)
	delete(states, after)
	if len(states) > 0 {
		t.Errorf("a reader between two commits of the restart saw %d states that are neither the one before it nor the one after it", len(states))
	}
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", seq, err)
	}
	logged := map[string]int{}
	for _, one := range changes {
		if one.Kind != delivery.ChangeKindEvalDays || one.Ref == "" {
			continue
		}
		logged[one.Ref]++
		if one.RunKind != delivery.EvalDaysWrittenByRebuild {
			t.Errorf("the eval_days change of %s carries run_kind %q, want %q", one.Ref, one.RunKind, delivery.EvalDaysWrittenByRebuild)
		}
	}
	for _, evalID := range []string{EvalID(main, SlugPROpenToMerge), EvalID(main, SlugDeployFrequency)} {
		if logged[evalID] != 1 {
			t.Errorf("the restart logged %d eval_days changes of %s, whose rows it moved, want one", logged[evalID], evalID)
		}
	}
	for evalID, count := range logged {
		if count > 1 {
			t.Errorf("the restart logged %d eval_days changes of %s, want one", count, evalID)
		}
	}
	walk, _ := newSettingsRigByIssue(t, walkBatch)
	r.expectSameAsWalk(t, "the rebuild of a release", walk)
	t.Logf("%d passes wrote in the transactions %v, logged %v, and left %d states between %d commits",
		passes, transactions, logged, len(states), len(prints.seen))
}

// TestAFoldOnlyMoveMarksTheViews (07:59Z, 1): a release whose folds differ from the build before it
// and whose day rows do not move lands the figures it moves in its flip, which logs an eval_days
// change of each eval whose figures moved, with the rebuild's mark, so an evals view of them is
// recomputed. Over the settled delivery and Legion histories, one eval's stored range figures are
// folded otherwise, as a build of other folds leaves them; a new build's passes then run until they
// have nothing left to do. The stored figures are then the fold of the rows, the eval logged one
// eval_days change after the release began, with the rebuild's mark, and no day row moved. Before,
// the new build's figures walk rewrote them in the open and logged nothing.
func TestAFoldOnlyMoveMarksTheViews(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	other := EvalID(deliveries.Project, SlugDeployFrequency)
	r.exec(t, "fold one eval's figures otherwise", `update eval_range_figures set figures = '\x00'::bytea where eval_id = $1`, other)
	rows := r.dayRows(t)
	seq := r.latestSeq(t)
	r.release("release-build")
	r.settle(t)
	expectTruth(t, "after the release", r.storedFigures(t), r.truthFigures(t))
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", seq, err)
	}
	var marked []string
	for _, one := range changes {
		if one.Kind == delivery.ChangeKindEvalDays && one.Ref == other {
			marked = append(marked, one.RunKind)
		}
	}
	if !slices.Equal(marked, []string{delivery.EvalDaysWrittenByRebuild}) {
		t.Errorf("the release logged the eval_days changes %q of %s, whose figures it moved, want one with the rebuild's mark", marked, other)
	}
	values := func(rows map[string]DayRow) map[string]DayRow {
		kept := map[string]DayRow{}
		for key, row := range rows {
			row.BuiltBy, row.UpdatedAt = "", time.Time{}
			kept[key] = row
		}
		return kept
	}
	if differ := sameValues(values(r.dayRows(t)), values(rows), rowFacts); len(differ) > 0 {
		t.Errorf("the release moved day rows whose input stood, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	t.Logf("the release logged %q of %s and moved no day row", marked, other)
}

// pendingRebuild is the rebuild that is building, by its build, and the empty string where none is.
func (r *refresherRig) pendingRebuild(t *testing.T) string {
	t.Helper()
	var build string
	if err := r.database.Pool.QueryRow(t.Context(), `select coalesce((select built_by from eval_rebuilds where state = 'building'), '')`).Scan(&build); err != nil {
		t.Fatalf("read the rebuild that is building: %v", err)
	}
	return build
}

// staged is how many staged rows the rebuilds hold, of every staging table.
func (r *refresherRig) staged(t *testing.T) int {
	t.Helper()
	return r.count(t, `select (select count(*) from eval_rebuild_days) + (select count(*) from eval_rebuild_members)
		+ (select count(*) from eval_rebuild_range_figures) + (select count(*) from eval_rebuild_day_figures)`)
}

// stagedMerges is each day of project's delivery rows the rebuild that is building holds staged,
// with the first pull request the day counts (its delivery member under no deploy repository),
// `<repo>#<number>`, in day order.
func (r *refresherRig) stagedMerges(t *testing.T, project string) map[time.Time]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select s.day, min(m.row_key) from eval_rebuild_days s
		join eval_rebuilds b on b.id = s.rebuild_id and b.state = 'building'
		join eval_day_members m on m.project_key = s.project_key and m.family = $2 and m.deploy_repo = '' and m.day = s.day
			and m.row_key like 'pull_request:%'
		where s.project_key = $1 group by s.day`, project, KindDelivery)
	if err != nil {
		t.Fatalf("read the staged days of %s: %v", project, err)
	}
	days := map[time.Time]string{}
	var day time.Time
	var member string
	if _, err := pgx.ForEachRow(rows, []any{&day, &member}, func() error {
		days[day.UTC()] = strings.TrimPrefix(member, "pull_request:")
		return nil
	}); err != nil {
		t.Fatalf("read the staged days of %s: %v", project, err)
	}
	return days
}

// TestAChangeDuringARebuildIsNeitherLostNorOverwritten (08:28Z (a)): a write in the open during a
// release's rebuild lands live, and the triggers of the day rows queue each day it touched that the
// rebuild had built, whatever process wrote it, so the flip's pass builds them again before the
// flip. Over the settled delivery history, walked two days a pass, the main project's rows are
// moved (corruptDays) and a new build's rebuild runs until it holds the staged rows of a moved day;
// then that day is written in the open: the merge of one of its pull requests moves an hour and
// the pages of a pass of the new build, or of the build before it, which still runs, write it
// (pageAlone); or a process that queues nothing writes its rows with plain SQL, as an older build's
// would. Right after the write the day's stored pr-open-to-merge rows hold what it wrote, the
// rebuild is still building and the day is queued; once the rebuild has flipped the database holds
// what a second database holds that walked the same history, with the same merge where one moved,
// whole: the change is not lost, and no stale staged row overwrote it.
func TestAChangeDuringARebuildIsNeitherLostNorOverwritten(t *testing.T) {
	t.Parallel()
	for _, writer := range []string{"the rebuild's own build", "the build before it", "a process that queues nothing"} {
		t.Run(writer, func(t *testing.T) {
			r, deliveries := newSettingsRigByIssue(t, 2)
			main := deliveries.Project
			r.corruptDays(t, main)
			old := *r
			r.release("release-build")
			var day time.Time
			var pr string
			for passes := 0; day.IsZero(); passes++ {
				r.pass(t)
				staged := r.stagedMerges(t, main)
				if len(staged) > 0 {
					day = slices.MaxFunc(slices.Collect(maps.Keys(staged)), time.Time.Compare)
					pr = staged[day]
				} else if passes > 50 || r.pendingRebuild(t) == "" {
					t.Fatalf("the new build's rebuild staged no row of %s on a day that counts a pull request", main)
				}
			}
			// stored is the day's stored pr-open-to-merge rows of the main project, of every
			// repository.
			stored := func() []string {
				var facts []string
				for _, row := range r.dayRows(t) {
					if row.ProjectKey == main && row.EvalID == EvalID(main, SlugPROpenToMerge) && row.DeployRepo == "" && row.Day.Equal(day) {
						facts = append(facts, rowFacts(row))
					}
				}
				slices.Sort(facts)
				return facts
			}
			stood := stored()
			switch writer {
			case "a process that queues nothing":
				r.exec(t, "write the day's rows as an older build would", `
					update eval_days set n = n + 3, sum = coalesce(sum, 0) + 3, updated_at = now()
					where project_key = $1 and eval_id = $2 and writer = 'computation' and repo = '' and day = $3
				`, main, EvalID(main, SlugPROpenToMerge), day)
			case "the build before it":
				r.moveMerge(t, pr)
				old.pageAlone(t)
			default:
				r.moveMerge(t, pr)
				r.pageAlone(t)
			}
			if live := stored(); slices.Equal(live, stood) {
				t.Errorf("%s wrote %s during the rebuild, and its stored rows stand as they stood, %v: the write did not land live", writer, day.Format(time.DateOnly), live)
			}
			if pending := r.pendingRebuild(t); pending != r.build {
				t.Errorf("as %s wrote %s, the rebuild building is %q's, want the release's, %s", writer, day.Format(time.DateOnly), pending, r.build)
			}
			if queued := r.count(t, `select count(*) from eval_rebuild_reverify where project_key = $1 and family = $2 and day = $3`,
				main, walkDelivery, day); queued == 0 {
				t.Errorf("the write of %s by %s during the rebuild queued it for no build again", day.Format(time.DateOnly), writer)
			}
			r.settle(t)
			if pending := r.pendingRebuild(t); pending != "" {
				t.Errorf("the rebuild of %s is still building after the passes settled", pending)
			}
			walk, _ := newSettingsHistory(t, walkBatch)
			walk.leaveInputsUnread(t)
			if writer != "a process that queues nothing" {
				walk.moveMerge(t, pr)
			}
			walk.settle(t)
			r.expectSameAsWalk(t, writer, walk)
			t.Logf("%s wrote %s (pull request %s) during the rebuild; the flip left what a whole walk holds", writer, day.Format(time.DateOnly), pr)
		})
	}
}

// TestAFreshDatabasesFirstWalkCommitsDayByDay: a database no rows are served of has nothing to keep
// serving, so its first walk builds every family in the open, a day a transaction, each day
// readable once written and "no data" before (k12's rule), and stages nothing: no rebuild begins.
// Over the delivery and Legion histories with their pull requests' attribution inputs read, so the
// delivery days hold rows of projects, of plain owner keys and of owner keys that carry lists
// (owners.go), and a project reads its own rows with both kinds of key, walked two days a pass:
// every transaction that writes a day row writes the rows of one day; every delivery and CI
// eval's rows of a day, of every key that holds one, are written by one transaction, so no read
// between two commits folds some of the keys a project reads of the day and not the others; that
// transaction moves the family's serving record past the day, as each Legion day's moves its
// project's; the first pass leaves rows on the walk's first two days and none after them, the
// walk and the serving record standing after them; and no rebuild and no staged row exists. Red
// where a day is committed one owner key at a time, or the serving record moves in a commit of its
// own.
func TestAFreshDatabasesFirstWalkCommitsDayByDay(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	trace := &dayCommits{open: map[*pgx.Conn]int{}, writing: map[*pgx.Conn]bool{}}
	r.registry = New(storetest.TracedStore(t, r.database, trace), r.documents, r.registry.events, r.registry.attributor)
	for _, statement := range []string{
		`create table evals_test_days (txid bigint not null, project_key text not null, eval_id text not null, day date not null)`,
		`create function evals_test_record_day() returns trigger language plpgsql as $$
		begin
			insert into evals_test_days (txid, project_key, eval_id, day)
			values (txid_current(), coalesce(new.project_key, old.project_key), coalesce(new.eval_id, old.eval_id), coalesce(new.day, old.day));
			return null;
		end $$`,
		`create constraint trigger evals_test_record_day after insert or update or delete on eval_days
			deferrable initially deferred for each row execute function evals_test_record_day()`,
		`create table evals_test_serving (txid bigint not null, family text not null, project_key text not null, next_day date not null)`,
		`create function evals_test_record_serving() returns trigger language plpgsql as $$
		begin
			insert into evals_test_serving (txid, family, project_key, next_day) values (txid_current(), new.family, new.project_key, new.next_day);
			return null;
		end $$`,
		`create trigger evals_test_record_serving after insert or update on eval_days_serving
			for each row execute function evals_test_record_serving()`,
	} {
		r.exec(t, "record what each transaction writes", statement)
	}
	// slug is a recorded row's slug, read off its eval id under its key.
	const slug = `substr(eval_id, length(project_key) + 2)`
	r.pass(t)
	var first, last *time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(day), max(day) from evals_test_days where `+slug+` = any($1)`,
		deliveryDaySlugs).Scan(&first, &last); err != nil || first == nil {
		t.Fatalf("read the first pass's delivery days: %v (%v)", err, first)
	}
	if last.Sub(*first) > 24*time.Hour {
		t.Errorf("the first pass wrote delivery rows from %s to %s, want its walk's first two days alone", first.Format(time.DateOnly), last.Format(time.DateOnly))
	}
	if next, _ := r.rebuildNext(t, walkDelivery, ""); next == nil || !next.Equal(first.AddDate(0, 0, 2)) {
		t.Errorf("the delivery walk stands at %v after its first batch, want %s", next, first.AddDate(0, 0, 2).Format(time.DateOnly))
	}
	if served := r.count(t, `select count(*) from eval_days_serving where family = $1 and next_day = $2`, walkDelivery, first.AddDate(0, 0, 2)); served != 1 {
		t.Errorf("the delivery rows are served through %d records at the walk's day, want one", served)
	}
	r.settle(t)

	// The check needs a project that reads its own rows with a plain owner key's and with a key's
	// that carries lists, of the days the walk wrote.
	keys, err := r.database.Pool.Query(t.Context(), `select distinct project_key from evals_test_days where `+slug+` = any($1)`, deliveryDaySlugs)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	var key string
	if _, err := pgx.ForEachRow(keys, []any{&key}, func() error {
		if _, _, lists, ok := parseOwner(key); ok {
			kinds[key] = "plain"
			if len(lists) > 0 {
				kinds[key] = "lists"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reads := map[string]map[string]int{}
	for owner, projects := range r.ownerReaders(t) {
		for _, project := range projects {
			if kind := kinds[owner]; kind != "" {
				if reads[project] == nil {
					reads[project] = map[string]int{}
				}
				reads[project][kind]++
			}
		}
	}
	reader := ""
	for _, project := range slices.Sorted(maps.Keys(reads)) {
		if reads[project]["plain"] > 0 && reads[project]["lists"] > 0 {
			reader = project
			break
		}
	}
	if reader == "" {
		t.Fatalf("no project reads both a plain owner key and a key that carries lists of the walked days (%v), and the check needs one", reads)
	}

	if days := r.count(t, `select count(distinct (`+slug+`, day)) from evals_test_days where `+slug+` = any($1)`, deliveryDaySlugs); days == 0 {
		t.Fatal("the walk recorded no delivery day, and the check needs its days")
	}
	if wide := r.count(t, `select count(*) from (select txid from evals_test_days group by txid having count(distinct day) > 1) wide`); wide != 0 {
		t.Errorf("%d transactions wrote the day rows of more than one day, want each a day's", wide)
	}
	split, err := r.database.Pool.Query(t.Context(), `
		select `+slug+`, day, count(distinct project_key), count(distinct txid) from evals_test_days
		where `+slug+` = any($1) group by 1, 2 having count(distinct txid) > 1 order by 2, 1`, deliveryDaySlugs)
	if err != nil {
		t.Fatal(err)
	}
	var evalSlug string
	var day time.Time
	var held, commits int
	splits := 0
	if _, err := pgx.ForEachRow(split, []any{&evalSlug, &day, &held, &commits}, func() error {
		if splits++; splits <= 5 {
			t.Errorf("%s's rows of %s, of %d keys, were written by %d transactions, want one: a read between them folds some of the keys alone",
				evalSlug, day.Format(time.DateOnly), held, commits)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if unserved := r.count(t, `
		with written as (
			select distinct txid, day,
				case when `+slug+` = any($1::text[]) then $3::text else $4::text end as family,
				case when `+slug+` = any($1::text[]) then '' else project_key end as walk
			from evals_test_days where `+slug+` = any($1::text[] || $2::text[])
		)
		select count(*) from written w where not exists (
			select 1 from evals_test_serving s where s.txid = w.txid and s.family = w.family and s.project_key = w.walk and s.next_day = w.day + 1)
	`, deliveryDaySlugs, legionDaySlugs, walkDelivery, walkLegion); unserved != 0 {
		t.Errorf("%d transactions wrote a walk's day and did not move its serving record past it, want none", unserved)
	}
	if rebuilds := r.count(t, `select count(*) from eval_rebuilds`); rebuilds != 0 || r.staged(t) != 0 {
		t.Errorf("the first walk began %d rebuilds and left %d staged rows, want none", rebuilds, r.staged(t))
	}
	t.Logf("the first pass wrote %s to %s; %d transactions wrote day rows, each of one day, %d delivery (eval, day)s of more than one; %s reads %d plain owner keys and %d keys that carry lists",
		first.Format(time.DateOnly), last.Format(time.DateOnly), r.count(t, `select count(distinct txid) from evals_test_days`), splits,
		reader, reads[reader]["plain"], reads[reader]["lists"])
	t.Logf("the longest day commit wrote %d rows and sent %d statements between its begin and its commit, of %d commits that wrote day rows",
		r.count(t, `select coalesce(max(n), 0) from (select count(*) as n from evals_test_days group by txid) one`), trace.most, trace.commits)
}

// dayCommits is a tracer (storetest.TracedStore) that counts, on the pool it traces, the
// statements of each transaction from its begin to its commit, and of those that wrote day rows
// (inserted, updated or deleted one) how many committed and the most statements one sent.
type dayCommits struct {
	mu            sync.Mutex
	open          map[*pgx.Conn]int
	writing       map[*pgx.Conn]bool
	commits, most int
}

func (d *dayCommits) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.ToLower(strings.TrimSpace(data.SQL))
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasPrefix(statement, "begin"):
		d.open[conn], d.writing[conn] = 0, false
	case statement == "commit":
		if d.writing[conn] {
			d.commits++
			d.most = max(d.most, d.open[conn])
		}
		delete(d.open, conn)
		delete(d.writing, conn)
	case statement == "rollback":
		delete(d.open, conn)
		delete(d.writing, conn)
	default:
		if _, in := d.open[conn]; in {
			d.open[conn]++
			d.writing[conn] = d.writing[conn] || strings.Contains(statement, "insert into eval_days (") ||
				strings.Contains(statement, "update eval_days ") || strings.Contains(statement, "delete from eval_days ")
		}
	}
	return ctx
}

func (d *dayCommits) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// moveMerge moves the merge of the pull request pr (`<repo>#<number>`) an hour, within its day.
func (r *refresherRig) moveMerge(t *testing.T, pr string) {
	t.Helper()
	repo, number := memberPR(t, "pull_request:"+pr)
	r.exec(t, "move the merge of "+pr, `
		update delivery_pull_requests set merged_at = case
			when merged_at < date_trunc('day', merged_at at time zone 'UTC') at time zone 'UTC' + interval '12 hours'
			then merged_at + interval '1 hour' else merged_at - interval '1 hour' end
		where repo = $1 and number = $2`, repo, number)
}

// buildingRebuild is the id of the rebuild that is building, 0 for none, and the next day of each of
// its walks by `<family> <project>`, "" for a walk that is to begin.
func (r *refresherRig) buildingRebuild(t *testing.T) (int64, map[string]string) {
	t.Helper()
	var id int64
	if err := r.database.Pool.QueryRow(t.Context(), `select coalesce((select id from eval_rebuilds where state = 'building'), 0)`).Scan(&id); err != nil {
		t.Fatalf("read the rebuild that is building: %v", err)
	}
	rows, err := r.database.Pool.Query(t.Context(), `
		select family || ' ' || project_key, coalesce(to_char(next_day, 'YYYY-MM-DD'), '') from eval_rebuild_walks where rebuild_id = $1`, id)
	if err != nil {
		t.Fatalf("read the walks of rebuild %d: %v", id, err)
	}
	walks := map[string]string{}
	var walk, next string
	if _, err := pgx.ForEachRow(rows, []any{&walk, &next}, func() error {
		walks[walk] = next
		return nil
	}); err != nil {
		t.Fatalf("read the walks of rebuild %d: %v", id, err)
	}
	return id, walks
}

// stagedPrint is one fingerprint of every staged day row and member of every rebuild.
func (r *refresherRig) stagedPrint(t *testing.T) string {
	t.Helper()
	var print string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select md5(coalesce((select string_agg(md5(s::text), '' order by md5(s::text)) from eval_rebuild_days s), '') || '|' ||
			coalesce((select string_agg(md5(s::text), '' order by md5(s::text)) from eval_rebuild_members s), ''))
	`).Scan(&print); err != nil {
		t.Fatalf("read the fingerprint of the staged rows: %v", err)
	}
	return print
}

// TestARebuildResumesAfterACrash: each batch of a rebuild stages its rows and its progress in one
// transaction, so a pass that fails mid-rebuild leaves the batch before it as it stood, and the
// passes after it go on from there. Over the settled, moved delivery history (corruptDays), a new
// build's rebuild stages a batch; every key's pr-open-to-merge rows of the days of the next batch
// are moved too, so it stages rows, and its staging fails once at its progress: that pass
// leaves the same rebuild building with each of its walks and its staged rows and members where
// the batch before it left them; the passes then run to the flip: that one rebuild, the only one
// the table holds, flipped, no staged row left, and the rows, members and figures a whole walk
// holds. Red where the failed pass, or one after it, abandons the rebuild and begins another, or
// where a batch's staged rows commit apart from its progress.
func TestARebuildResumesAfterACrash(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, 2)
	main := deliveries.Project
	r.corruptDays(t, main)
	r.release("release-build")
	r.pass(t)
	rebuild, walks := r.buildingRebuild(t)
	next, _ := r.rebuildNext(t, walkDelivery, "")
	if rebuild == 0 || next == nil {
		t.Fatalf("the release's first pass left rebuild %d building with walks %v, want one that staged a batch", rebuild, walks)
	}
	if moved := r.count(t, `
		with moved as (
			update eval_days set n = n + 1 where writer = 'computation' and substr(eval_id, length(project_key) + 2) = $1 and day >= $2 and day < $3 returning 1
		) select count(*) from moved
	`, SlugPROpenToMerge, *next, next.AddDate(0, 0, r.batch)); moved == 0 {
		t.Fatalf("no key holds a pr-open-to-merge row of the next batch's days from %s, and the check needs its staging to hold rows", next.Format(time.DateOnly))
	}
	staged := r.stagedPrint(t)
	r.exec(t, "count the failures", `create sequence evals_test_failures`)
	r.exec(t, "fail one staging", `create function evals_test_fail_once() returns trigger language plpgsql as $$
		begin
			if nextval('evals_test_failures') = 1 then
				raise exception 'the test fails this staging once';
			end if;
			return new;
		end $$`)
	r.exec(t, "fail one staging", `create trigger evals_test_fail_once before insert on eval_rebuild_walks
		for each row execute function evals_test_fail_once()`)
	r.exec(t, "fail one staging", `create trigger evals_test_fail_progress before update on eval_rebuild_walks
		for each row execute function evals_test_fail_once()`)
	r.failingPass(t)
	if after, afterWalks := r.buildingRebuild(t); after != rebuild || !maps.Equal(afterWalks, walks) {
		t.Errorf("the failed pass left rebuild %d building with walks %v, want rebuild %d with %v as the batch before it left them",
			after, afterWalks, rebuild, walks)
	}
	if r.stagedPrint(t) != staged {
		t.Errorf("the failed pass left other staged rows or members than the batch before it left: its batch's staging committed without its progress")
	}
	r.settle(t)
	if rebuilds := r.count(t, `select count(*) from eval_rebuilds`); rebuilds != 1 {
		t.Errorf("eval_rebuilds holds %d rebuilds, want the one that resumed", rebuilds)
	}
	if flipped := r.count(t, `select count(*) from eval_rebuilds where id = $1 and state = 'flipped' and built_by = $2`, rebuild, r.build); flipped != 1 {
		t.Errorf("rebuild %d of %s did not flip", rebuild, r.build)
	}
	if staged := r.staged(t); staged != 0 {
		t.Errorf("%d staged rows are left after the flip", staged)
	}
	walk, _ := newSettingsRig(t, walkBatch)
	r.expectSameAsWalk(t, "a rebuild that resumed after a crash", walk)
}

// rebuildStatements is a tracer that records, on the pool it traces, which of the flip's statements
// (flipStatements, by their text) ran and the lists any of them sent, and the arguments of every
// other statement that reads or writes a rebuild's staging, those that stage members apart too.
type rebuildStatements struct {
	mu      sync.Mutex
	staging [][]any
	members [][]any
	flip    []string
	arrays  []string
}

func (s *rebuildStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flip := slices.IndexFunc(flipStatements, func(one struct{ what, sql string }) bool { return one.sql == data.SQL }); flip >= 0 {
		s.flip = append(s.flip, flipStatements[flip].what)
		for _, arg := range data.Args {
			if value := reflect.ValueOf(arg); value.Kind() == reflect.Slice && value.Type().Elem().Kind() != reflect.Uint8 {
				s.arrays = append(s.arrays, fmt.Sprintf("%s: %T", flipStatements[flip].what, arg))
			}
		}
		return ctx
	}
	if !strings.Contains(data.SQL, "eval_rebuild") {
		return ctx
	}
	s.staging = append(s.staging, slices.Clone(data.Args))
	if strings.Contains(data.SQL, "insert into eval_rebuild_members") {
		s.members = append(s.members, slices.Clone(data.Args))
	}
	return ctx
}

func (s *rebuildStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// argBytes is at least the bytes args take as pgx encodes them in binary: 4 of length before each,
// a string's or a byte list's bytes, 8 for a number or a moment, and for a list its header and each
// element after 4 bytes of length.
func argBytes(args []any) int {
	bytes := 0
	var one func(value reflect.Value) int
	one = func(value reflect.Value) int {
		switch value.Kind() {
		case reflect.Pointer:
			if value.IsNil() {
				return 0
			}
			return one(value.Elem())
		case reflect.String:
			return value.Len()
		case reflect.Slice:
			if value.Type().Elem().Kind() == reflect.Uint8 {
				return value.Len()
			}
			size := arrayHeaderBytes
			for i := range value.Len() {
				size += 4 + one(value.Index(i))
			}
			return size
		default:
			return 8
		}
	}
	for _, arg := range args {
		if arg == nil {
			bytes += 4
			continue
		}
		bytes += 4 + one(reflect.ValueOf(arg))
	}
	return bytes
}

// TestTheFlipSendsNoArrayAndTheStagingStaysUnderTheBound: the flip is set-based statements that
// read the staging by the rebuild's id, with no list a client sends, and every statement that
// stages rows sends at most statementBytes of parameters, a staging past it cut into several. Over
// the settled delivery history in which 3,000 more pull requests of the deploy repository merged
// on its last day, its attribution inputs left unread, the main project's rows moved (corruptDays)
// and every member of the last day moved to another repository, a new build's rebuild runs to its
// flip on a traced pool: every statement of the flip ran and took no array, every other statement
// of the staging sent at most statementBytes, and the members it staged, more than twice
// statementBytes of them, went in at least three statements.
func TestTheFlipSendsNoArrayAndTheStagingStaysUnderTheBound(t *testing.T) {
	t.Parallel()
	const copies = 3000
	r, deliveries := newSettingsHistory(t, walkBatch)
	crowdLastDay(t, r, deliveries, copies)
	r.leaveInputsUnread(t)
	r.settle(t)
	r.corruptDays(t, deliveries.Project)
	r.exec(t, "move every member of the last day", `update eval_day_members set repo = repo || '-moved' where project_key = $1 and day = $2`,
		deliveries.Project, deliveries.To.AddDate(0, 0, -1))
	statements := &rebuildStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	r.release("release-build")
	r.settle(t)
	if len(statements.flip) != len(flipStatements) {
		t.Fatalf("the flip ran the statements %v, want each of its %d once", statements.flip, len(flipStatements))
	}
	if len(statements.arrays) > 0 {
		t.Errorf("the flip's statements sent the lists %v", statements.arrays)
	}
	largest := 0
	for _, args := range statements.staging {
		largest = max(largest, argBytes(args))
	}
	if largest > statementBytes {
		t.Errorf("a statement of the staging sent %d bytes of parameters, more than statementBytes, %d", largest, statementBytes)
	}
	members := 0
	for _, args := range statements.members {
		members += argBytes(args)
	}
	if members <= 2*statementBytes || len(statements.members) < 3 {
		t.Errorf("the staging sent members in %d statements of %d bytes of parameters together, and the check needs more than %d in at least three",
			len(statements.members), members, 2*statementBytes)
	}
	t.Logf("the flip's %d statements sent no list; the largest of the staging's %d statements sent %d bytes, its members %d bytes in %d statements",
		len(statements.flip), len(statements.staging), largest, members, len(statements.members))
}

// TestTwoBuildsRollingTogetherFlipOnce (07:59Z, 1; a rolling deploy): while two builds' refreshers
// share a database, passes taken in turn, the new build rebuilds and flips once, and the build
// before it, which served the rows when its process began, begins no rebuild of its own, before
// the flip or after it. Over the settled delivery and Legion histories, a new build's passes and
// the old build's alternate until the new build's rebuild has flipped, then twenty more: one
// rebuild flipped, the new build's, no rebuild building, and every stored family served by it.
// Before, each build's passes continued their own walks, and a roll rewrote every row twice.
func TestTwoBuildsRollingTogetherFlipOnce(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	newer := *r
	newer.release("newer-build")
	passes := 0
	for flipped := 0; passes < 200 && flipped < 20; passes++ {
		rig := &newer
		if passes%2 == 1 {
			rig = r
		}
		rig.pass(t)
		if r.count(t, `select count(*) from eval_rebuilds where state = 'flipped'`) > 0 {
			flipped++
		}
	}
	if flips := r.count(t, `select count(*) from eval_rebuilds where state = 'flipped'`); flips != 1 {
		t.Errorf("the two builds flipped %d rebuilds, want one", flips)
	}
	if other := r.count(t, `select count(*) from eval_rebuilds where built_by <> $1`, newer.build); other != 0 {
		t.Errorf("the build before the new one began %d rebuilds, want none", other)
	}
	if building := r.pendingRebuild(t); building != "" {
		t.Errorf("a rebuild of %s is building after the flip", building)
	}
	if served := r.count(t, `select count(*) from eval_days_serving where built_by <> $1`, newer.build); served != 0 {
		t.Errorf("%d families are served by another build than %s after its flip", served, newer.build)
	}
	t.Logf("in %d passes taken in turn, one rebuild flipped, %s's", passes, newer.build)
}

// flippedBuilds is the build of each rebuild that flipped, in the order they began.
func (r *refresherRig) flippedBuilds(t *testing.T) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select built_by from eval_rebuilds where state = 'flipped' order by id`)
	if err != nil {
		t.Fatalf("read the rebuilds that flipped: %v", err)
	}
	builds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the rebuilds that flipped: %v", err)
	}
	return builds
}

// TestANewerBuildRebuildsOnceAnOlderReleasesRebuildHasFlipped (07:59Z, 1; two releases rolling at
// once): a process whose first pass finds another build's rebuild building, an older release's
// still rolling out, waits for it and then rebuilds the rows itself, so the newest build serves
// them. Over the settled delivery and Legion histories, walked two days a pass, an older build's
// first pass begins its release's rebuild, and a newer build's first pass finds it building; their
// passes then alternate until no rebuild is building and neither has work left: two rebuilds
// flipped, the older build's and then the newer's, and every family is served by the newer build.
// Before, the newer build stopped wanting its rebuild once the older one flipped, a build its first
// pass had not found serving, and the rows stayed the older build's.
func TestANewerBuildRebuildsOnceAnOlderReleasesRebuildHasFlipped(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	older, newer := *r, *r
	older.release("older-build")
	older.pass(t)
	newer.release("newer-build")
	newer.pass(t)
	if building := r.pendingRebuild(t); building != older.build {
		t.Fatalf("the newer build's first pass found %q building, and the check needs the older build's release building", building)
	}
	passes := 0
	for left := true; left; passes++ {
		if passes == 100 {
			t.Fatalf("100 passes of each build left work to do, the rebuild of %q among it", r.pendingRebuild(t))
		}
		olderReport, _ := older.pass(t)
		newerReport, _ := newer.pass(t)
		left = olderReport.walksLeft || newerReport.walksLeft || r.pendingRebuild(t) != ""
	}
	if flipped := r.flippedBuilds(t); !slices.Equal(flipped, []string{older.build, newer.build}) {
		t.Errorf("the rebuilds of %v flipped, want the older build's and then the newer build's", flipped)
	}
	if served := r.count(t, `select count(*) from eval_days_serving where built_by <> $1`, newer.build); served != 0 {
		t.Errorf("%d families are served by another build than the newer one, %s", served, newer.build)
	}
	t.Logf("in %d passes of each build, the rebuilds of %v flipped", passes, r.flippedBuilds(t))
}

// liveProcess registers a live server process (server_processes, as processes.Register writes
// one) that serves commit, its heartbeat an hour ahead so that it stays live however long the test
// runs, and answers its instance id.
func (r *refresherRig) liveProcess(t *testing.T, commit string) string {
	t.Helper()
	var instance string
	if err := r.database.Pool.QueryRow(t.Context(), `
		insert into server_processes (instance, commit, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), $1, now() + interval '1 hour', $2) returning instance::text
	`, commit, store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("register a server process of %q: %v", commit, err)
	}
	return instance
}

// TestARebuildBackIsTakenBackOnceItsBuildHasLeft (07:59Z, 1): a process of an older build that
// begins after a release has flipped rebuilds the rows back, as a rollback does; the newer build's
// running process begins no rebuild while a live server process serves the older build
// (server_processes), and takes the rows back once, in one rebuild, when none does. Over the
// settled delivery and Legion histories, walked two days a pass, with a live server process of
// each build registered, a newer build's release rebuilds and flips; then, with no pass of the
// newer build's process between, as a process whose walks have nothing left makes none until its
// next batch, a process of the build before it begins, and rebuilds and flips them back. Twenty
// passes of the newer build begin no rebuild; then the older build's process leaves the registry,
// deregistered or its heartbeat lapsed past the live window, and the newer build's passes flip the
// rows back to it once, every family served by it. Where the processes are unstamped, their
// registry rows naming no build, the newer build's passes begin none, before the older build's
// process leaves or after. Before, a process whose own build had served every family never
// rebuilt again, and the rows stayed the older build's for as long as the newer one ran; and a
// process that had flipped wanted its release until a pass read the serving records again, so its
// next pass rebuilt the rows while the older build still ran.
func TestARebuildBackIsTakenBackOnceItsBuildHasLeft(t *testing.T) {
	t.Parallel()
	for _, leave := range []struct {
		name    string
		stamped bool
		leave   func(t *testing.T, r *refresherRig, instance string)
	}{
		{"the older build's process deregisters", true, func(t *testing.T, r *refresherRig, instance string) {
			r.exec(t, "deregister the older build's process", `delete from server_processes where instance = $1`, instance)
		}},
		{"the older build's heartbeat lapses", true, func(t *testing.T, r *refresherRig, instance string) {
			r.exec(t, "let the older build's heartbeat lapse", `update server_processes set heartbeat_at = now() - $2::interval where instance = $1`,
				instance, processes.LiveWindow+time.Minute)
		}},
		{"unstamped processes", false, func(t *testing.T, r *refresherRig, instance string) {
			r.exec(t, "deregister the older build's process", `delete from server_processes where instance = $1`, instance)
		}},
	} {
		t.Run(leave.name, func(t *testing.T) {
			t.Parallel()
			r := newRefresherRig(t)
			r.batch = 2
			r.seedHistories(t)
			r.settle(t)
			r.pass(t)
			commit := func(build string) string {
				if leave.stamped {
					return build
				}
				return ""
			}
			newer, older := *r, *r
			newer.release("newer-build")
			newer.liveProcess(t, commit(newer.build))
			newer.settle(t)
			older.release(r.build)
			instance := older.liveProcess(t, commit(older.build))
			older.settle(t)
			if flipped := r.flippedBuilds(t); !slices.Equal(flipped, []string{newer.build, older.build}) {
				t.Fatalf("the rebuilds of %v flipped, and the check needs the newer build's and then the older build's, which rebuilt the rows back", flipped)
			}
			rebuilds := r.count(t, `select count(*) from eval_rebuilds`)
			for range 20 {
				newer.pass(t)
			}
			if began := r.count(t, `select count(*) from eval_rebuilds`) - rebuilds; began != 0 {
				t.Errorf("while a process of the older build was live, the newer build's passes began %d rebuilds, want none", began)
			}
			leave.leave(t, r, instance)
			newer.settle(t)
			want, serving := []string{newer.build, older.build, newer.build}, newer.build
			if !leave.stamped {
				want, serving = want[:2], older.build
			}
			if flipped := r.flippedBuilds(t); !slices.Equal(flipped, want) || r.pendingRebuild(t) != "" {
				t.Errorf("the rebuilds of %v flipped and %q is building, want %v flipped and none building", flipped, r.pendingRebuild(t), want)
			}
			if other := r.count(t, `select count(*) from eval_days_serving where built_by <> $1`, serving); other != 0 {
				t.Errorf("%d families are served by another build than %s", other, serving)
			}
		})
	}
}

// TestARebuildWhoseBuildRunsIsNotAbandonedWhileItWaits: every pass of the walks of the build that
// runs a rebuild touches it, though it stages nothing, so another build abandons only a rebuild no
// pass of its build has touched for staleRebuild, and then whether or not it wants a rebuild of
// its own. Over the settled delivery and Legion histories, a new build's release begins its
// rebuild; then every walk begins again in the open, which the rebuild waits for (caughtUp), and
// the rebuild's record is dated staleRebuild back. A pass of the new build, which stages nothing
// of the rebuild, and then a pass of the build before it leave the rebuild building; dated back
// again, with no pass of its build after, the build before it, whose rows are served and which
// wants no rebuild, abandons it in its pass, which deletes the rebuild's staging and queue, and a
// write of day rows after it queues nothing. Before, a pass that waited touched nothing, and the
// build before it abandoned a rebuild whose build still ran.
func TestARebuildWhoseBuildRunsIsNotAbandonedWhileItWaits(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	old := *r
	r.release("release-build")
	r.pass(t)
	rebuild, _ := r.buildingRebuild(t)
	if rebuild == 0 {
		t.Fatal("the release's first pass began no rebuild")
	}
	dateBack := func() {
		t.Helper()
		r.exec(t, "date the rebuild's record back", `update eval_rebuilds set updated_at = now() - $2::interval where id = $1`, rebuild, staleRebuild+time.Minute)
	}
	r.exec(t, "begin every walk of the day rows again in the open", `update eval_days_walks set next_day = null where family in ($1, $2)`, walkDelivery, walkLegion)
	dateBack()
	staged := r.stagedPrint(t)
	r.pass(t)
	if r.stagedPrint(t) != staged {
		t.Fatal("the release's pass staged rows while every walk began again, and the check needs a pass that waits")
	}
	old.pass(t)
	if building, _ := r.buildingRebuild(t); building != rebuild {
		t.Errorf("after a pass of its build that waited, a pass of the build before it left rebuild %d building, want rebuild %d", building, rebuild)
	}
	dateBack()
	old.pass(t)
	if building, _ := r.buildingRebuild(t); building != 0 {
		t.Errorf("a pass of the build before the release left rebuild %d building, which no pass of its build touched for staleRebuild", building)
	}
	queue := `select count(*) from eval_rebuild_reverify`
	if staged, queued := r.staged(t), r.count(t, queue); staged != 0 || queued != 0 {
		t.Errorf("the pass that abandoned the rebuild left %d staged rows and %d queued days", staged, queued)
	}
	r.exec(t, "write the day rows of the history's last day", `update eval_days set n = n + 1 where writer = 'computation' and day = $1`, refresherToday.AddDate(0, 0, -1))
	if queued := r.count(t, queue); queued != 0 {
		t.Errorf("a write of day rows after the abandon queued %d days, and no rebuild is building", queued)
	}
}

// TestAWalkBackStagesNoRowOfADayAPageEmptied: a day a walk back walks again whose input and stored
// rows a page took since keeps none of the rows the rebuild staged of it before, so the flip does
// not write them back. Over the hand-written Legion history of EVAL with EVAL-3 closed on the day
// the history begins, two days before the project's first close, settled, the first day's rows are
// left as a build of other rules built them; a new build's rebuild walks one day a pass and stages
// that day. EVAL-3 then closes on the third day again, as the history has it, and the page that
// reads it writes the first day and the day after it, which hold no input and no zero row any more,
// and deletes their rows. The first day's first input days move, so the rebuild walks back to it
// and walks it again, finding no row: once the passes settle, the one rebuild has flipped and
// EVAL's Legion rows, members and figures are those of a second database that walked the history
// whole. Before, the walk back left the staged rows of the days it found empty, which the flip
// wrote back.
func TestAWalkBackStagesNoRowOfADayAPageEmptied(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	first := legionHistoryDay(0)
	closed := history.rows["EVAL-3"]
	early := closed
	early.ClosedAt = new(first.Add(14 * time.Hour))
	history.rows["EVAL-3"] = early
	r.writeFacts(t, history, "EVAL-3")
	r.settle(t)
	r.exec(t, "leave the first day's rows as a build of other rules built them", `
		update eval_days set n = n + 5 where project_key = 'EVAL' and writer = 'computation' and day = $1`, first)
	r.batch = 1
	r.release("release-build")
	r.pass(t)
	if next, rebuilding := r.rebuildNext(t, walkLegion, "EVAL"); !rebuilding || next == nil || !next.After(first) {
		t.Fatalf("after the release's first pass EVAL's Legion walk stands at %v, rebuilding %t, and the check needs a rebuild that has built %s",
			next, rebuilding, first.Format(time.DateOnly))
	}
	if staged := r.count(t, `select count(*) from eval_rebuild_days where project_key = 'EVAL' and op = 'u' and day = $1`, first); staged == 0 {
		t.Fatalf("the rebuild staged no row of %s, and the check needs the rows it staged there", first.Format(time.DateOnly))
	}
	history.rows["EVAL-3"] = closed
	r.writeFacts(t, history, "EVAL-3")
	r.settle(t)
	if flipped := r.flippedBuilds(t); !slices.Equal(flipped, []string{r.build}) || r.pendingRebuild(t) != "" {
		t.Errorf("the rebuilds of %v flipped and %q is building, want the release's flipped and none building", flipped, r.pendingRebuild(t))
	}
	walk := newRefresherRig(t)
	walk.seedLegionEval(t)
	walk.settle(t)
	r.expectSameLegionAsWalk(t, "a walk back over a day a page emptied", walk)
}

// TestAMemberThatMovesAfterItWasStagedIsCountedOnce: the day a rebuild staged a member on is
// queued when the stored member moves in the open, so the flip's pass builds that day again and
// the item is counted once. Over the settled delivery history with its pull requests' attribution
// inputs left unread, walked two days a pass, one of the main project's pull requests of the third
// day merges a day later while the change cursor is lost, so no page writes it and the stored rows
// and member keep the day it left, as a build of other rules leaves them; the lost cursor's
// rebuild stages its member on the fourth day, where it merges now, from the third. Before the
// rebuild reaches the last day, the pull request merges a day later again, on the last day, which
// a page writes in the open, moving the stored member there. Once the passes settle, the one
// rebuild has flipped and the database holds the delivery and CI rows, the members and the figures
// of a second database whose pull request merged on the last day before its first walk. Before,
// the fourth day was not built again, and its staged rows counted the pull request beside the last
// day's.
func TestAMemberThatMovesAfterItWasStagedIsCountedOnce(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRigByIssue(t, 2)
	main := deliveries.Project
	left := deliveries.From.AddDate(0, 0, 2)
	var pr *delivery.DeliveryPullRequest
	for i, one := range deliveries.PullRequests {
		if namesIssueOf(deliveries, one) && one.MergedAt != nil && !one.Partial && measures.Midnight(*one.MergedAt).Equal(left) {
			pr = &deliveries.PullRequests[i]
			break
		}
	}
	if pr == nil {
		t.Fatalf("the history holds no pull request of %s merged on %s, and the check needs one", main, left.Format(time.DateOnly))
	}
	later := func(r *refresherRig) {
		t.Helper()
		r.exec(t, "merge the pull request a day later", `update delivery_pull_requests set merged_at = merged_at + interval '1 day' where repo = $1 and number = $2`,
			pr.Repo, pr.Number)
	}
	later(r)
	r.loseChangeCursor(t)
	r.pass(t)
	r.pass(t)
	staged := left.AddDate(0, 0, 1)
	var day, from *time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `
		select s.day, s.from_day from eval_rebuild_members s join eval_rebuilds b on b.id = s.rebuild_id and b.state = 'building'
		where s.project_key = $1 and s.deploy_repo = '' and s.row_key = $2 and s.op = 'u'
	`, main, PullRequestMember(pr.Repo, pr.Number)).Scan(&day, &from); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the staged member of %s#%d: %v", pr.Repo, pr.Number, err)
	}
	if day == nil || from == nil || !day.Equal(staged) || !from.Equal(left) {
		t.Fatalf("the rebuild staged %s#%d on %v from %v, and the check needs it staged on %s from %s", pr.Repo, pr.Number, day, from,
			staged.Format(time.DateOnly), left.Format(time.DateOnly))
	}
	if next, rebuilding := r.rebuildNext(t, walkDelivery, ""); !rebuilding || next == nil || next.After(staged.AddDate(0, 0, 1)) {
		t.Fatalf("the rebuild's delivery walk stands at %v, rebuilding %t, and the check needs it short of the history's last day", next, rebuilding)
	}
	later(r)
	r.settle(t)
	if flipped := r.flippedBuilds(t); len(flipped) != 1 || r.pendingRebuild(t) != "" {
		t.Errorf("the rebuilds of %v flipped and %q is building, want the lost cursor's flipped and none building", flipped, r.pendingRebuild(t))
	}
	walk, _ := newSettingsHistory(t, walkBatch)
	walk.leaveInputsUnread(t)
	later(walk)
	later(walk)
	walk.settle(t)
	r.expectSameAsWalk(t, "a pull request that merged later again after the rebuild staged it", walk)
}

// beforeStatement is a tracer (storetest.TracedStore) that runs do, once, as the first statement
// whose text holds marker starts on the pool it traces.
type beforeStatement struct {
	marker string
	do     func()
	once   sync.Once
}

func (b *beforeStatement) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, b.marker) {
		b.once.Do(b.do)
	}
	return ctx
}

func (b *beforeStatement) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestAReadingsWriteDuringARebuildsFoldStandsAfterTheFlip: a readings write of a logged eval that
// commits while a rebuild folds its figures is left as it stands by the flip. Over the settled
// Legion history of EVAL beside its logged review-latency, a new build's release rebuilds them, and
// a reading of review-latency is logged on a day the fold reads just before the fold reads the
// stored figures it compares with; once the passes settle, the one rebuild has flipped and every
// stored figure is the fold of the rows that stand. Before, the fold read the rows before the
// reading and the stored figures after it, and the flip wrote a fold of the rows without the
// reading over the reading's figures.
func TestAReadingsWriteDuringARebuildsFoldStandsAfterTheFlip(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLegionEval(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	plain := r.registry
	var logged atomic.Bool
	hook := &beforeStatement{marker: "deploy_repo, fold_key, figures, oldest_updated_at from eval_range_figures", do: func() {
		reading := row(t, map[string]any{"series": "minutes", "row_kind": "review", "row_key": "during-the-fold", "value": 500,
			"measured_at": refresherToday.AddDate(0, 0, -2).Add(9 * time.Hour)})
		if _, err := plain.WriteReadings(context.Background(), Batch{Project: r.project, Slug: "review-latency", Rows: []json.RawMessage{reading},
			Caller: alice, BuiltBy: testBuild}); err != nil {
			t.Errorf("log a reading of review-latency during the rebuild's fold: %v", err)
		}
		logged.Store(true)
	}}
	r.registry = New(storetest.TracedStore(t, r.database, hook), r.documents, plain.events, plain.attributor)
	r.release("release-build")
	r.settle(t)
	if !logged.Load() {
		t.Fatal("the rebuild read no stored figures, and the check needs its fold")
	}
	if flipped := r.flippedBuilds(t); !slices.Equal(flipped, []string{r.build}) {
		t.Errorf("the rebuilds of %v flipped, want the release's", flipped)
	}
	expectTruth(t, "a readings write during the rebuild's fold", r.storedFigures(t), r.truthFigures(t))
}

// queue is every day the queue of the rebuild that is building holds (eval_rebuild_reverify), as
// `<family> <project> <day>`.
func (r *refresherRig) queue(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select q.family || ' ' || q.project_key || ' ' || q.day from eval_rebuild_reverify q
		join eval_rebuilds b on b.id = q.rebuild_id and b.state = 'building'`)
	if err != nil {
		t.Fatalf("read the rebuild's queue: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the rebuild's queue: %v", err)
	}
	queued := map[string]bool{}
	for _, line := range lines {
		queued[line] = true
	}
	return queued
}

// writeDuringRebuild runs a new build's rebuild over r's settled, moved delivery history
// (corruptDays) until its delivery walk has built the history's first four days, and then writes
// those days' pr-open-to-merge rows of the main project with plain SQL, as a process that queues
// nothing would: the triggers queue each of them for the rebuild to build again. It answers the
// rebuild's queue then.
func (r *refresherRig) writeDuringRebuild(t *testing.T, deliveries deliveryseed.Fixture) map[string]bool {
	t.Helper()
	main := deliveries.Project
	r.corruptDays(t, main)
	r.release("release-build")
	written := deliveries.From.AddDate(0, 0, 3)
	for passes := 0; ; passes++ {
		if passes == 50 {
			t.Fatalf("50 passes of the rebuild did not build %s", written.Format(time.DateOnly))
		}
		r.pass(t)
		next, rebuilding := r.rebuildNext(t, walkDelivery, "")
		if !rebuilding {
			t.Fatalf("the rebuild ended before its delivery walk built %s, and the check needs it building", written.Format(time.DateOnly))
		}
		if next != nil && next.After(written) {
			break
		}
	}
	r.exec(t, "write four days' rows as an older build would", `
		update eval_days set n = n + 3, sum = coalesce(sum, 0) + 3, updated_at = now()
		where project_key = $1 and eval_id = $2 and writer = 'computation' and repo = '' and day >= $3 and day <= $4
	`, main, EvalID(main, SlugPROpenToMerge), deliveries.From, written)
	queued := r.queue(t)
	if days := len(queued); days <= r.batch {
		t.Fatalf("the plain write queued %d days, %v, and the check needs more than a batch of %d", days, queued, r.batch)
	}
	return queued
}

// TestTheRebuildBuildsItsQueueABatchATransactionAndStopsForAWake (08:20Z, 00:14Z): the days a
// rebuild builds again before its flip are built a batch of the gap at a time, each its own
// transaction, the end days of their figures folded again a batch at a time, and the rebuild stops
// between two for an owed wake. Over the settled, moved delivery history, walked two days a pass,
// four days a new build's rebuild has built are written with plain SQL, which queues them; then
// every pass is made with a wake owed, until the rebuild flips. No pass takes more than two days,
// or of a project more than two end days, off the queue; the rebuild flips only once its queue is
// empty; and the database then holds what a second database that walked the history whole holds.
// Before, the pass that built the queue again built every day of it in one transaction and folded
// the end days of every range that held any of them in another, however far apart, with no stop
// for a wake.
func TestTheRebuildBuildsItsQueueABatchATransactionAndStopsForAWake(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRigByIssue(t, 2)
	r.writeDuringRebuild(t, deliveries)
	passes, most := 0, map[string]int{}
	for building := true; building; passes++ {
		if passes == 100 {
			t.Fatalf("100 passes with a wake owed left the rebuild of %q building", r.pendingRebuild(t))
		}
		before := r.queue(t)
		r.owedPass(t, &gapBatch{})
		after := r.queue(t)
		days, ends := map[string]bool{}, map[string]int{}
		for line := range before {
			if after[line] {
				continue
			}
			fields := strings.Fields(line)
			if fields[0] == walkFigures {
				ends[fields[1]]++
			} else {
				days[fields[2]] = true
			}
		}
		most["days"] = max(most["days"], len(days))
		if len(days) > r.batch {
			t.Errorf("pass %d with a wake owed took the days %v off the queue, more than a batch of %d", passes+1, slices.Sorted(maps.Keys(days)), r.batch)
		}
		for project, n := range ends {
			most["end days"] = max(most["end days"], n)
			if n > r.batch {
				t.Errorf("pass %d with a wake owed took %d end days of %s off the queue, more than a batch of %d", passes+1, n, project, r.batch)
			}
		}
		if building = r.pendingRebuild(t) != ""; !building && len(after) > 0 {
			t.Errorf("the rebuild flipped with %d days queued: %v", len(after), after)
		}
	}
	walk, _ := newSettingsRigByIssue(t, walkBatch)
	r.expectSameAsWalk(t, "a rebuild whose queue was built again with a wake owed", walk)
	t.Logf("%d passes with a wake owed, at most %v taken off the queue in one", passes, most)
}

// TestAFailedPassLeavesTheFiguresOfTheDaysItBuiltAgainQueued: a day the rebuild builds again before
// its flip leaves the queue in the transaction that stages it, which queues the end days of its
// figures, so a pass that fails before it folds them leaves them for the next. Over the settled,
// moved delivery history, walked two days a pass, four days a new build's rebuild has built are
// written with plain SQL, which queues them; the first fold of figures after a queued day has left
// the queue fails once. The passes after it run to the flip: one pass failed, the one rebuild
// flipped, and the database holds what a second database that walked the history whole holds, its
// stored figures the fold of its rows. Before, the queued days left the queue before their figures
// were folded again, and the flip wrote the figures folded of the rows the plain write left.
func TestAFailedPassLeavesTheFiguresOfTheDaysItBuiltAgainQueued(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRigByIssue(t, 2)
	r.writeDuringRebuild(t, deliveries)
	for _, statement := range []string{
		`create table evals_test_armed (armed boolean not null)`,
		`create function evals_test_arm() returns trigger language plpgsql as $$
		begin
			insert into evals_test_armed values (true);
			return null;
		end $$`,
		`create trigger evals_test_arm after delete on eval_rebuild_reverify
			for each row when (old.family <> 'figures') execute function evals_test_arm()`,
		`create sequence evals_test_failures`,
		`create function evals_test_fail_fold() returns trigger language plpgsql as $$
		begin
			if exists (select 1 from evals_test_armed) and nextval('evals_test_failures') = 1 then
				raise exception 'the test fails the first fold after a queued day left the queue';
			end if;
			return null;
		end $$`,
		`create trigger evals_test_fail_fold before delete on eval_rebuild_range_figures
			for each statement execute function evals_test_fail_fold()`,
	} {
		r.exec(t, "fail the first fold after a queued day left the queue", statement)
	}
	failed, passes := 0, 0
	for left := true; left; passes++ {
		if passes == 100 {
			t.Fatalf("100 passes left the rebuild of %q building", r.pendingRebuild(t))
		}
		if _, err := r.horizon.Await(t.Context()); err != nil {
			t.Fatalf("await the event log's safe horizon: %v", err)
		}
		report, _, err := r.registry.refresh(t.Context(), RefresherDeps{Horizon: r.horizon, Build: r.build},
			refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds, &gapBatch{})
		switch {
		case err != nil && strings.Contains(err.Error(), "the test fails the first fold"):
			failed++
		case err != nil:
			t.Fatalf("the refresher's pass failed: %v", err)
		default:
			left = report.walksLeft
		}
	}
	if failed != 1 {
		t.Errorf("%d passes failed at the fold after a queued day left the queue, want one", failed)
	}
	if flipped := r.flippedBuilds(t); !slices.Equal(flipped, []string{r.build}) || r.pendingRebuild(t) != "" {
		t.Errorf("the rebuilds of %v flipped and %q is building, want the release's flipped and none building", flipped, r.pendingRebuild(t))
	}
	walk, _ := newSettingsRigByIssue(t, walkBatch)
	r.expectSameAsWalk(t, "a rebuild whose fold of the days it built again failed once", walk)
	t.Logf("%d passes, %d of them failed", passes, failed)
}
