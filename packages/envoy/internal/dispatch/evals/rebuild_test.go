package evals

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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
// a pass; then EVAL-3's close moves from the 3rd day to the 7th, and the pages that read it, with
// no walk after them (pageAlone), write both days. Each still holds a row of every series it held
// before: the 3rd, which holds no input any more, its zero rows, its work time n 0; the 7th its zero
// rows beside EVAL-3's input. Before, the page judged zero rows by the walk begun again, which had
// computed neither day, and deleted them, so those series read "no data" until the walk came back.
// Once the passes settle, EVAL's Legion rows and figures are a whole walk's over the moved close.
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
// pass until it has nothing left to do: after every pass a reader reads the stored day rows,
// members and figures it read before the restart, no row, member or figure was written (its xmin
// stands), and the change log holds no eval_days change after the restart began. Before, a release
// rewrote every computed row whose built_by was another build's, and its figures walk folded every
// figure again in the open.
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
			for ; passes < 100; passes++ {
				report, _ := r.pass(t)
				if got := prints.read(); got != print {
					moved, n := movedKeys(written, r.xmins(t, writtenQuery))
					t.Fatalf("after pass %d of the restart a reader reads other rows than before it: %d written, first %v", passes+1, n, moved)
				}
				if !report.walksLeft {
					break
				}
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
			t.Logf("%s: %d passes, every one reading the rows it read before, writing none and logging no change", restart.name, passes+1)
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

// flipLength is how long the transaction of each flip recorded since the test began was open, from
// the refresher's log of it.
func flipLength(records []slog.Record) []time.Duration {
	var lengths []time.Duration
	for _, record := range records {
		if !strings.Contains(record.Message, "flipped a rebuild") {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "transaction_ms" {
				lengths = append(lengths, time.Duration(attr.Value.Int64())*time.Millisecond)
			}
			return true
		})
	}
	return lengths
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
	log := &logtest.Recorder{}
	logtest.SetDefault(t, log)
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
	t.Logf("%d passes wrote in the transactions %v, logged %v, and left %d states between %d commits; the flip took %v",
		passes, transactions, logged, len(states), len(prints.seen), flipLength(log.Records()))
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
// then that day is written in the open: the merge of one of its pull requests moves an hour and a
// pass of the new build, or of the build before it, which still runs, writes it; or a process that
// queues nothing writes its rows with plain SQL, as an older build's would. The day is queued, and
// once the rebuild has flipped the database holds what a second database holds that walked the same
// history, with the same merge where one moved, whole: the change is not lost, and no stale staged
// row overwrote it.
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
			switch writer {
			case "a process that queues nothing":
				r.exec(t, "write the day's rows as an older build would", `
					update eval_days set n = n + 3, sum = coalesce(sum, 0) + 3, updated_at = now()
					where project_key = $1 and eval_id = $2 and writer = 'computation' and repo = '' and day = $3
				`, main, EvalID(main, SlugPROpenToMerge), day)
				if queued := r.count(t, `select count(*) from eval_rebuild_reverify where project_key = $1 and family = $2 and day = $3`,
					main, walkDelivery, day); queued == 0 {
					t.Errorf("the plain write of %s during the rebuild queued it for no build again", day.Format(time.DateOnly))
				}
			case "the build before it":
				r.moveMerge(t, pr)
				old.pass(t)
			default:
				r.moveMerge(t, pr)
				r.pass(t)
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
// Over the delivery and Legion histories walked two days a pass, every transaction that writes a
// day row writes the rows of one day, the first pass leaves rows on the walk's first two days and
// none after them, the serving records follow the walks, and no rebuild and no staged row exists.
func TestAFreshDatabasesFirstWalkCommitsDayByDay(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	for _, statement := range []string{
		`create table evals_test_days (txid bigint not null, day date not null, primary key (txid, day))`,
		`create function evals_test_record_day() returns trigger language plpgsql as $$
		begin
			insert into evals_test_days (txid, day) values (txid_current(), coalesce(new.day, old.day)) on conflict do nothing;
			return null;
		end $$`,
		`create constraint trigger evals_test_record_day after insert or update or delete on eval_days
			deferrable initially deferred for each row execute function evals_test_record_day()`,
	} {
		r.exec(t, "record the days each transaction writes", statement)
	}
	r.pass(t)
	main := deliveries.Project
	var first, last *time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(day), max(day) from eval_days where project_key = $1 and eval_id = $2`,
		main, EvalID(main, SlugDeployFrequency)).Scan(&first, &last); err != nil || first == nil {
		t.Fatalf("read the first pass's delivery days: %v (%v)", err, first)
	}
	if last.Sub(*first) > 24*time.Hour {
		t.Errorf("the first pass wrote %s's deploy-frequency rows from %s to %s, want its walk's first two days alone", main,
			first.Format(time.DateOnly), last.Format(time.DateOnly))
	}
	if next, _ := r.rebuildNext(t, walkDelivery, ""); next == nil || !next.Equal(first.AddDate(0, 0, 2)) {
		t.Errorf("the delivery walk stands at %v after its first batch, want %s", next, first.AddDate(0, 0, 2).Format(time.DateOnly))
	}
	if served := r.count(t, `select count(*) from eval_days_serving where family = $1 and next_day = $2`, walkDelivery, first.AddDate(0, 0, 2)); served != 1 {
		t.Errorf("the delivery rows are served through %d records at the walk's day, want one", served)
	}
	r.settle(t)
	if wide := r.count(t, `select count(*) from (select txid from evals_test_days group by txid having count(*) > 1) wide`); wide != 0 {
		t.Errorf("%d transactions wrote the day rows of more than one day, want each a day's", wide)
	}
	if rebuilds := r.count(t, `select count(*) from eval_rebuilds`); rebuilds != 0 || r.staged(t) != 0 {
		t.Errorf("the first walk began %d rebuilds and left %d staged rows, want none", rebuilds, r.staged(t))
	}
	t.Logf("the first pass wrote %s to %s; %d transactions wrote day rows, each of one day", first.Format(time.DateOnly), last.Format(time.DateOnly),
		r.count(t, `select count(distinct txid) from evals_test_days`))
}

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

// TestARebuildResumesAfterACrash: each batch of a rebuild stages its rows and its progress in one
// transaction, so a pass that fails mid-rebuild leaves the batch before it as it stood, and the
// passes after it go on from there. Over the settled, moved delivery history (corruptDays), a new
// build's rebuild stages a batch, the next batch's staging fails once, and that pass leaves the
// same rebuild building with each of its walks where the batch before it left it; the passes then
// run to the flip: that one rebuild, the only one the table holds, flipped, no staged row left, and
// the rows, members and figures a whole walk holds. Red where the failed pass, or one after it,
// abandons the rebuild and begins another.
func TestARebuildResumesAfterACrash(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, 2)
	r.corruptDays(t, deliveries.Project)
	r.release("release-build")
	r.pass(t)
	rebuild, walks := r.buildingRebuild(t)
	if rebuild == 0 || !slices.ContainsFunc(slices.Collect(maps.Values(walks)), func(next string) bool { return next != "" }) {
		t.Fatalf("the release's first pass left rebuild %d building with walks %v, want one that staged a batch", rebuild, walks)
	}
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

// rebuildStatements is a tracer that records the SQL and the arguments of every statement that
// reads or writes a rebuild's staging on the pool it traces, and which of them write the live
// tables from it: the flip's.
type rebuildStatements struct {
	mu      sync.Mutex
	staging [][]any
	flip    []string
	arrays  []string
}

func (s *rebuildStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	if !strings.Contains(sql, "eval_rebuild") {
		return ctx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := false
	for _, table := range []string{"insert into eval_days ", "delete from eval_days d", "insert into eval_day_members", "delete from eval_day_members m",
		"update eval_range_figures", "insert into eval_range_figures", "delete from eval_range_figures", "update eval_day_figures",
		"insert into eval_day_figures", "delete from eval_day_figures", "eval_days_change_log("} {
		live = live || strings.Contains(sql, table)
	}
	if live {
		s.flip = append(s.flip, strings.Join(strings.Fields(sql), " ")[:min(len(strings.Join(strings.Fields(sql), " ")), 60)])
		for _, arg := range data.Args {
			if value := reflect.ValueOf(arg); value.Kind() == reflect.Slice && value.Type().Elem().Kind() != reflect.Uint8 {
				s.arrays = append(s.arrays, fmt.Sprintf("%T", arg))
			}
		}
		return ctx
	}
	s.staging = append(s.staging, slices.Clone(data.Args))
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
// stages rows sends at most statementBytes of parameters. Over the settled, moved delivery and
// Legion histories, a new build's rebuild runs to its flip on a traced pool: every statement that
// writes the live tables from the staging takes no array, and every other statement of the staging
// sends at most statementBytes.
func TestTheFlipSendsNoArrayAndTheStagingStaysUnderTheBound(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 4
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.corruptDays(t, deliveries.Project)
	statements := &rebuildStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	r.release("release-build")
	r.settle(t)
	if len(statements.flip) == 0 {
		t.Fatal("no statement wrote the live tables from the staging")
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
	t.Logf("the flip's %d statements sent no list; the largest of the staging's %d statements sent %d bytes", len(statements.flip), len(statements.staging), largest)
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
