package evals

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The checks of the refresher's answer to a change of the delivery settings (settings_rewrite.go):
// one pass rewrites what the change moves as the whole delivery walk would write it, and begins no
// walk; a change that moves no eval writes nothing; and the members write sends only the members
// that moved.

// settingsApplyStep is the apply step newSettingsRig records on the production jobs.
const settingsApplyStep = "terraform apply"

// newSettingsRig is a refresher over a database of its own loaded with deliverytest's history of
// five days (newSettingsHistory) and settled by its passes, each walking batch days.
func newSettingsRig(t *testing.T, batch int) (*refresherRig, deliveryseed.Fixture) {
	t.Helper()
	r, deliveries := newSettingsHistory(t, batch)
	r.settle(t)
	return r, deliveries
}

// newSettingsRigByIssue is newSettingsRig with the history's attribution inputs left unread before
// it settles (leaveInputsUnread), so the issue each row names is its attribution: the rig of a test
// whose oracle is the issue deliverytest names each pull request after.
func newSettingsRigByIssue(t *testing.T, batch int) (*refresherRig, deliveryseed.Fixture) {
	t.Helper()
	r, deliveries := newSettingsHistory(t, batch)
	r.leaveInputsUnread(t)
	r.settle(t)
	return r, deliveries
}

// newSettingsHistory is a refresher over a database of its own loaded with deliverytest's history
// of five days, whose passes each walk batch days, and none run yet. Every production job that
// succeeded records an apply step, settingsApplyStep, which succeeded but on the runs whose id ends
// in 3, where it was skipped; no setting names the step, so each job counts by its own conclusion.
func newSettingsHistory(t *testing.T, batch int) (*refresherRig, deliveryseed.Fixture) {
	t.Helper()
	r := newRefresherRig(t)
	r.batch = batch
	deliveries := deliverytest.Seed(t, r.database.Pool, deliveryseed.Spec{Days: 5})
	r.exec(t, "record the production jobs' apply step", `
		update delivery_run_jobs
		set apply_step = $2, apply_step_conclusion = case when run_id % 10 = 3 then 'skipped' else 'success' end
		where repo = $1 and name = any($3) and conclusion = 'success'
	`, deliveries.DeployRepo, settingsApplyStep, deliveries.Settings.ProductionJobNames)
	return r, deliveries
}

// putSettings saves settings as actor, as PUT /api/v1/settings/delivery does.
func (r *refresherRig) putSettings(t *testing.T, settings delivery.DeliverySettings, actor model.Actor) {
	t.Helper()
	if _, err := delivery.PutSettings(t.Context(), r.database.Pool, settings, actor); err != nil {
		t.Fatalf("save the delivery settings: %v", err)
	}
}

// isDeliveryEval reports whether evalID is a delivery or CI eval's, one a delivery day writes: a
// project's or an owner key's (owners.go), whose key holds its repository's '/', so the slug is
// what follows the id's last '/'.
func isDeliveryEval(evalID string) bool {
	return slices.Contains(deliveryDaySlugs, evalID[strings.LastIndex(evalID, "/")+1:])
}

// walkCut is what two databases' rows and stored figures of one family compare by
// (expectCutAsWalk): its rows (row), its stored figures by eval id (figure), and the rows' name in
// a failure.
type walkCut struct {
	name   string
	row    func(DayRow) bool
	figure func(evalID string) bool
}

var (
	deliveryCut = walkCut{name: "day rows", row: func(row DayRow) bool { return isDeliveryEval(row.EvalID) }, figure: isDeliveryEval}
	legionCut   = walkCut{name: "Legion day rows", row: func(row DayRow) bool { return familyOf(row) == walkLegion },
		figure: func(evalID string) bool {
			_, slug, _ := strings.Cut(evalID, "/")
			return slices.Contains(legionDaySlugs, slug)
		}}
)

// valuesOf is every stored row of cut, by rowKey, without the moment and the build of its last
// write: what two databases' rows compare by.
func (r *refresherRig) valuesOf(t *testing.T, cut walkCut) map[string]DayRow {
	t.Helper()
	values := map[string]DayRow{}
	for key, row := range r.dayRows(t) {
		if cut.row(row) {
			row.UpdatedAt, row.BuiltBy = time.Time{}, ""
			values[key] = row
		}
	}
	return values
}

// memberLines is every eval_day_members row, `<project> <family> <row key>` mapped to
// `<day> <repo> <shipped by>`.
func (r *refresherRig) memberLines(t *testing.T) map[string]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select project_key || ' ' || family || ' ' || deploy_repo || ' ' || row_key, day::text || ' ' || repo || ' ' || shipped_by from eval_day_members`)
	if err != nil {
		t.Fatalf("read the members: %v", err)
	}
	members := map[string]string{}
	var key, at string
	if _, err := pgx.ForEachRow(rows, []any{&key, &at}, func() error {
		members[key] = at
		return nil
	}); err != nil {
		t.Fatalf("read the members: %v", err)
	}
	return members
}

// figureValuesOf is the fold key and figures of each stored figures row of cut, by its line: what
// two databases' figures compare by, since the oldest updated_at and the stamp a row records are
// each database's own moments.
func figureValuesOf(stored map[string]figureRow, cut walkCut) map[string]string {
	values := map[string]string{}
	for line, row := range stored {
		if cut.figure(line[strings.LastIndex(line, " ")+1:]) {
			values[line] = fmt.Sprintf("%d %x", row.foldKey, row.figures)
		}
	}
	return values
}

// xmins is the transaction that last wrote each row query reads, by the row's name: the query's
// first column, its xmin the second.
func (r *refresherRig) xmins(t *testing.T, query string) map[string]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), query)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	written := map[string]string{}
	var name, xmin string
	if _, err := pgx.ForEachRow(rows, []any{&name, &xmin}, func() error {
		written[name] = xmin
		return nil
	}); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return written
}

// walkXmin is the transaction that last wrote the record of the delivery walk, which a walk begun
// again rewrites.
func (r *refresherRig) walkXmin(t *testing.T) string {
	t.Helper()
	var xmin string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select xmin::text from eval_days_walks where family = $1 and project_key = ''
	`, walkDelivery).Scan(&xmin); err != nil {
		t.Fatalf("read the delivery walk's record: %v", err)
	}
	return xmin
}

// movedRows is each row that appeared, went, or whose updated_at moved from before to after: a
// row whose values moved, as after holds it, or as before held it where it went.
func movedRows(before, after map[string]DayRow) []DayRow {
	var moved []DayRow
	for key, row := range after {
		if old, held := before[key]; !held || !old.UpdatedAt.Equal(row.UpdatedAt) {
			moved = append(moved, row)
		}
	}
	for key, row := range before {
		if _, kept := after[key]; !kept {
			moved = append(moved, row)
		}
	}
	return moved
}

// movedEvals is each eval with a row whose values moved from before to after (movedRows).
func movedEvals(before, after map[string]DayRow) []string {
	moved := map[string]bool{}
	for _, row := range movedRows(before, after) {
		moved[row.EvalID] = true
	}
	return slices.Sorted(maps.Keys(moved))
}

// changedEvals is how many eval_days changes each eval logged after seq: the refresher's words of
// no eval, that the rows stand at the settings (delivery.SettingsApplied) and that it recorded them
// (delivery.SettingsRecorded), left out.
func (r *refresherRig) changedEvals(t *testing.T, seq int64) map[string]int {
	t.Helper()
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", seq, err)
	}
	changed := map[string]int{}
	for _, change := range changes {
		if change.Kind == delivery.ChangeKindEvalDays && !delivery.SettingsApplied(change) && !delivery.SettingsRecorded(change) {
			changed[change.Ref]++
		}
	}
	return changed
}

// sameValues answers, sorted, the keys a and b do not hold alike, each with show of both sides,
// the first ten of them.
func sameValues[V any](a, b map[string]V, show func(V) string) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	var differ []string
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		one, inA := a[key]
		other, inB := b[key]
		if inA != inB || !reflect.DeepEqual(one, other) {
			left, right := "none", "none"
			if inA {
				left = show(one)
			}
			if inB {
				right = show(other)
			}
			differ = append(differ, fmt.Sprintf("%s: %s, want %s", key, left, right))
		}
	}
	return differ[:min(len(differ), 10)]
}

// expectSameAsWalk is expectCutAsWalk of the delivery and CI rows (deliveryCut).
func (r *refresherRig) expectSameAsWalk(t *testing.T, what string, walk *refresherRig) {
	t.Helper()
	r.expectCutAsWalk(t, what, deliveryCut, walk)
}

// expectCutAsWalk checks that r holds the day rows of cut, the members and the stored figures of
// cut walk holds, and that r's stored figures are the fold of its rows.
func (r *refresherRig) expectCutAsWalk(t *testing.T, what string, cut walkCut, walk *refresherRig) {
	t.Helper()
	if differ := sameValues(r.valuesOf(t, cut), walk.valuesOf(t, cut), rowFacts); len(differ) > 0 {
		t.Errorf("%s: the %s differ from the whole walk's, first ones:\n\t%s", what, cut.name, strings.Join(differ, "\n\t"))
	}
	if differ := sameValues(r.memberLines(t), walk.memberLines(t), func(at string) string { return at }); len(differ) > 0 {
		t.Errorf("%s: the members differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
	stored := r.storedFigures(t)
	expectTruth(t, what, stored, r.truthFigures(t))
	if differ := sameValues(figureValuesOf(stored, cut), figureValuesOf(walk.storedFigures(t), cut), func(figures string) string { return figures }); len(differ) > 0 {
		t.Errorf("%s: the stored figures differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
}

// rebuildChanges moves the change log's outbox into the log and answers how many eval_days
// changes of each eval the log holds after seq, failing t where one does not carry the rebuild's
// mark (delivery.EvalDaysWrittenByRebuild) or one eval logged more than one; who names the writer
// in a failure.
func (r *refresherRig) rebuildChanges(t *testing.T, seq int64, who string) map[string]int {
	t.Helper()
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
	for evalID, count := range logged {
		if count > 1 {
			t.Errorf("%s logged %d eval_days changes of %s, want one", who, count, evalID)
		}
	}
	return logged
}

// TestASettingsChangeRewritesInOnePassWhatAWholeWalkWrites: two databases load one history and
// settle alike, the first walking two days a pass, so a walk begun again would leave days for later
// passes. Each change of a setting k8's phase D measured ("What each setting moves") is saved in
// both, in turn: a staging gate renamed to a job no run holds and then to one every run holds, and
// back; the staging deploy job renamed and back; the production job named as the staging gate's
// job, which moves the staging gate too (a name two cards follow counts for the first), and back;
// the apply step named, and cleared. One pass of the first then rewrites what the change moves and
// begins no walk, while the second begins its delivery walk again from no record of the settings
// its rows stand built under, and walks every day. After each, the two hold the same delivery and
// CI day rows, the same members and the same stored figures, the first's the fold of its rows;
// every eval whose rows moved is one settingsMoves names; and each, as the projects that read it
// read it (an owner key's under each project that reads the key, readerEvals), has exactly one
// eval_days change of the pass, and no other eval one, so an evals view hears of the change
// once. Then the deploy repository is moved to one with pull requests and no run, and back: the
// pass writes no row and no member, logs no eval_days change and begins no walk, the read of the
// evals keyed by the deploy repository answers none of their rows and then the old repository's
// again, and the first still holds what the second's whole walk under the new settings holds.
func TestASettingsChangeRewritesInOnePassWhatAWholeWalkWrites(t *testing.T) {
	t.Parallel()
	rewriting, deliveries := newSettingsRig(t, 2)
	walking, _ := newSettingsRig(t, walkBatch)
	compare := func(what string) {
		t.Helper()
		rewriting.expectSameAsWalk(t, what, walking)
	}
	compare("the first whole walk")

	original := deliveries.Settings
	api := deliveries.Repos[1]
	steps := []struct {
		name   string
		change func(*delivery.DeliverySettings)
	}{
		{"the staging gate renamed to a job no run holds", func(s *delivery.DeliverySettings) { s.StagingE2EJobName = new("staging-e2e / run-v2") }},
		{"the staging gate named as a job every run holds", func(s *delivery.DeliverySettings) { s.StagingE2EJobName = new("build / image") }},
		{"the staging gate named back", func(s *delivery.DeliverySettings) { s.StagingE2EJobName = original.StagingE2EJobName }},
		{"the staging deploy job renamed", func(s *delivery.DeliverySettings) { s.StagingApplyJobName = new("staging-release / apply-v2") }},
		{"the staging deploy job named back", func(s *delivery.DeliverySettings) { s.StagingApplyJobName = original.StagingApplyJobName }},
		{"the production job named as the staging gate's", func(s *delivery.DeliverySettings) {
			s.ProductionJobNames = []string{*original.StagingE2EJobName}
		}},
		{"the production job named back", func(s *delivery.DeliverySettings) { s.ProductionJobNames = original.ProductionJobNames }},
		{"the apply step named", func(s *delivery.DeliverySettings) { s.ProductionApplyStepName = new(settingsApplyStep) }},
		{"the apply step cleared", func(s *delivery.DeliverySettings) { s.ProductionApplyStepName = nil }},
	}
	settings := original
	for _, step := range steps {
		prev := settings
		step.change(&settings)
		slugs, _ := settingsMoves(prev, settings)
		rewriting.putSettings(t, settings, alice)
		walking.putSettings(t, settings, alice)

		seq, walk, before := rewriting.latestSeq(t), rewriting.walkXmin(t), rewriting.dayRows(t)
		if report, _ := rewriting.pass(t); report.walksLeft {
			t.Errorf("%s: the pass left a walk with days to build", step.name)
		}
		if rewriting.walkXmin(t) != walk {
			t.Errorf("%s: the pass began the delivery walk again", step.name)
		}
		moved, changed := movedEvals(before, rewriting.dayRows(t)), rewriting.changedEvals(t, seq)
		if len(moved) == 0 {
			t.Errorf("%s: no eval's rows moved, and the change moves %v", step.name, slugs)
		}
		for _, evalID := range moved {
			if slug := evalID[strings.LastIndex(evalID, "/")+1:]; !slices.Contains(slugs, slug) {
				t.Errorf("%s: %s's rows moved, and the change moves %v alone", step.name, evalID, slugs)
			}
		}
		// An owner key's rows (owners.go) log their change under each project that reads them, and
		// none where no project does.
		want := readerEvals(moved, rewriting.ownerReaders(t))
		if got := slices.Sorted(maps.Keys(changed)); !slices.Equal(got, want) || slices.ContainsFunc(got, func(evalID string) bool { return changed[evalID] != 1 }) {
			t.Errorf("%s: the pass logged the eval_days changes %v, want one of each eval whose rows moved, as its readers read it, %v", step.name, changed, want)
		}

		walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		walking.settle(t)
		compare(step.name)
		t.Logf("%s: one pass rewrote %v, one eval_days change each", step.name, moved)
	}

	held := rewriting.keyedRead(t, deliveries.Project, original.DeployRepo)
	if len(held) == 0 {
		t.Fatalf("the read of %s's evals keyed by the deploy repository answers no row under %s", deliveries.Project, original.DeployRepo)
	}
	for _, move := range []struct {
		name, repo string
		want       []string
	}{
		{"the deploy repository moved to one with no run", api, nil},
		{"the deploy repository moved back", original.DeployRepo, held},
	} {
		settings.DeployRepo = move.repo
		rewriting.putSettings(t, settings, alice)
		walking.putSettings(t, settings, alice)
		seq, walk := rewriting.latestSeq(t), rewriting.walkXmin(t)
		rows, members := rewriting.xmins(t, dayRowXmins), rewriting.xmins(t, memberXmins)
		if report, _ := rewriting.pass(t); report.walksLeft {
			t.Errorf("%s: the pass left a walk with days to build", move.name)
		}
		show := func(xmin string) string { return "written by " + xmin }
		if differ := sameValues(rewriting.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("%s: the pass wrote day rows, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(rewriting.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("%s: the pass wrote members, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if changed := rewriting.changedEvals(t, seq); len(changed) > 0 {
			t.Errorf("%s: the pass logged the eval_days changes %v, want none of an eval", move.name, changed)
		}
		if rewriting.walkXmin(t) != walk {
			t.Errorf("%s: the pass began the delivery walk again", move.name)
		}
		if got := rewriting.keyedRead(t, deliveries.Project, move.repo); !slices.Equal(got, move.want) {
			t.Errorf("%s: the read of the evals keyed by the deploy repository answers %d rows, want %d", move.name, len(got), len(move.want))
		}
		walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		walking.settle(t)
		compare(move.name)
		t.Logf("%s: no row written, the keyed evals read %d rows", move.name, len(move.want))
	}
}

// TestASettingsRewriteFoldsTheFirstPaintOnlyIfAllMarkedEvalsAreComputed covers two cuts with
// refolds still owed. The rewrite folds the standing computed eval as well as the staging gate
// it moves, but defers the paint of the cut whose standing mark names a logged eval.
func TestASettingsRewriteFoldsTheFirstPaintOnlyIfAllMarkedEvalsAreComputed(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsHistory(t, walkBatch)
	project, repo := deliveries.Project, deliveries.DeployRepo
	r.logReviews(t, project, refresherToday.AddDate(0, 0, -1), repo)
	r.settle(t)

	own, named := staleCut{project: project}, staleCut{project: project, repo: repo}
	gate, other := EvalID(project, SlugStagingE2E), EvalID(project, SlugDeployFrequency)
	logged := EvalID(project, "review-latency")
	// Both cuts already owe a paint, just as after a mark that has not been refolded yet.
	r.exec(t, "leave first paints owed for a computed eval and a logged eval", `
		insert into eval_figure_cuts_stale (project_key, repo, generation, evals)
		values ($1, '', 1, array[$2::text]), ($1, $3, 1, array[$4::text])`, project, other, repo, logged)
	r.exec(t, "leave the computed eval's range figures older than its rows", `
		update eval_range_figures set oldest_updated_at = oldest_updated_at - interval '1 day'
		where project_key = $1 and repo = '' and eval_id = $2`, project, other)

	settings := deliveries.Settings
	settings.StagingE2EJobName = new("no-such-job")
	r.putSettings(t, settings, alice)
	r.pageAlone(t)

	marks := r.staleMarks(t)
	through := refresherToday.AddDate(0, 0, FigureLead)
	computed := []string{other, gate}
	mixed := []string{logged, gate}
	if mark := marks[own]; mark.generation != 2 || mark.ready == nil || !mark.ready.Equal(through) || mark.next != nil || !slices.Equal(mark.evals, computed) {
		t.Errorf("the rewrite's own cut has %s, want generation 2 with %v and its first paint folded", mark, computed)
	}
	if mark := marks[named]; mark.generation != 2 || mark.ready != nil || mark.next != nil || !slices.Equal(mark.evals, mixed) {
		t.Errorf("the rewrite's repository cut has %s, want generation 2 with %v and its first paint owed", mark, mixed)
	}
	expectPaintTruth(t, "the settings rewrite's computed first paint", through, map[staleCut][]string{own: computed}, r.storedFigures(t), r.truthFigures(t))
	want := map[staleCut][]string{own: computed, named: mixed}
	r.expectReads(t, "after the settings rewrite", want, readsOf(moveShapes()), func(cut staleCut) bool { return cut == own })
	r.wokenPass(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the refold left stale cuts %v, want none", marks)
	}
	expectTruth(t, "after the settings refold", r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "after the settings refold", want, readsOf(moveShapes()), func(staleCut) bool { return true })
}

// dayRowXmins and memberXmins read the transaction that last wrote each day row and each member,
// by its key (xmins).
const (
	dayRowXmins = `select project_key || ' ' || eval_id || ' ' || deploy_repo || ' ' || series || ' ' || repo || ' ' || day::text, xmin::text from eval_days`
	memberXmins = `select project_key || ' ' || family || ' ' || deploy_repo || ' ' || row_key, xmin::text from eval_day_members`
)

// keyedRead is the day rows the eval list reads of project's evals keyed by the deploy repository
// where the delivery settings name deployRepo (DayWindowIn over every day through today), each as
// `<eval> <series> <day> n=<n>`, sorted.
func (r *refresherRig) keyedRead(t *testing.T, project, deployRepo string) []string {
	t.Helper()
	page, err := r.registry.Current(t.Context(), []string{project}, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	var keyed []Entry
	for _, entry := range page.Items {
		if KeyedByDeployRepo(entry.Definition) {
			keyed = append(keyed, entry)
		}
	}
	rows, err := DayWindowIn(t.Context(), r.database.Pool, []string{project}, keyed, deployRepo, nil,
		Range{From: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), To: r.now.AddDate(0, 0, 1)}, nil)
	if err != nil {
		t.Fatalf("read the keyed evals of %s under %s: %v", project, deployRepo, err)
	}
	labels := make([]string, len(rows))
	for i, row := range rows {
		labels[i] = fmt.Sprintf("%s %s %s n=%d", row.EvalID, row.Part.Series, row.Part.Day.Format(time.DateOnly), row.Part.N)
	}
	slices.Sort(labels)
	return labels
}

// TestASettingsChangeThatMovesNoEvalWritesNothing: over a settled history whose staging gate
// follows the production job, each change of a setting no day row reads (the two workflow paths,
// the population rule's authors and excluded repositories, the roll-up job), a production job name
// no run holds, listed beside the one that is, the production job names in another order, and the
// staging gate renamed from one name the production rule follows to the other (a name two cards
// follow counts for the first, the production card), logs its settings change, and the pass after
// it builds no delivery day (its report holds none: the rewrite records there each day it builds
// and writes, as the walk does), writes no day row, member or stored figure, logs no eval_days
// change of an eval, begins no walk, and leaves the settings the rows stand built under agreeing
// with the new ones on every field a day row reads. Where the change moves one of those fields, the
// pass logs one eval_days change of no project that the settings are recorded
// (delivery.SettingsRecorded), in the transaction that records them (a trigger of the test's own
// database notes the transaction of each), and none where it moves none. The same settings saved
// again by another person log no change at all (migration 20261120111107), and the pass after them
// writes nothing either.
func TestASettingsChangeThatMovesNoEvalWritesNothing(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	settings := deliveries.Settings
	settings.StagingE2EJobName = new(settings.ProductionJobNames[0])
	r.putSettings(t, settings, alice)
	r.settle(t)
	for _, statement := range []string{
		`create table evals_test_recorded (txid bigint not null, what text not null)`,
		`create function evals_test_recorded() returns trigger language plpgsql as $$
		begin
			insert into evals_test_recorded values (txid_current(), tg_argv[0]);
			return null;
		end $$`,
		`create trigger evals_test_recorded after insert on delivery_changes_pending for each row
			when (new.ref = '` + delivery.SettingsRecordedRef + `') execute function evals_test_recorded('change')`,
		`create trigger evals_test_recorded after insert or update on eval_days_settings for each row
			execute function evals_test_recorded('settings')`,
	} {
		r.exec(t, "note the transactions that record the settings", statement)
	}
	for _, change := range []struct {
		name          string
		change        func(*delivery.DeliverySettings)
		actor         model.Actor
		logs, records bool
	}{
		{"the deploy workflow's path", func(s *delivery.DeliverySettings) { s.DeployWorkflowPath = ".github/workflows/deploy-next.yml" }, alice, true, false},
		{"the PR-checks workflow's path", func(s *delivery.DeliverySettings) { s.PRChecksWorkflowPath = ".github/workflows/pr-checks-next.yml" }, alice, true, false},
		{"one population author fewer", func(s *delivery.DeliverySettings) {
			s.PopulationAuthors = s.PopulationAuthors[:len(s.PopulationAuthors)-1]
		}, alice, true, false},
		{"one excluded repository more", func(s *delivery.DeliverySettings) { s.ExcludedRepos = []string{"acme/elsewhere"} }, alice, true, false},
		{"the roll-up job renamed", func(s *delivery.DeliverySettings) { s.PRChecksRollupJobName = new("checks-complete-v2") }, alice, true, false},
		{"a production job name no run holds", func(s *delivery.DeliverySettings) {
			s.ProductionJobNames = append(slices.Clone(s.ProductionJobNames), "production-release / apply-v2")
		}, alice, true, true},
		{"the production job names in another order", func(s *delivery.DeliverySettings) {
			s.ProductionJobNames = slices.Clone(s.ProductionJobNames)
			slices.Reverse(s.ProductionJobNames)
		}, alice, true, true},
		{"the staging gate renamed between two names the production rule follows", func(s *delivery.DeliverySettings) {
			s.StagingE2EJobName = new(s.ProductionJobNames[0])
		}, alice, true, true},
		{"the same settings saved by another person", func(*delivery.DeliverySettings) {}, implementer, false, false},
	} {
		change.change(&settings)
		seq, walk, before := r.latestSeq(t), r.walkXmin(t), r.writtenState(t)
		r.putSettings(t, settings, change.actor)
		changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100)
		if err != nil {
			t.Fatalf("read the changes after %d: %v", seq, err)
		}
		logged := slices.ContainsFunc(changes, func(c delivery.Change) bool { return c.Kind == delivery.ChangeKindSettings })
		if logged != change.logs {
			t.Errorf("%s: a settings change logged is %t, want %t (%+v)", change.name, logged, change.logs, changes)
		}

		report, _ := r.pass(t)
		if built := builtDeliveryDays(report); len(built) > 0 {
			t.Errorf("%s: the pass built the delivery days %v, want none", change.name, built)
		}
		r.expectUnwritten(t, change.name, before)
		if changes, err = delivery.ChangesSince(t.Context(), r.database.Pool, seq, 1000); err != nil {
			t.Fatalf("read the changes after %d: %v", seq, err)
		}
		var words, others []delivery.Change
		for _, one := range changes {
			switch {
			case one.Kind != delivery.ChangeKindEvalDays:
			case delivery.SettingsRecorded(one):
				words = append(words, one)
			default:
				others = append(others, one)
			}
		}
		if len(others) > 0 {
			t.Errorf("%s: the pass logged the eval_days changes %+v, want none of an eval", change.name, others)
		}
		want := 0
		if change.records {
			want = 1
		}
		if len(words) != want {
			t.Errorf("%s: the pass logged %d changes that the settings are recorded (%+v), want %d", change.name, len(words), words, want)
		}
		// The change is logged by the transaction that records the settings.
		txs := map[string][]string{}
		rows, err := r.database.Pool.Query(t.Context(), `with taken as (delete from evals_test_recorded returning txid, what) select what, txid::text from taken`)
		if err != nil {
			t.Fatalf("read the transactions that recorded the settings: %v", err)
		}
		var what, txid string
		if _, err := pgx.ForEachRow(rows, []any{&what, &txid}, func() error {
			txs[what] = append(txs[what], txid)
			return nil
		}); err != nil {
			t.Fatalf("read the transactions that recorded the settings: %v", err)
		}
		if len(txs["change"]) != want || want == 1 && !slices.Contains(txs["settings"], txs["change"][0]) {
			t.Errorf("%s: the change that the settings are recorded was logged by the transactions %v, and eval_days_settings written by %v", change.name, txs["change"], txs["settings"])
		}
		if r.walkXmin(t) != walk {
			t.Errorf("%s: the pass began the delivery walk again", change.name)
		}
		recorded, _, err := r.registry.readBuiltUnder(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if recorded == nil || !sameBuiltSettings(*recorded, settings) {
			t.Fatalf("%s: the rows stand built under %+v, want the new settings, production jobs %v", change.name, recorded, settings.ProductionJobNames)
		}
	}
}

// TestAProductionRuleChangeSendsOnlyTheMembersItMoves: over a settled history, the apply step
// named moves the deploys whose step was skipped out of production, so the pull requests they
// shipped ship with a later deploy and those deploys count for other projects; the rewrite sends
// eval_day_members (as a trigger of the test's own database records each row an insert or a
// delete is handed) exactly the members whose day, repository or shipping run moved, or that are
// new, and deletes exactly those the days no longer hold: none of the members that stand.
func TestAProductionRuleChangeSendsOnlyTheMembersItMoves(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	if owed := r.count(t, `select count(*) from eval_days_owed`); owed != 0 {
		t.Fatalf("the settled history owes %d days a rewrite, which the pass would write with their members", owed)
	}
	for _, statement := range []string{
		`create table evals_test_members_sent (op text not null, member text not null)`,
		`create function evals_test_members_sent() returns trigger language plpgsql as $$
		begin
			if tg_op = 'DELETE' then
				insert into evals_test_members_sent values ('delete', old.project_key || ' ' || old.family || ' ' || old.deploy_repo || ' ' || old.row_key);
				return old;
			end if;
			insert into evals_test_members_sent values ('insert', new.project_key || ' ' || new.family || ' ' || new.deploy_repo || ' ' || new.row_key);
			return new;
		end $$`,
		`create trigger evals_test_members_sent before insert or delete on eval_day_members
			for each row execute function evals_test_members_sent()`,
	} {
		r.exec(t, "record the members the writes are handed", statement)
	}
	before := r.memberLines(t)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	r.putSettings(t, settings, alice)
	r.pass(t)
	after := r.memberLines(t)

	sent := map[string][]string{}
	rows, err := r.database.Pool.Query(t.Context(), `select op, member from evals_test_members_sent`)
	if err != nil {
		t.Fatalf("read the members sent: %v", err)
	}
	var op, member string
	if _, err := pgx.ForEachRow(rows, []any{&op, &member}, func() error {
		sent[op] = append(sent[op], member)
		return nil
	}); err != nil {
		t.Fatalf("read the members sent: %v", err)
	}
	var moved, gone []string
	standing := 0
	for key, at := range after {
		if was, held := before[key]; !held || was != at {
			moved = append(moved, key)
		} else {
			standing++
		}
	}
	for key := range before {
		if _, kept := after[key]; !kept {
			gone = append(gone, key)
		}
	}
	slices.Sort(moved)
	slices.Sort(gone)
	slices.Sort(sent["insert"])
	slices.Sort(sent["delete"])
	if !slices.Equal(sent["insert"], moved) {
		t.Errorf("the rewrite sent %d members to its upsert, want the %d that moved:\n\tsent %v\n\tmoved %v",
			len(sent["insert"]), len(moved), sent["insert"][:min(len(sent["insert"]), 10)], moved[:min(len(moved), 10)])
	}
	if !slices.Equal(sent["delete"], gone) {
		t.Errorf("the rewrite deleted the members %v, want those its days no longer hold, %v", sent["delete"], gone)
	}
	if len(moved) == 0 || standing <= len(moved) {
		t.Errorf("the apply step moved %d members and left %d standing, want some moved and more standing", len(moved), standing)
	}
	t.Logf("the apply step moved %d, deleted %d and left %d of %d members standing; the rewrite sent the moved and the deleted alone",
		len(moved), len(gone), standing, len(before))
}

// TestAProductionRuleChangeSendsOnlyTheDayRowsItMoves: over a settled history, the apply step
// named moves the deploys whose step was skipped out of production; the rewrite sends eval_days
// (as a trigger of the test's own database records each row an insert, and so an upsert, or a
// delete is handed) exactly the rows whose values moved or that are new, and deletes exactly those
// the days no longer hold: none of the rows that stand, which the rewrite builds all the same.
func TestAProductionRuleChangeSendsOnlyTheDayRowsItMoves(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	for _, statement := range []string{
		`create table evals_test_rows_sent (op text not null, project_key text not null, eval_id text not null, deploy_repo text not null,
			series text not null, repo text not null, day date not null)`,
		`create function evals_test_rows_sent() returns trigger language plpgsql as $$
		begin
			if tg_op = 'DELETE' then
				insert into evals_test_rows_sent values ('delete', old.project_key, old.eval_id, old.deploy_repo, old.series, old.repo, old.day);
				return old;
			end if;
			insert into evals_test_rows_sent values ('insert', new.project_key, new.eval_id, new.deploy_repo, new.series, new.repo, new.day);
			return new;
		end $$`,
		`create trigger evals_test_rows_sent before insert or delete on eval_days for each row execute function evals_test_rows_sent()`,
	} {
		r.exec(t, "record the day rows the writes are handed", statement)
	}
	before := r.dayRows(t)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	r.putSettings(t, settings, alice)
	r.exec(t, "forget the rows the setup was handed", `delete from evals_test_rows_sent`)
	r.pass(t)
	after := r.dayRows(t)

	sent := map[string][]string{}
	rows, err := r.database.Pool.Query(t.Context(), `select op, project_key, eval_id, deploy_repo, series, repo, day from evals_test_rows_sent`)
	if err != nil {
		t.Fatalf("read the day rows sent: %v", err)
	}
	var op string
	var row DayRow
	if _, err := pgx.ForEachRow(rows, []any{&op, &row.ProjectKey, &row.EvalID, &row.DeployRepo, &row.Series, &row.Repo, &row.Day}, func() error {
		sent[op] = append(sent[op], rowKey(row))
		return nil
	}); err != nil {
		t.Fatalf("read the day rows sent: %v", err)
	}
	var moved, gone []string
	standing := 0
	for key, now := range after {
		if was, held := before[key]; !held || !was.UpdatedAt.Equal(now.UpdatedAt) {
			moved = append(moved, key)
		} else {
			standing++
		}
	}
	for key := range before {
		if _, kept := after[key]; !kept {
			gone = append(gone, key)
		}
	}
	slices.Sort(moved)
	slices.Sort(gone)
	slices.Sort(sent["insert"])
	slices.Sort(sent["delete"])
	if !slices.Equal(sent["insert"], moved) {
		t.Errorf("the rewrite sent %d day rows to its upsert, want the %d that moved:\n\tsent %v\n\tmoved %v",
			len(sent["insert"]), len(moved), sent["insert"][:min(len(sent["insert"]), 10)], moved[:min(len(moved), 10)])
	}
	if !slices.Equal(sent["delete"], gone) {
		t.Errorf("the rewrite deleted the day rows %v, want those its days no longer hold, %v", sent["delete"], gone)
	}
	if len(moved) == 0 || standing <= len(moved) {
		t.Errorf("the apply step moved %d day rows and left %d standing, want some moved and more standing", len(moved), standing)
	}
	t.Logf("the apply step moved %d day rows, deleted %d and left %d of %d standing; the rewrite sent the moved and the deleted alone",
		len(moved), len(gone), standing, len(before))
}

// TestOnlyTheSettingsRewritesEvalDaysChangesCarryItsMark: over a settled history, the eval_days
// changes of the passes that walked it carry no cause in run_kind, but the changes of no eval
// with which the first page publishes the projects' default repositories, which carry that
// publishing's mark (delivery.EvalDaysWrittenByDefaults); the pass after the apply step is named
// rewrites the evals that moves, and each eval_days change it logs carries the rewrite's mark
// (delivery.EvalDaysWrittenBySettingsRewrite), for which the Delivery page's live hub recomputes
// an evals view at once; and the ordinary pass after a merged pull request's opening moves an hour
// earlier rewrites its merge day and logs eval_days changes that carry none. Each pass's changes
// are read from the change log once a drain has moved the outbox's rows there.
func TestOnlyTheSettingsRewritesEvalDaysChangesCarryItsMark(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	// expect requires that the eval_days changes logged after seq are some, each with runKind.
	expect := func(what string, seq int64, runKind string) {
		t.Helper()
		if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
			t.Fatalf("drain the change log's outbox: %v", err)
		}
		changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100000)
		if err != nil {
			t.Fatalf("read the delivery changes after %d: %v", seq, err)
		}
		logged, carrying := 0, 0
		for _, change := range changes {
			if change.Kind != delivery.ChangeKindEvalDays {
				continue
			}
			if change.RunKind == delivery.EvalDaysWrittenByDefaults && (change.Ref == "" || delivery.JudgingMoved(change)) || delivery.SettingsRecorded(change) {
				// The page's publishing of the default repositories (evals.publishDefaults), or the
				// record of the settings the rows stand built under (recordBuiltUnder): a change of
				// no eval.
				continue
			}
			logged++
			if change.RunKind == runKind {
				carrying++
			} else {
				t.Errorf("%s: the eval_days change of %s carries run_kind %q, want %q", what, change.Ref, change.RunKind, runKind)
			}
		}
		if logged == 0 {
			t.Fatalf("%s logged no eval_days change", what)
		}
		t.Logf("%s logged %d eval_days changes, %d of them with run_kind %q", what, logged, carrying, runKind)
	}

	expect("the passes that walked the history", 0, "")

	seq := r.latestSeq(t)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	r.putSettings(t, settings, alice)
	r.pass(t)
	expect("the settings rewrite", seq, delivery.EvalDaysWrittenBySettingsRewrite)

	var opened *delivery.DeliveryPullRequest
	for i, pr := range deliveries.PullRequests {
		if pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial && slices.Contains(settings.PopulationAuthors, pr.Author) {
			opened = &deliveries.PullRequests[i]
		}
	}
	if opened == nil {
		t.Fatal("the history holds no merged pull request of the population")
	}
	seq = r.latestSeq(t)
	r.exec(t, "open a merged pull request an hour earlier", `
		update delivery_pull_requests set created_at = created_at - interval '1 hour' where repo = $1 and number = $2
	`, opened.Repo, opened.Number)
	r.pass(t)
	expect("the ordinary pass after "+opened.Repo+"#"+strconv.Itoa(opened.Number)+" opened earlier", seq, "")
}

// historyJobReads is a tracer (storetest.Traced) that records each read of the history's jobs on
// the pool it traces: delivery.ReadDeployHistory's read of the jobs of a repository's deploy runs
// by their ids (ListRunJobsForRuns), one statement for every rewriteRunsPerRead runs, with the job
// names it asks for and the rows it answers.
type historyJobReads struct {
	mu    sync.Mutex
	reads []historyJobRead
}

// historyJobRead is one read historyJobReads records.
type historyJobRead struct {
	names []string
	rows  int64
}

// historyJobReadAt marks the context of a recorded read with its index among the reads.
type historyJobReadAt struct{}

func (h *historyJobReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "from delivery_run_jobs") || !strings.Contains(data.SQL, "run_id = any($2)") || !strings.Contains(data.SQL, "name = any($3)") || len(data.Args) < 3 {
		return ctx
	}
	names, ok := data.Args[2].([]string)
	if !ok {
		return ctx
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, historyJobRead{names: slices.Clone(names)})
	return context.WithValue(ctx, historyJobReadAt{}, len(h.reads)-1)
}

func (h *historyJobReads) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if index, ok := ctx.Value(historyJobReadAt{}).(int); ok {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.reads[index].rows = data.CommandTag.RowsAffected()
	}
}

// take answers the reads recorded since the last take, each of them ended, and forgets them.
func (h *historyJobReads) take() []historyJobRead {
	h.mu.Lock()
	defer h.mu.Unlock()
	taken := h.reads
	h.reads = nil
	return taken
}

// TestASettingsChangeReadsTheJobsOfTheCardsItMovesAlone: over a settled history, the pass after
// each change reads the history's jobs (historyJobReads, on the registry's pool) of the production
// rule's names and of the cards the change moves (settingsMoves) alone. The apply step named moves
// the production rule and with it every card: a push run counts for the projects of the deploy it
// is, which the rule's containment decides, so the read asks for every card's names. The staging
// gate renamed moves the gate alone, and the read asks for its old and new names and the
// production rule's, whose verdicts decide which projects each deploy counts for and so which rows
// each project's day holds, and not the staging deploy's.
// Each read is logged with the rows it answered beside those a read of both rules' names and of
// every card's under both settings answers.
func TestASettingsChangeReadsTheJobsOfTheCardsItMovesAlone(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	reads := &historyJobReads{}
	r.registry = r.tracedStore(t, reads)
	original := deliveries.Settings
	gate, deploy := *original.StagingE2EJobName, *original.StagingApplyJobName
	const renamed = "staging-e2e / run-v2"
	settings := original
	for _, change := range []struct {
		name   string
		change func(*delivery.DeliverySettings)
		// moves is the evals the change moves; asked the names the read asks for, and left those
		// of the cards it leaves, which it does not.
		moves, asked, left []string
	}{
		{"the apply step named", func(s *delivery.DeliverySettings) { s.ProductionApplyStepName = new(settingsApplyStep) },
			slices.Concat(leadTimeSlugs, []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugProductionDeploy, SlugStagingE2E, SlugStagingDeploy}),
			slices.Concat(original.ProductionJobNames, []string{gate, deploy}), nil},
		{"the staging gate renamed", func(s *delivery.DeliverySettings) { s.StagingE2EJobName = new(renamed) },
			[]string{SlugStagingE2E}, slices.Concat(original.ProductionJobNames, []string{gate, renamed}), []string{deploy}},
	} {
		prev := settings
		change.change(&settings)
		moves := slices.Sorted(slices.Values(change.moves))
		if slugs, _ := settingsMoves(prev, settings); !slices.Equal(slugs, moves) {
			t.Fatalf("%s moves %v, and the check needs %v", change.name, slugs, moves)
		}
		r.putSettings(t, settings, alice)
		r.pass(t)
		taken := reads.take()
		if len(taken) != 1 {
			t.Fatalf("%s: the pass read the history's jobs %d times, want once, of %s, the one repository that holds deploy runs", change.name, len(taken), deliveries.DeployRepo)
		}
		read := taken[0]
		for _, name := range change.asked {
			if !slices.Contains(read.names, name) {
				t.Errorf("%s: the read of the history's jobs asks for %q, and not %q", change.name, read.names, name)
			}
		}
		for _, name := range change.left {
			if slices.Contains(read.names, name) {
				t.Errorf("%s: the read of the history's jobs asks for %q, among them %q, which only a card the change leaves follows (it answered %d rows)",
					change.name, read.names, name, read.rows)
			}
		}
		every := slices.Concat(prev.Production().JudgedJobNames(), settings.Production().JudgedJobNames(),
			measures.DeployCardJobNames(measures.DeployCards(prev)), measures.DeployCardJobNames(measures.DeployCards(settings)))
		all := r.count(t, `
			select count(*) from delivery_run_jobs
			where repo = $1 and name = any($2) and run_id in (select run_id from delivery_runs where repo = $1 and kind = 'deploy' and head_branch = 'main')
		`, deliveries.DeployRepo, every)
		t.Logf("%s: the read of the history's jobs asked for %q and answered %d rows; one of both rules' names and every card's answers %d",
			change.name, read.names, read.rows, all)
	}
}

// copyRuns writes, as runs of to, every run of from of kind with its jobs: the same ids, times,
// conclusions and jobs, so to holds runs of its own as from does.
func (r *refresherRig) copyRuns(t *testing.T, from, to string, kind delivery.DeliveryRunKind) {
	t.Helper()
	r.copyRunsBefore(t, from, to, kind, nil)
}

// copyRunsBefore is copyRuns of the runs of from that started before before alone (every run,
// where it is nil).
func (r *refresherRig) copyRunsBefore(t *testing.T, from, to string, kind delivery.DeliveryRunKind, before *time.Time) {
	t.Helper()
	r.exec(t, "copy the "+string(kind)+" runs of "+from+" to "+to, `
		insert into delivery_runs
		select (jsonb_populate_record(null::delivery_runs, to_jsonb(run) || jsonb_build_object('repo', $2::text))).*
		from delivery_runs run where run.repo = $1 and run.kind = $3 and ($4::timestamptz is null or run.started_at < $4)`, from, to, string(kind), before)
	r.exec(t, "copy the jobs of those runs", `
		insert into delivery_run_jobs
		select (jsonb_populate_record(null::delivery_run_jobs, to_jsonb(job) || jsonb_build_object('repo', $2::text))).*
		from delivery_run_jobs job join delivery_runs run on run.repo = job.repo and run.run_id = job.run_id
		where job.repo = $1 and run.kind = $3 and ($4::timestamptz is null or run.started_at < $4)`, from, to, string(kind), before)
}

// TestAProductionRuleChangeRewritesEachRepositoryThatHoldsDeploys: two databases load one history
// in which a second repository holds deploy runs of its own (the deploy repository's, copied) and a
// third PR-checks runs alone, and each walks it whole, so both keep the three repositories' rows of
// the evals that read the deploy repository. The apply step named moves the deploys whose step was
// skipped out of production in both repositories that hold deploy runs: the pass of the first
// rewrites the rows of each of them, writes no row and no member of the repository that holds
// PR-checks runs alone, and begins no walk; the first then holds what the second's whole walk under
// the new rule holds.
func TestAProductionRuleChangeRewritesEachRepositoryThatHoldsDeploys(t *testing.T) {
	t.Parallel()
	rigs := make([]*refresherRig, 2)
	var deliveries deliveryseed.Fixture
	for i := range rigs {
		rigs[i], deliveries = newSettingsRig(t, walkBatch)
		rigs[i].copyRuns(t, deliveries.DeployRepo, deliveries.Repos[1], delivery.DeliveryRunKindDeploy)
		rigs[i].copyRuns(t, deliveries.DeployRepo, deliveries.Repos[2], delivery.DeliveryRunKindPRChecks)
		rigs[i].exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		rigs[i].settle(t)
	}
	rewriting, walking := rigs[0], rigs[1]
	rewriting.expectSameAsWalk(t, "the first whole walk", walking)

	checksOnly := deliveries.Repos[2]
	ofChecksOnly := ` where deploy_repo = '` + checksOnly + `'`
	untouched, untouchedMembers := rewriting.xmins(t, dayRowXmins+ofChecksOnly), rewriting.xmins(t, memberXmins+ofChecksOnly)
	if len(untouched) == 0 || len(untouchedMembers) == 0 {
		t.Fatalf("%s holds %d rows and %d members, and the check needs some of each", checksOnly, len(untouched), len(untouchedMembers))
	}
	before, walk := rewriting.dayRows(t), rewriting.walkXmin(t)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	rewriting.putSettings(t, settings, alice)
	walking.putSettings(t, settings, alice)
	rewriting.pass(t)
	if rewriting.walkXmin(t) != walk {
		t.Error("the pass began the delivery walk again")
	}
	moved := map[string]int{}
	for _, row := range movedRows(before, rewriting.dayRows(t)) {
		moved[row.DeployRepo]++
	}
	if moved[deliveries.DeployRepo] == 0 || moved[deliveries.Repos[1]] == 0 || moved[checksOnly] != 0 {
		t.Errorf("the pass moved the rows of each deploy repository %v, want some of %s's and %s's and none of %s's",
			moved, deliveries.DeployRepo, deliveries.Repos[1], checksOnly)
	}
	show := func(xmin string) string { return "written by " + xmin }
	if differ := sameValues(rewriting.xmins(t, dayRowXmins+ofChecksOnly), untouched, show); len(differ) > 0 {
		t.Errorf("the pass wrote rows of %s, which holds no deploy run, first ones:\n\t%s", checksOnly, strings.Join(differ, "\n\t"))
	}
	if differ := sameValues(rewriting.xmins(t, memberXmins+ofChecksOnly), untouchedMembers, show); len(differ) > 0 {
		t.Errorf("the pass wrote members of %s, which holds no deploy run, first ones:\n\t%s", checksOnly, strings.Join(differ, "\n\t"))
	}
	walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walking.settle(t)
	rewriting.expectSameAsWalk(t, "the apply step named", walking)
	t.Logf("the apply step moved rows of %v; %s's %d rows and %d members stand", moved, checksOnly, len(untouched), len(untouchedMembers))
}

// TestAProductionRuleChangeDuringTheFirstWalkRewritesWhatAWholeWalkWrites: a history whose passes
// walk two days each is walked by one pass, so the delivery walk has days left, and the apply step
// is then named. The pass after it rewrites every day of the evals the step moves, each day of the
// history among them (rewriteSettings, where the walk has not written today), as a whole walk
// under the step would write them, and the passes after it finish the walk; the database then
// holds what a second database whose first walk ran under the step holds.
func TestAProductionRuleChangeDuringTheFirstWalkRewritesWhatAWholeWalkWrites(t *testing.T) {
	t.Parallel()
	rewriting, deliveries := newSettingsHistory(t, 2)
	if report, _ := rewriting.pass(t); !report.walksLeft {
		t.Fatal("the first pass of a walk of two days a pass left no day to walk, and the check needs a walk with days left")
	}
	walking, _ := newSettingsHistory(t, walkBatch)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	rewriting.putSettings(t, settings, alice)
	walking.putSettings(t, settings, alice)
	report, _ := rewriting.pass(t)
	built := builtDeliveryDays(report)
	for day := measures.Midnight(deliveries.From); day.Before(refresherToday); day = day.AddDate(0, 0, 1) {
		if !slices.Contains(built, day.Format(time.DateOnly)) {
			t.Errorf("the pass after the step built the days %v, and not %s, a day of the history", built, day.Format(time.DateOnly))
		}
	}
	rewriting.settle(t)
	walking.settle(t)
	rewriting.expectSameAsWalk(t, "the apply step named during the first walk", walking)
}

// TestAPageWriteAfterARewriteOfEveryDayKeepsTheZeroRowsOfARepositoryItDidNotRewrite: two databases
// load one history in which a third repository holds PR-checks runs alone (the deploy
// repository's, copied), so pr-ci's rows are kept for it as the deploy repository, and walk it
// whole. A UTC day passes, so the delivery walk has not written the new today; then, in one page,
// the apply step is named and that repository's PR-checks runs of the history's third day are
// deleted, so the day holds no input of its pr-ci, whose series began before it. The pass's
// rewrite builds every day of the repositories that hold a deploy run (rewriteSettings), and the
// page then writes the third day: its pr-ci rows of the repository of PR-checks runs alone are
// zero rows (R12: a day with no input of a series begun reads 0), as a whole walk holds them. Red
// where the pass's later writes build by the first input days the rewrite built, which hold none
// of a repository it did not rewrite, and so delete those rows.
func TestAPageWriteAfterARewriteOfEveryDayKeepsTheZeroRowsOfARepositoryItDidNotRewrite(t *testing.T) {
	t.Parallel()
	rigs := make([]*refresherRig, 2)
	var deliveries deliveryseed.Fixture
	for i := range rigs {
		rigs[i], deliveries = newSettingsRig(t, walkBatch)
		rigs[i].copyRuns(t, deliveries.DeployRepo, deliveries.Repos[2], delivery.DeliveryRunKindPRChecks)
		rigs[i].exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		rigs[i].settle(t)
		rigs[i].now = rigs[i].now.AddDate(0, 0, 1)
	}
	rewriting, walking := rigs[0], rigs[1]
	checksOnly, third := deliveries.Repos[2], measures.Midnight(deliveries.From).AddDate(0, 0, 2)
	settings := deliveries.Settings
	settings.ProductionApplyStepName = new(settingsApplyStep)
	for _, r := range rigs {
		r.putSettings(t, settings, alice)
		r.exec(t, "delete "+checksOnly+"'s PR-checks runs of "+third.Format(time.DateOnly), `
			with gone as (
				delete from delivery_runs where repo = $1 and kind = $2 and started_at >= $3 and started_at < $4 returning run_id
			)
			delete from delivery_run_jobs where repo = $1 and run_id in (select run_id from gone)
		`, checksOnly, string(delivery.DeliveryRunKindPRChecks), third, third.AddDate(0, 0, 1))
	}
	rewriting.pass(t)
	walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walking.settle(t)
	zeros := `select count(*) from eval_days where deploy_repo = $1 and day = $2 and repo = '' and n = 0 and eval_id like '%/' || $3`
	held, kept := walking.count(t, zeros, checksOnly, third, SlugPRCI), rewriting.count(t, zeros, checksOnly, third, SlugPRCI)
	if held == 0 {
		t.Fatalf("a whole walk holds no pr-ci zero row of %s on %s, and the check needs some", checksOnly, third.Format(time.DateOnly))
	}
	if kept != held {
		t.Errorf("the pass left %d pr-ci zero rows of %s on %s, and a whole walk holds %d", kept, checksOnly, third.Format(time.DateOnly), held)
	}
	rewriting.expectSameAsWalk(t, "the apply step named beside "+checksOnly+"'s runs of "+third.Format(time.DateOnly)+" deleted", walking)
	t.Logf("%s holds %d pr-ci zero rows on %s", checksOnly, kept, third.Format(time.DateOnly))
}

// builtDeliveryDays is the days of the delivery writes a pass's report holds, YYYY-MM-DD, each once
// and sorted: the days a settings rewrite built and wrote, which it records as the walk does.
func builtDeliveryDays(report refreshed) []string {
	days := map[string]bool{}
	for day := range report.days {
		if day.family == walkDelivery {
			days[day.day.Format(time.DateOnly)] = true
		}
	}
	return slices.Sorted(maps.Keys(days))
}

// writtenState is the transaction that last wrote each day row, each member and each stored
// figures row, by its key: what a pass that writes nothing leaves as it was.
type writtenState struct {
	rows, members, figures map[string]string
}

func (r *refresherRig) writtenState(t *testing.T) writtenState {
	t.Helper()
	figures := map[string]string{}
	for line, row := range r.storedFigures(t) {
		figures[line] = row.xmin
	}
	return writtenState{rows: r.xmins(t, dayRowXmins), members: r.xmins(t, memberXmins), figures: figures}
}

// expectUnwritten checks that r holds every day row, member and stored figures row before held,
// each as the transaction before names last wrote it, and no other.
func (r *refresherRig) expectUnwritten(t *testing.T, what string, before writtenState) {
	t.Helper()
	after := r.writtenState(t)
	show := func(written string) string { return "written by " + written }
	for _, table := range []struct {
		name          string
		before, after map[string]string
	}{
		{"day rows", before.rows, after.rows},
		{"members", before.members, after.members},
		{"stored figures", before.figures, after.figures},
	} {
		if differ := sameValues(table.after, table.before, show); len(differ) > 0 {
			t.Errorf("%s: the pass wrote %s, first ones:\n\t%s", what, table.name, strings.Join(differ, "\n\t"))
		}
	}
}

// deliveryState is what a database's delivery and CI evals hold: their day rows (valuesOf), the
// members and their stored figures (figureValuesOf), each of deliveryCut, none with the moment or
// the build of its write.
type deliveryState struct {
	rows    map[string]DayRow
	members map[string]string
	figures map[string]string
}

func (r *refresherRig) deliveryState(t *testing.T) deliveryState {
	t.Helper()
	return deliveryState{rows: r.valuesOf(t, deliveryCut), members: r.memberLines(t), figures: figureValuesOf(r.storedFigures(t), deliveryCut)}
}

// The jobs and the step of the history a series of production rule changes runs over
// (newRuleChangeRig): ruleSecondJob names the production job of every deploy run started on the
// history's second day, and no setting names it at first; ruleSecondStep is the apply step the
// production jobs of the runs started on its fourth day record, skipped on each whose run id is a
// multiple of 3; and no run holds a job of ruleNoRunJobs.
const (
	ruleSecondJob  = "production-release / apply-v2"
	ruleSecondStep = "tofu apply"
)

var ruleNoRunJobs = []string{"production-release / apply-v3", "production-release / apply-v4"}

// newRuleChangeRig is newSettingsRig's history with the production jobs of its second day named
// ruleSecondJob and those of its fourth day recording ruleSecondStep, walked whole again: a rule
// that names ruleSecondJob beside the first name moves the verdicts of the second day's runs alone,
// and each apply step those of the runs whose jobs record it alone.
func newRuleChangeRig(t *testing.T) (*refresherRig, deliveryseed.Fixture) {
	t.Helper()
	r, deliveries := newSettingsRig(t, walkBatch)
	second, fourth := deliveries.From.AddDate(0, 0, 1), deliveries.From.AddDate(0, 0, 3)
	r.exec(t, "name the production jobs of the second day "+ruleSecondJob, `
		update delivery_run_jobs job set name = $2
		from delivery_runs run
		where job.repo = $1 and job.name = $3 and run.repo = job.repo and run.run_id = job.run_id
			and run.started_at >= $4 and run.started_at < $5
	`, deliveries.DeployRepo, ruleSecondJob, deliveries.Settings.ProductionJobNames[0], second, second.AddDate(0, 0, 1))
	r.exec(t, "record the apply step of the fourth day's production jobs as "+ruleSecondStep, `
		update delivery_run_jobs job set apply_step = $2,
			apply_step_conclusion = case when job.run_id % 3 = 0 then 'skipped' else 'success' end
		from delivery_runs run
		where job.repo = $1 and job.apply_step is not null and run.repo = job.repo and run.run_id = job.run_id
			and run.started_at >= $3 and run.started_at < $4
	`, deliveries.DeployRepo, ruleSecondStep, fourth, fourth.AddDate(0, 0, 1))
	r.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	r.settle(t)
	return r, deliveries
}

// ruleChange is one change of a series of production rule changes: its kind and the settings it
// saves.
type ruleChange struct {
	kind     string
	settings delivery.DeliverySettings
}

// ruleChanges is a series of at least twelve changes of the production rule of settings, drawn by
// rng, in which each kind below is at least once: a name some run holds added (the first name or
// ruleSecondJob), a name no run holds added, a name removed, a name renamed, a name renamed to one
// no run holds, and the apply step set, changed and cleared (settingsApplyStep, ruleSecondStep).
// Each change is one its settings admit (a name removed from a rule of two names or more), and
// until every kind has been drawn, half of them, and every one after the twelfth, are of a kind not
// drawn yet where one is admitted.
func ruleChanges(t *testing.T, rng *rand.Rand, settings delivery.DeliverySettings) []ruleChange {
	t.Helper()
	held := []string{settings.ProductionJobNames[0], ruleSecondJob}
	steps := []string{settingsApplyStep, ruleSecondStep}
	absent := func(listed, from []string) []string {
		return slices.DeleteFunc(slices.Clone(from), func(name string) bool { return slices.Contains(listed, name) })
	}
	add := func(from []string) func(*delivery.DeliverySettings) bool {
		return func(s *delivery.DeliverySettings) bool {
			missing := absent(s.ProductionJobNames, from)
			if len(missing) == 0 {
				return false
			}
			s.ProductionJobNames = append(s.ProductionJobNames, missing[rng.IntN(len(missing))])
			return true
		}
	}
	rename := func(from []string) func(*delivery.DeliverySettings) bool {
		return func(s *delivery.DeliverySettings) bool {
			missing := absent(s.ProductionJobNames, from)
			if len(missing) == 0 {
				return false
			}
			s.ProductionJobNames[rng.IntN(len(s.ProductionJobNames))] = missing[rng.IntN(len(missing))]
			return true
		}
	}
	kinds := []struct {
		name   string
		change func(*delivery.DeliverySettings) bool
	}{
		{"a name some runs hold added", add(held)},
		{"a name no run holds added", add(ruleNoRunJobs)},
		{"a name removed", func(s *delivery.DeliverySettings) bool {
			if len(s.ProductionJobNames) < 2 {
				return false
			}
			i := rng.IntN(len(s.ProductionJobNames))
			s.ProductionJobNames = slices.Delete(s.ProductionJobNames, i, i+1)
			return true
		}},
		{"a name renamed", rename(slices.Concat(held, ruleNoRunJobs))},
		{"a name renamed to one no run holds", rename(ruleNoRunJobs)},
		{"the apply step set", func(s *delivery.DeliverySettings) bool {
			if s.ProductionApplyStepName != nil {
				return false
			}
			s.ProductionApplyStepName = new(steps[rng.IntN(len(steps))])
			return true
		}},
		{"the apply step changed", func(s *delivery.DeliverySettings) bool {
			if s.ProductionApplyStepName == nil {
				return false
			}
			other := steps[0]
			if *s.ProductionApplyStepName == other {
				other = steps[1]
			}
			s.ProductionApplyStepName = new(other)
			return true
		}},
		{"the apply step cleared", func(s *delivery.DeliverySettings) bool {
			if s.ProductionApplyStepName == nil {
				return false
			}
			s.ProductionApplyStepName = nil
			return true
		}},
	}
	var series []ruleChange
	drawn := map[string]bool{}
	for len(series) < 12 || len(drawn) < len(kinds) {
		if len(series) == 60 {
			t.Fatalf("60 changes left the kinds %v undrawn", drawn)
		}
		fresh := len(series) >= 12 || rng.IntN(2) == 0
		made := false
		for _, onlyFresh := range []bool{fresh, false} {
			for _, k := range rng.Perm(len(kinds)) {
				if made || onlyFresh && drawn[kinds[k].name] {
					continue
				}
				candidate := settings
				candidate.ProductionJobNames = slices.Clone(settings.ProductionJobNames)
				if kinds[k].change(&candidate) {
					settings, drawn[kinds[k].name], made = candidate, true, true
					series = append(series, ruleChange{kind: kinds[k].name, settings: candidate})
				}
			}
		}
	}
	return series
}

// TestASeededSeriesOfProductionRuleChangesRewritesWhatAWholeWalkWrites: two databases load one
// history (newRuleChangeRig) and walk it whole. A seeded series of at least twelve changes of the
// production rule (ruleChanges: a name some runs hold added, a name no run holds added, a name
// removed, a name renamed, a name renamed to one no run holds, and the apply step set, changed and
// cleared) is saved in both, one after another; the test prints the seed, and
// EVALS_RULE_CHANGES_SEED draws a series again. After each change one pass of the first rewrites
// what it moves, and the second walks every day again from no record of its rows' settings; the
// first then holds the same day rows, members and stored figures as the second (expectSameAsWalk).
// A change after which the whole walk holds what it held before moves no verdict and no card's
// part: its pass builds no day (its report holds none) and writes no day row, member or figure.
// Each change is logged with the days its pass built and the day rows it wrote.
func TestASeededSeriesOfProductionRuleChangesRewritesWhatAWholeWalkWrites(t *testing.T) {
	t.Parallel()
	seed := rand.Uint64()
	if given := os.Getenv("EVALS_RULE_CHANGES_SEED"); given != "" {
		parsed, err := strconv.ParseUint(given, 10, 64)
		if err != nil {
			t.Fatalf("EVALS_RULE_CHANGES_SEED=%q is no seed: %v", given, err)
		}
		seed = parsed
	}
	t.Logf("the series' seed is %d: EVALS_RULE_CHANGES_SEED=%d draws it again", seed, seed)
	rewriting, deliveries := newRuleChangeRig(t)
	walking, _ := newRuleChangeRig(t)
	rewriting.expectSameAsWalk(t, "the first whole walk", walking)
	series := ruleChanges(t, rand.New(rand.NewPCG(seed, 0)), deliveries.Settings)

	walked := walking.deliveryState(t)
	quiet := 0
	for index, change := range series {
		step := "no step"
		if change.settings.ProductionApplyStepName != nil {
			step = "the step " + *change.settings.ProductionApplyStepName
		}
		what := fmt.Sprintf("change %d, %s (production jobs %q, %s)", index+1, change.kind, change.settings.ProductionJobNames, step)
		rewriting.putSettings(t, change.settings, alice)
		walking.putSettings(t, change.settings, alice)
		rows, state := rewriting.dayRows(t), rewriting.writtenState(t)
		report, _ := rewriting.pass(t)
		built, written := builtDeliveryDays(report), len(movedRows(rows, rewriting.dayRows(t)))

		walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		walking.settle(t)
		rewriting.expectSameAsWalk(t, what, walking)
		// Nothing writes the walking rig's rows between its walk of one change and the next change's
		// settings, which write none.
		now := walking.deliveryState(t)
		if reflect.DeepEqual(now, walked) {
			quiet++
			if len(built) > 0 {
				t.Errorf("%s: the whole walk holds what it held before, and the pass built the days %v", what, built)
			}
			rewriting.expectUnwritten(t, what, state)
		}
		walked = now
		t.Logf("%s: the pass built %d days %v and wrote %d day rows", what, len(built), built, written)
	}
	if quiet == 0 {
		t.Errorf("every change of the series moved the whole walk's rows, and a name no run holds added moves none")
	}
}

// TestAProductionRuleThatShipsAnEarlierPullRequestFirstRewritesTheDaysUpToTheOldFirstDay: two
// databases load one history, its pull requests' attribution inputs left unread so the issue each
// row names is its attribution (leaveInputsUnread), under a production rule whose one job no run
// holds, and walk it whole: no deploy reaches production and no pull request ships, so the deploy
// repository's lead times hold no row and no first day. The rule then names the history's
// production job, under which
// the repository's first pull request ships first: containment ships every pull request merged
// before an apply's creation, so a rule that ships any ships the first, and the first shipped pull
// request moves earlier only from none. One pass of the first rewrites the change, and the lead
// times' rows of every day from the new first day up to the old one, which is none, so through
// today, are the second's whole walk's: today's zero rows among them, which no input of today moves
// (movedDeliveryDays' first days). The first then holds what the second's whole walk holds.
func TestAProductionRuleThatShipsAnEarlierPullRequestFirstRewritesTheDaysUpToTheOldFirstDay(t *testing.T) {
	t.Parallel()
	rigs := make([]*refresherRig, 2)
	var deliveries deliveryseed.Fixture
	for i := range rigs {
		rigs[i] = newRefresherRig(t)
		deliveries = deliverytest.Seed(t, rigs[i].database.Pool, deliveryseed.Spec{Days: 5})
		rigs[i].leaveInputsUnread(t)
		none := deliveries.Settings
		none.ProductionJobNames = []string{"production-release / apply-v0"}
		rigs[i].putSettings(t, none, alice)
		rigs[i].settle(t)
	}
	rewriting, walking := rigs[0], rigs[1]
	leadTimes := func(r *refresherRig) map[string]DayRow {
		rows := map[string]DayRow{}
		for key, row := range r.valuesOf(t, deliveryCut) {
			if row.DeployRepo == deliveries.DeployRepo && slices.Contains(leadTimeSlugs, slugOf(row.ProjectKey, row.EvalID)) {
				rows[key] = row
			}
		}
		return rows
	}
	if held := leadTimes(rewriting); len(held) > 0 {
		t.Fatalf("under a rule no run holds the lead times of %s hold %d rows, and the check needs none", deliveries.DeployRepo, len(held))
	}
	for _, r := range rigs {
		r.putSettings(t, deliveries.Settings, alice)
	}
	report, _ := rewriting.pass(t)
	walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walking.settle(t)

	shipped := leadTimes(rewriting)
	if differ := sameValues(shipped, leadTimes(walking), rowFacts); len(differ) > 0 {
		t.Errorf("the lead times' rows differ from the whole walk's, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	var span daySpan
	for _, row := range shipped {
		span = span.widen(row.Day)
	}
	if !measures.Midnight(span.last).Equal(refresherToday) {
		t.Errorf("the lead times' rows end on %s, want today's zero rows, %s", span.last.Format(time.DateOnly), refresherToday.Format(time.DateOnly))
	}
	rewriting.expectSameAsWalk(t, "the history's production job named", walking)
	t.Logf("the lead times of %s hold %d rows from %s through %s; the pass built the days %v",
		deliveries.DeployRepo, len(shipped), span.first.Format(time.DateOnly), span.last.Format(time.DateOnly), builtDeliveryDays(report))
}

// TestAStagingGateRenamedToAJobSomePushRunsHoldRewritesWhatAWholeWalkWrites: two databases load
// one history in which the push runs of its first two days name their staging gate's job
// `staging-e2e / legacy`, which no card follows, and those after them whose run id is even
// `staging-e2e / run-v2`, and walk it whole. The staging gate is renamed to `staging-e2e / run-v2`,
// a job only some push runs hold. One pass of the first rewrites what the rename moves and builds
// none of the first two days, whose push runs hold no job either name follows, so the gate's part
// of each is the same under both, and no day whose rows it moves none of: the gate's series
// begins on the third day under both names, so no first day it moves earlier reaches a day after
// its moved ones, whatever first days the projects the deploy repository's pull requests count
// for hold (movedDeliveryDays). The first then holds what the second's whole walk holds.
func TestAStagingGateRenamedToAJobSomePushRunsHoldRewritesWhatAWholeWalkWrites(t *testing.T) {
	t.Parallel()
	const legacy, renamed = "staging-e2e / legacy", "staging-e2e / run-v2"
	rigs := make([]*refresherRig, 2)
	var deliveries deliveryseed.Fixture
	for i := range rigs {
		rigs[i], deliveries = newSettingsRig(t, walkBatch)
		rigs[i].exec(t, "name the staging gate's jobs anew", `
			update delivery_run_jobs job set name = case when run.started_at < $4 then $5::text else $6::text end
			from delivery_runs run
			where job.repo = $1 and job.name = $2 and run.repo = job.repo and run.run_id = job.run_id and run.kind = $3
				and (run.started_at < $4 or job.run_id % 2 = 0)
		`, deliveries.DeployRepo, *deliveries.Settings.StagingE2EJobName, string(delivery.DeliveryRunKindDeploy),
			deliveries.From.AddDate(0, 0, 2), legacy, renamed)
		rigs[i].exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		rigs[i].settle(t)
	}
	rewriting, walking := rigs[0], rigs[1]
	rewriting.expectSameAsWalk(t, "the first whole walk", walking)
	settings := deliveries.Settings
	settings.StagingE2EJobName = new(renamed)
	rewriting.putSettings(t, settings, alice)
	walking.putSettings(t, settings, alice)
	before := rewriting.dayRows(t)
	report, _ := rewriting.pass(t)
	built, third := builtDeliveryDays(report), deliveries.From.AddDate(0, 0, 2).Format(time.DateOnly)
	if len(built) == 0 || built[0] < third {
		t.Errorf("the pass built the days %v, want some and none before %s", built, third)
	}
	moved := movedRows(before, rewriting.dayRows(t))
	days := map[string]bool{}
	for _, row := range moved {
		days[row.Day.Format(time.DateOnly)] = true
	}
	for _, day := range built {
		if !days[day] {
			t.Errorf("the pass built %s, a day whose rows the rename moves none of (it built %v and moved the rows of %v)", day, built, slices.Sorted(maps.Keys(days)))
		}
	}
	walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walking.settle(t)
	rewriting.expectSameAsWalk(t, "the staging gate renamed to a job some push runs hold", walking)
	t.Logf("the staging gate renamed to %s: the pass built the days %v and wrote %d day rows", renamed, built, len(moved))
}

// TestAParallelFoldOfTheStoredFiguresWritesWhatOneWorkerWrites: over a settled history of thirty
// days, foldMovedDays folds the stored figures of every day of every eval, cut and deploy
// repository of a project that holds a listed row it reads (its own rows, repo "", and each
// repository's, its own or an owner key's it reads, eval_project_owners), once on one worker and
// once on figureFoldWorkers, and the two answer the same rows, byte for byte, in the order
// writeFigures sends them (sortByKey): a group's folds share its rows and parts and write into
// neither (figurePlan.fold). Run it under -race, by name, which fails a fold that writes into what
// the others read. The history holds no row stored under legionfacts.DefaultRepo, so each row's
// cut is its repository.
func TestAParallelFoldOfTheStoredFiguresWritesWhatOneWorkerWrites(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries := deliverytest.Seed(t, r.database.Pool, deliveryseed.Spec{Days: 30})
	r.settle(t)
	ctx, project := t.Context(), deliveries.Project
	scope, folded, err := readFigureScope(ctx, r.database.Pool, r.build, project)
	if err != nil || !folded {
		t.Fatalf("read how far the figures of %s are folded: folded %t, %v", project, folded, err)
	}
	entries, err := readEntries(ctx, r.database.Pool, project, nil)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	rows, err := r.database.Pool.Query(ctx, `
		select distinct o.project_key || substr(d.eval_id, length(o.owner) + 1), d.deploy_repo, d.repo, d.day
		from eval_project_owners o join eval_days d on d.project_key = o.owner
		where o.project_key = $1 and d.listed and d.day < $2
	`, project, scope.through)
	if err != nil {
		t.Fatalf("read the days of %s's listed rows: %v", project, err)
	}
	moved := map[figureGroupKey]map[time.Time]bool{}
	var group figureGroupKey
	var day time.Time
	if _, err := pgx.ForEachRow(rows, []any{&group.eval, &group.deployRepo, &group.repo, &day}, func() error {
		if moved[group] == nil {
			moved[group] = map[time.Time]bool{}
		}
		moved[group][day.UTC()] = true
		return nil
	}); err != nil {
		t.Fatalf("read the days of %s's listed rows: %v", project, err)
	}
	fold := func(workers int) figureWrites {
		t.Helper()
		writes, err := foldMovedDays(ctx, r.database.Pool, scope, entriesByID(entries), moved, workers)
		if err != nil {
			t.Fatalf("fold the figures of %s on %d workers: %v", project, workers, err)
		}
		writes.sortByKey()
		return writes
	}
	one, parallel := fold(1), fold(figureFoldWorkers)
	if !reflect.DeepEqual(parallel, one) {
		t.Errorf("on %d workers the fold answers %d ranges, %d gone ranges, %d days and %d gone days, and on one %d, %d, %d and %d, or other bytes",
			figureFoldWorkers, len(parallel.ranges), len(parallel.goneRanges), len(parallel.days), len(parallel.goneDays),
			len(one.ranges), len(one.goneRanges), len(one.days), len(one.goneDays))
		for index := range min(len(one.ranges), len(parallel.ranges)) {
			if !reflect.DeepEqual(parallel.ranges[index], one.ranges[index]) {
				t.Errorf("the first range that differs: %+v on %d workers, %+v on one", parallel.ranges[index], figureFoldWorkers, one.ranges[index])
				break
			}
		}
	}
	ranges := map[figureGroupKey]int{}
	for _, row := range one.ranges {
		ranges[figureGroupKey{eval: row.eval, figureCut: figureCut{repo: row.repo, deployRepo: row.deployRepo}}]++
	}
	most := 0
	for _, count := range ranges {
		most = max(most, count)
	}
	if most < 200 {
		t.Errorf("the group of most ranges folds %d, and the check needs a group of many", most)
	}
	t.Logf("%d groups folded %d ranges (at most %d in a group) and %d days on one worker and on %d",
		len(moved), len(one.ranges), most, len(one.days), figureFoldWorkers)
}

// TestARepositoryThatNeverDeployedHoldsWhatAWholeWalkHoldsOnceItsFirstRunsArrive: over a settled
// history, the deploy repository moves to the history's second repository, which holds pull
// requests and no run, and that repository then gets its first deploy runs (the deploy
// repository's of the history's first three days, copied). The passes after them leave the
// database holding the delivery and CI rows, the members and the stored figures of a second
// database that held the same runs and settings before its first walk: every day of the new
// repository from its start on, the lead times of the days after its last deploy among them, and
// its members of every pull request it counts. The change that brings the runs names their days
// and those of the pull requests they ship alone; the repositories that hold runs move (syncKept),
// and the page builds every day of the new repository from its start on in its own commit
// (refresh.writeNamed), which moves no walk. Red where the page leaves the days after the new
// repository's last deploy without those lead times' rows or those members.
func TestARepositoryThatNeverDeployedHoldsWhatAWholeWalkHoldsOnceItsFirstRunsArrive(t *testing.T) {
	t.Parallel()
	live, deliveries := newSettingsRig(t, walkBatch)
	api := deliveries.Repos[1]
	moved := deliveries.Settings
	moved.DeployRepo = api
	cutoff := deliveries.From.AddDate(0, 0, 3)
	live.putSettings(t, moved, alice)
	live.pass(t)
	live.copyRunsBefore(t, deliveries.DeployRepo, api, delivery.DeliveryRunKindDeploy, &cutoff)
	live.settle(t)

	walk, _ := newSettingsHistory(t, walkBatch)
	walk.putSettings(t, moved, alice)
	walk.copyRunsBefore(t, deliveries.DeployRepo, api, delivery.DeliveryRunKindDeploy, &cutoff)
	walk.settle(t)
	live.expectSameAsWalk(t, api+"'s first runs", walk)
	rows := live.count(t, `select count(*) from eval_days where deploy_repo = $1 and day >= $2`, api, cutoff)
	members := live.count(t, `select count(*) from eval_day_members where deploy_repo = $1 and day >= $2`, api, cutoff)
	if rows == 0 || members == 0 {
		t.Errorf("%s holds %d rows and %d members from %s on, after its last deploy, and the check needs some of each", api, rows, members, cutoff.Format(time.DateOnly))
	}
	t.Logf("%s holds %d rows and %d members from %s on, after its last deploy", api, rows, members, cutoff.Format(time.DateOnly))
}

// TestAnUnmappedRepositorysFirstRunsKeepWhatAWholeWalkKeeps: over a settled history, a repository
// with no pull request gets deploy runs (the deploy repository's, copied), which ship none of its
// pull requests (it has none) and count for no project, so the change that brings them
// writes no day. The passes after them leave the database holding the delivery and CI rows, the
// members and the stored figures of a second database that held the same runs before its first
// walk: the members of every project's pull requests under that repository from its start on,
// which the walk keeps for every repository that holds a run. The repositories that hold runs move
// (syncKept), and the page builds the new repository's members of every day from its start on in
// its own commit (refresh.writeNamed). Red where the page leaves those members out.
func TestAnUnmappedRepositorysFirstRunsKeepWhatAWholeWalkKeeps(t *testing.T) {
	t.Parallel()
	const unmapped = "acme/unmapped"
	live, deliveries := newSettingsRig(t, walkBatch)
	live.copyRuns(t, deliveries.DeployRepo, unmapped, delivery.DeliveryRunKindDeploy)
	live.settle(t)

	walk, _ := newSettingsHistory(t, walkBatch)
	walk.copyRuns(t, deliveries.DeployRepo, unmapped, delivery.DeliveryRunKindDeploy)
	walk.settle(t)
	live.expectSameAsWalk(t, unmapped+"'s first runs", walk)
	members := live.count(t, `select count(*) from eval_day_members where deploy_repo = $1`, unmapped)
	if members == 0 {
		t.Errorf("%s holds no member, and a whole walk keeps the members of every project's pull requests under it", unmapped)
	}
	t.Logf("%s holds %d members", unmapped, members)
}

// TestASeriesWhoseFirstDayLosesItsInputBeginsWhereAWholeWalkBeginsIt: over a settled history,
// every pull request merged on the first day of the main project's pr-open-to-merge is merged a
// day later, so that day holds no input of it. The passes after it leave the database holding the
// delivery and CI rows, the members and the stored figures of a second database that made the
// same move before its first walk: the series begins on the next day with input, and the day that
// lost its input holds no row of it, where it kept its zero row before (the store's first day of
// the series was that day itself). The page drops the zero row and, in the same commit, the
// series' rows of every day up to its next day with input (refresh.writeNamed), which is where a
// whole walk begins it.
func TestASeriesWhoseFirstDayLosesItsInputBeginsWhereAWholeWalkBeginsIt(t *testing.T) {
	t.Parallel()
	live, deliveries := newSettingsRig(t, walkBatch)
	main := deliveries.Project
	days, _ := seriesDays(live.readRows(t), main, SlugPROpenToMerge, measures.SeriesMinutes)
	if len(days) < 2 {
		t.Fatalf("%s's pr-open-to-merge holds rows of %v, and the check needs two days", main, days)
	}
	first := days[0]
	move := func(r *refresherRig) {
		t.Helper()
		r.exec(t, "merge every pull request of "+first.Format(time.DateOnly)+" a day later", `
			update delivery_pull_requests set merged_at = merged_at + interval '1 day' where merged_at >= $1 and merged_at < $2
		`, first, first.AddDate(0, 0, 1))
	}
	move(live)
	live.settle(t)

	walk, _ := newSettingsHistory(t, walkBatch)
	move(walk)
	walk.settle(t)
	live.expectSameAsWalk(t, "every pull request of "+first.Format(time.DateOnly)+" merged a day later", walk)
	after, _ := seriesDays(live.readRows(t), main, SlugPROpenToMerge, measures.SeriesMinutes)
	if len(after) == 0 || !after[0].After(first) {
		t.Errorf("%s's pr-open-to-merge holds rows of %v, want none on %s, which lost its input", main, after, first.Format(time.DateOnly))
	}
	t.Logf("%s's pr-open-to-merge began on %s and holds rows of %v after", main, first.Format(time.DateOnly), after)
}

// recordWrites makes every transaction that writes a day row, a member or a stored figure record
// itself as it commits: a constraint trigger deferred to the commit on eval_days, eval_day_members,
// eval_range_figures and eval_day_figures notes the transaction's id beside the table. It answers
// the tables each transaction recorded since the last call wrote, by transaction id. The table and
// the triggers are the test database's alone.
func recordWrites(t *testing.T, r rig) func() map[int64][]string {
	t.Helper()
	statements := []string{
		`create table evals_test_writes (txid bigint not null, written text not null, primary key (txid, written))`,
		`create function evals_test_record_write() returns trigger language plpgsql as $$
		begin
			insert into evals_test_writes (txid, written) values (txid_current(), tg_table_name) on conflict do nothing;
			return null;
		end $$`,
	}
	for _, table := range []string{"eval_days", "eval_day_members", "eval_range_figures", "eval_day_figures"} {
		statements = append(statements, `create constraint trigger evals_test_record_write after insert or update or delete on `+table+`
			deferrable initially deferred for each row execute function evals_test_record_write()`)
	}
	for _, statement := range statements {
		if _, err := r.database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("record the writing transactions: %v", err)
		}
	}
	return func() map[int64][]string {
		t.Helper()
		rows, err := r.database.Pool.Query(t.Context(), `
			with taken as (delete from evals_test_writes returning txid, written)
			select txid, written from taken order by txid, written`)
		if err != nil {
			t.Fatalf("read the writing transactions: %v", err)
		}
		wrote := map[int64][]string{}
		var txid int64
		var table string
		if _, err := pgx.ForEachRow(rows, []any{&txid, &table}, func() error {
			wrote[txid] = append(wrote[txid], table)
			return nil
		}); err != nil {
			t.Fatalf("read the writing transactions: %v", err)
		}
		return wrote
	}
}

const dayRowsAndMembersPrint = `
		coalesce((select string_agg(md5(d::text), '' order by d.project_key, d.eval_id, d.writer, d.deploy_repo, d.series, d.repo, d.day) from eval_days d), '') || '|' ||
		coalesce((select string_agg(md5(m::text), '' order by m.project_key, m.family, m.deploy_repo, m.row_key) from eval_day_members m), '')`

// storedPrint is one fingerprint of every stored day row, member and stored figure, read by one
// statement, so of one snapshot: what a reader of that moment can see of them.
const storedPrint = `
	select md5(` + dayRowsAndMembersPrint + ` || '|' ||
		coalesce((select string_agg(md5(f::text), '' order by md5(f::text)) from eval_range_figures f), '') || '|' ||
		coalesce((select string_agg(md5(f::text), '' order by md5(f::text)) from eval_day_figures f), ''))`

// storedSettingsPrint fingerprints the rows, members and recorded settings in one snapshot.
// Use readSettingsSnapshot beside it: a settings rewrite can leave raw stored figures stale.
const storedSettingsPrint = `
	select md5(` + dayRowsAndMembersPrint + `) || ' ' || coalesce((
		select md5(s::text) from (
			select production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name, deploy_repo
			from eval_days_settings) s), 'none')`

// readSettingsSnapshot fingerprints the day rows, members and recorded settings beside what
// the project's own and deploy repository cuts answer, not the raw figures of marked evals.
func readSettingsSnapshot(t *testing.T, registry *Store, project, repo string) string {
	t.Helper()
	ctx := t.Context()
	tx, err := registry.BeginSnapshot(ctx)
	if err != nil {
		t.Errorf("begin the settings snapshot: %v", err)
		return ""
	}
	defer tx.Rollback(ctx)
	var print string
	if err := tx.QueryRow(ctx, storedSettingsPrint).Scan(&print); err != nil {
		t.Errorf("read the day rows, members and settings: %v", err)
		return ""
	}
	page, err := CurrentIn(ctx, tx, []string{project}, "", 200, 0)
	if err != nil {
		t.Errorf("read %s's evals in the settings snapshot: %v", project, err)
		return ""
	}
	first, past := moveShapes()[0], pastShapes()[0]
	var out strings.Builder
	out.WriteString(print)
	for _, repos := range [][]string{nil, {repo}} {
		state := readListStateIn(t, tx, project, repos, page.Items, first.window, first.compare, past.window, past.compare)
		for _, shape := range []readShape{first, past} {
			stated, _, _ := state.values(t, shape.window, &shape.compare)
			fmt.Fprint(&out, " ", stated)
		}
	}
	return out.String()
}

// commitPrints records what snapshot reads (or storedPrint by default) on the rig's own pool
// after each commit by the traced pool, so a test can check what a reader sees between commits.
type commitPrints struct {
	t        *testing.T
	rig      rig
	snapshot func() string
	mu       sync.Mutex
	seen     []string
}

type commitPrintAt struct{}

func (c *commitPrints) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		return context.WithValue(ctx, commitPrintAt{}, true)
	}
	return ctx
}

func (c *commitPrints) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(commitPrintAt{}) == nil || data.Err != nil {
		return
	}
	print := c.read()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, print)
}

// read records the current state the test's snapshot reads, or the raw rows by default.
func (c *commitPrints) read() string {
	if c.snapshot != nil {
		return c.snapshot()
	}
	var print string
	if err := c.rig.database.Pool.QueryRow(context.Background(), storedPrint).Scan(&print); err != nil {
		c.t.Errorf("read the fingerprint of the stored rows: %v", err)
	}
	return print
}

// others is how many states c saw after its commits since the last call that are neither before
// nor after, and it forgets them.
func (c *commitPrints) others(before, after string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	states := map[string]bool{}
	for _, print := range c.seen {
		states[print] = true
	}
	c.seen = nil
	delete(states, before)
	delete(states, after)
	return len(states)
}

// spanChange is one change that moved a walk back before a rewound span landed in one commit: its
// name, what it does to a settled rig before the pass the test judges (prepare, nil for nothing),
// the change itself, and what a rig that never walked does to stand where prepare left the first
// (prepareWalk, nil for nothing).
type spanChange struct {
	name        string
	prepare     func(t *testing.T, r *refresherRig)
	change      func(t *testing.T, r *refresherRig)
	prepareWalk func(t *testing.T, r *refresherRig)
}

// spanChanges is the changes TestARewoundSpanLandsInOneCommitAsAWholeWalkHoldsIt makes over
// deliveries' history, where first is the first day of the main project's pr-open-to-merge:
//
//   - B: a pull request of the main project merged again six days before that first day, which
//     moves the first input day of its series earlier;
//   - E: every pull request merged on that first day merged a day later, so the day loses its input;
//   - C: the deploy repository moved to the history's second repository, which holds pull requests
//     and no run, and that repository's first deploy runs, the deploy repository's of the history's
//     first three days, copied; and a repository no project's default is given the deploy
//     repository's deploy runs.
//
// B's pull request is one of the deploy repository that names an issue of the main project, which
// it counts for through that issue whatever repository is the project's default.
func spanChanges(deliveries deliveryseed.Fixture, first time.Time) []spanChange {
	var early *delivery.DeliveryPullRequest
	for i, pr := range deliveries.PullRequests {
		if early == nil && pr.Repo == deliveries.DeployRepo && namesIssueOf(deliveries, pr) && pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial &&
			slices.Contains(deliveries.Settings.PopulationAuthors, pr.Author) {
			early = &deliveries.PullRequests[i]
		}
	}
	api, cutoff := deliveries.Repos[1], deliveries.From.AddDate(0, 0, 3)
	moved := deliveries.Settings
	moved.DeployRepo = api
	const unmapped = "acme/unmapped"
	return []spanChange{
		{name: "B, a pull request merged six days before its project's first day", change: func(t *testing.T, r *refresherRig) {
			t.Helper()
			if early == nil {
				t.Fatal("the history holds no merged pull request of the deploy repository that names an issue of the main project")
			}
			r.exec(t, "merge "+PullRequestMember(early.Repo, early.Number)+" six days before the first day", `
				update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour')
				where repo = $1 and number = $2
			`, early.Repo, early.Number, first.AddDate(0, 0, -6).Add(12*time.Hour))
		}},
		{name: "E, the first day's pull requests merged a day later", change: func(t *testing.T, r *refresherRig) {
			t.Helper()
			r.exec(t, "merge every pull request of "+first.Format(time.DateOnly)+" a day later", `
				update delivery_pull_requests set merged_at = merged_at + interval '1 day' where merged_at >= $1 and merged_at < $2
			`, first, first.AddDate(0, 0, 1))
		}},
		{name: "C, the first runs of a repository that never deployed",
			prepare: func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.putSettings(t, moved, alice)
				r.pass(t)
			},
			change: func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.copyRunsBefore(t, deliveries.DeployRepo, api, delivery.DeliveryRunKindDeploy, &cutoff)
			},
			prepareWalk: func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.putSettings(t, moved, alice)
			}},
		{name: "C, the first runs of a repository that is no project's default", change: func(t *testing.T, r *refresherRig) {
			t.Helper()
			r.copyRuns(t, deliveries.DeployRepo, unmapped, delivery.DeliveryRunKindDeploy)
		}},
	}
}

// walkRecord is the record of the delivery walk: the transaction that last wrote it and the day it
// stands at.
func (r *refresherRig) walkRecord(t *testing.T) string {
	t.Helper()
	var record string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select xmin::text || ' ' || coalesce(next_day::text, 'to begin') from eval_days_walks where family = $1 and project_key = ''
	`, walkDelivery).Scan(&record); err != nil {
		t.Fatalf("read the delivery walk's record: %v", err)
	}
	return record
}

// TestARewoundSpanLandsInOneCommitAsAWholeWalkHoldsIt: a change that moved a walk back, whose days
// the walk then rebuilt a batch a pass, each day a commit of its own, now lands in one commit
// (dispatch://INFRA-2916, R14's 2 s for every change kind). Over a settled history whose passes
// walk two days each, so a walk moved back would need several, each of spanChanges is made, and
// then one pass. That pass:
//
//   - commits every day row, member and stored figure it writes in one transaction (recordWrites);
//   - logs one eval_days change of each eval whose rows moved, as the projects that read it read
//     it (readerEvals: an owner key's under each project that reads the key, none where no
//     project does), and none of any other eval, each carrying the rebuild's mark
//     (delivery.EvalDaysWrittenByRebuild), for which the live hub recomputes an evals view at once;
//   - leaves no reader a state other than the one before it and the one after it: the fingerprint
//     of the rows, members and figures read after each of its commits (commitPrints) is one of the
//     two;
//   - leaves the delivery walk's record as it stood, and no walk with days to build;
//   - and leaves the database holding the delivery and CI rows, the members and the stored figures
//     of a second database that made the same change before its first walk (expectSameAsWalk).
func TestARewoundSpanLandsInOneCommitAsAWholeWalkHoldsIt(t *testing.T) {
	t.Parallel()
	probe, deliveries := newSettingsHistory(t, walkBatch)
	probe.settle(t)
	days, _ := seriesDays(probe.readRows(t), deliveries.Project, SlugPROpenToMerge, measures.SeriesMinutes)
	if len(days) < 2 {
		t.Fatalf("%s's pr-open-to-merge holds rows of %v, and the check needs two days", deliveries.Project, days)
	}
	for _, change := range spanChanges(deliveries, days[0]) {
		t.Run(change.name, func(t *testing.T) {
			live, _ := newSettingsRig(t, 2)
			if change.prepare != nil {
				change.prepare(t, live)
			}
			if _, err := delivery.DrainPending(t.Context(), live.database.Pool); err != nil {
				t.Fatalf("drain the change log's outbox: %v", err)
			}
			seq, record := live.latestSeq(t), live.walkRecord(t)
			change.change(t, live)
			rows := live.dayRows(t)
			wrote := recordWrites(t, live.rig)
			prints := &commitPrints{t: t, rig: live.rig}
			before := prints.read()
			live.registry = live.tracedStore(t, prints)
			report, _ := live.pass(t)
			after := prints.read()

			transactions := wrote()
			if len(transactions) != 1 {
				t.Errorf("the pass wrote day rows, members or figures in %d transactions, want one: %v", len(transactions), transactions)
			}
			if before == after {
				t.Errorf("the pass left the rows, the members and the figures as they stood, and the change moves them")
			}
			commits := len(prints.seen)
			states := prints.others(before, after)
			if states > 0 {
				t.Errorf("a reader between two commits of the pass saw %d states that are neither the one before it nor the one after it", states)
			}
			if report.walksLeft {
				t.Errorf("the pass left a walk with days to build")
			}
			if got := live.walkRecord(t); got != record {
				t.Errorf("the delivery walk's record is %q after the pass, want it as it stood, %q", got, record)
			}
			logged := live.rebuildChanges(t, seq, "the pass")
			// A repository that is no project's default brings the rows of its owner key alone
			// (owners.go), which no project reads, and so no change.
			want := readerEvals(movedEvals(rows, live.dayRows(t)), live.ownerReaders(t))
			if got := slices.Sorted(maps.Keys(logged)); !slices.Equal(got, want) {
				t.Errorf("the pass logged the eval_days changes %v, want one of each eval whose rows moved, as its readers read it, %v", logged, want)
			}

			walk, _ := newSettingsHistory(t, walkBatch)
			if change.prepareWalk != nil {
				change.prepareWalk(t, walk)
			}
			change.change(t, walk)
			walk.settle(t)
			live.expectSameAsWalk(t, change.name, walk)
			t.Logf("%s: one pass wrote in the transactions %v, logged the eval_days changes %v and left %d states between its %d commits",
				change.name, transactions, logged, states, commits)
		})
	}
}

// memberWrites is a tracer that records the arguments of each statement that writes
// eval_day_members on the pool it traces.
type memberWrites struct {
	mu   sync.Mutex
	sent [][]any
}

func (m *memberWrites) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// Every statement that takes the members of a day or of a span as arrays: the upserts and
	// deletes of eval_day_members, and the statements that find the days their items leave.
	if strings.Contains(data.SQL, "eval_day_members") && strings.Contains(data.SQL, "unnest(") {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.sent = append(m.sent, slices.Clone(data.Args))
	}
	return ctx
}

func (m *memberWrites) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestARebuiltSpansMembersPastTheBoundGoInSeveralStatements: over a settled history in which 3,000
// pull requests of the deploy repository merged on the history's last day, a repository that is no
// project's default gets its first deploy runs, the deploy repository's of the first three days, copied.
// The rows of the evals that read the deploy repository are kept for the new repository from its
// start on, and so are the members: every pull request of every day from then on, the 3,000 of the
// last day among them, a day no change of the page names. The one pass after the runs writes those
// members in statements of at most statementBytes of parameters each, several of them, and the
// database then holds what a second database that held the runs before its first walk holds.
func TestARebuiltSpansMembersPastTheBoundGoInSeveralStatements(t *testing.T) {
	t.Parallel()
	const unmapped, copies = "acme/unmapped", 3000
	crowd := func(t *testing.T, r *refresherRig, deliveries deliveryseed.Fixture) {
		t.Helper()
		crowdLastDay(t, r, deliveries, copies)
	}
	live, deliveries := newSettingsHistory(t, walkBatch)
	crowd(t, live, deliveries)
	live.settle(t)
	cutoff := deliveries.From.AddDate(0, 0, 3)
	live.copyRunsBefore(t, deliveries.DeployRepo, unmapped, delivery.DeliveryRunKindDeploy, &cutoff)
	writes := &memberWrites{}
	live.registry = live.tracedStore(t, writes)
	report, _ := live.pass(t)
	if report.walksLeft {
		t.Errorf("the pass left a walk with days to build")
	}
	largest, total := 0, 0
	for index, args := range writes.sent {
		size := paramEstimate(t, args)
		largest, total = max(largest, size), total+size
		if size > statementBytes {
			t.Errorf("statement %d that writes members takes %d bytes of parameters, past %d", index, size, statementBytes)
		}
	}
	if total <= statementBytes || len(writes.sent) < 2 {
		t.Errorf("the pass wrote members in %d statements of %d bytes of parameters together, and the check needs more than %d bytes in several",
			len(writes.sent), total, statementBytes)
	}
	held := live.count(t, `select count(*) from eval_day_members where deploy_repo = $1`, unmapped)
	if held < copies {
		t.Errorf("%s holds %d members, want the %d pull requests of the last day among them", unmapped, held, copies)
	}

	walk, _ := newSettingsHistory(t, walkBatch)
	crowd(t, walk, deliveries)
	walk.copyRunsBefore(t, deliveries.DeployRepo, unmapped, delivery.DeliveryRunKindDeploy, &cutoff)
	walk.settle(t)
	live.expectSameAsWalk(t, unmapped+"'s first runs beside "+strconv.Itoa(copies)+" pull requests of one day", walk)
	t.Logf("%s's %d members went in %d statements of %d bytes of parameters together, the largest %d of %d",
		unmapped, held, len(writes.sent), total, largest, statementBytes)
}

// namesIssueOf reports whether pr names an issue of deliveries' main project, which it counts for
// through that issue whatever repository is the project's default.
func namesIssueOf(deliveries deliveryseed.Fixture, pr delivery.DeliveryPullRequest) bool {
	return pr.IssueKey != nil && slices.Contains(deliveries.Issues[deliveries.Project], *pr.IssueKey)
}

// crowdLastDay merges copies more pull requests of the deploy repository on the history's last
// day, each a copy of one merged that day that names an issue of the main project (namesIssueOf),
// and answers that one.
func crowdLastDay(t *testing.T, r *refresherRig, deliveries deliveryseed.Fixture, copies int) delivery.DeliveryPullRequest {
	t.Helper()
	var template *delivery.DeliveryPullRequest
	last := deliveries.To.AddDate(0, 0, -1)
	for i, pr := range deliveries.PullRequests {
		if pr.Repo == deliveries.DeployRepo && namesIssueOf(deliveries, pr) && pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial &&
			!pr.MergedAt.Before(last) && slices.Contains(deliveries.Settings.PopulationAuthors, pr.Author) {
			template = &deliveries.PullRequests[i]
		}
	}
	if template == nil {
		t.Fatalf("the history holds no pull request of the deploy repository merged on its last day, %s, that names an issue of %s",
			last.Format(time.DateOnly), deliveries.Project)
	}
	r.exec(t, "merge "+strconv.Itoa(copies)+" more pull requests on the last day", `
		insert into delivery_pull_requests
		select (jsonb_populate_record(null::delivery_pull_requests, to_jsonb(pr) || jsonb_build_object('number', 100000 + n))).*
		from delivery_pull_requests pr, generate_series(1, $3::int) as n
		where pr.repo = $1 and pr.number = $2
	`, template.Repo, template.Number, copies)
	return *template
}

// TestADaysMembersPastTheBoundGoInSeveralStatements: over a history in which 3,000 pull requests
// of the deploy repository merged on its last day, their attribution inputs left unread so the
// issue each row names is its attribution (leaveInputsUnread), the walk writes that day, and then
// a page writes it again, once one of them opened an hour earlier. Each write sends the day's
// members, more than statementBytes of them, to the statements that record the days its items leave,
// upsert them and delete the ones the day no longer holds, and each of those statements takes at
// most statementBytes of parameters, the members cut into runs of their keys (memberRuns). The
// page's write leaves every member where the walk put it, and the day's rows as a second database
// holds them whose pull request opened so before its first walk. Before, each of the three
// statements took the day's members whole: 884,665 bytes in one.
func TestADaysMembersPastTheBoundGoInSeveralStatements(t *testing.T) {
	t.Parallel()
	const copies = 3000
	live, deliveries := newSettingsHistory(t, walkBatch)
	crowded := crowdLastDay(t, live, deliveries, copies)
	live.leaveInputsUnread(t)
	writes := &memberWrites{}
	live.registry = live.tracedStore(t, writes)
	check := func(what string) {
		t.Helper()
		largest, total := 0, 0
		for index, args := range writes.sent {
			size := paramEstimate(t, args)
			largest, total = max(largest, size), total+size
			if size > statementBytes {
				t.Errorf("%s: statement %d that takes members takes %d bytes of parameters, past %d", what, index, size, statementBytes)
			}
		}
		if total <= statementBytes {
			t.Errorf("%s sent members in %d statements of %d bytes of parameters together, and the check needs more than %d", what, len(writes.sent), total, statementBytes)
		}
		t.Logf("%s sent members in %d statements of %d bytes of parameters together, the largest %d of %d", what, len(writes.sent), total, largest, statementBytes)
		writes.sent = nil
	}
	live.settle(t)
	check("the walk")
	day := measures.Midnight(*crowded.MergedAt)
	members := live.memberLines(t)
	if len(members) < copies {
		t.Fatalf("the walk recorded %d members, want the %d pull requests of %s among them", len(members), copies, day.Format(time.DateOnly))
	}
	open := func(r *refresherRig) {
		t.Helper()
		r.exec(t, "open a pull request of the last day an hour earlier", `
			update delivery_pull_requests set created_at = created_at - interval '1 hour' where repo = $1 and number = $2
		`, crowded.Repo, 100000+copies)
	}
	open(live)
	report, _ := live.pass(t)
	if !report.days[builtDay{family: walkDelivery, project: deliveries.Project, day: day}] {
		t.Fatalf("the pass wrote %v, and not %s's day %s", report.sorted(), deliveries.Project, day.Format(time.DateOnly))
	}
	check("the page")
	if differ := sameValues(live.memberLines(t), members, func(at string) string { return at }); len(differ) > 0 {
		t.Errorf("the page's write moved members, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	walk, _ := newSettingsHistory(t, walkBatch)
	crowdLastDay(t, walk, deliveries, copies)
	walk.leaveInputsUnread(t)
	open(walk)
	walk.settle(t)
	live.expectSameAsWalk(t, "a day of "+strconv.Itoa(copies)+" pull requests written again", walk)
}

// spanSeriesChange is one kind of change of a seeded series (TestASeededSeriesOfSpanChanges...): it
// reads what it needs of live, draws from rng, and answers the change to make in each database,
// nil where the history admits none of its kind now.
type spanSeriesChange struct {
	name string
	draw func(t *testing.T, rng *rand.Rand, live *refresherRig) func(t *testing.T, r *refresherRig)
}

// TestASeededSeriesOfSpanChangesLandsInOnePassEachAsAWholeWalkHoldsThem: two databases load one
// history and walk it whole, the first two days a pass. A seeded series of changes that each
// moved a walk back before (a pull request merged before every first day, B; the first merge day's
// pull requests merged a day later, E; a repository's first deploy runs, the runs of its earliest
// day gone, and all its runs gone, C; and the deploy repository's deploys of its first deploy day
// gone, E and C together) is made in both, one after another; the test prints the seed, and
// EVALS_SPAN_CHANGES_SEED draws a series again. After each change one pass of the first rebuilds
// what it moves and leaves no walk with days to build, and the second walks every day again from
// no record of its rows' settings; the first then holds the same day rows, members and stored
// figures as the second (expectSameAsWalk).
func TestASeededSeriesOfSpanChangesLandsInOnePassEachAsAWholeWalkHoldsThem(t *testing.T) {
	t.Parallel()
	seed := rand.Uint64()
	if given := os.Getenv("EVALS_SPAN_CHANGES_SEED"); given != "" {
		parsed, err := strconv.ParseUint(given, 10, 64)
		if err != nil {
			t.Fatalf("EVALS_SPAN_CHANGES_SEED=%q is no seed: %v", given, err)
		}
		seed = parsed
	}
	t.Logf("the series' seed is %d: EVALS_SPAN_CHANGES_SEED=%d draws it again", seed, seed)
	rng := rand.New(rand.NewPCG(seed, 0))
	live, deliveries := newSettingsRig(t, 2)
	walking, _ := newSettingsRig(t, walkBatch)
	live.expectSameAsWalk(t, "the first whole walk", walking)

	day := func(t *testing.T, r *refresherRig, query string, args ...any) (time.Time, bool) {
		t.Helper()
		var at *time.Time
		if err := r.database.Pool.QueryRow(t.Context(), query, args...).Scan(&at); err != nil {
			t.Fatalf("read %q: %v", query, err)
		}
		if at == nil {
			return time.Time{}, false
		}
		return measures.Midnight(*at), true
	}
	var added []string
	runDays := `select count(distinct (started_at at time zone 'UTC')::date) from delivery_runs where repo = $1`
	dropRuns := func(t *testing.T, r *refresherRig, repo string, kind *delivery.DeliveryRunKind, before time.Time) {
		t.Helper()
		var of *string
		if kind != nil {
			of = new(string(*kind))
		}
		r.exec(t, "delete the jobs of "+repo+"'s runs before "+before.Format(time.DateOnly), `
			delete from delivery_run_jobs job using delivery_runs run
			where run.repo = job.repo and run.run_id = job.run_id and run.repo = $1 and ($2::text is null or run.kind = $2) and run.started_at < $3
		`, repo, of, before)
		r.exec(t, "delete "+repo+"'s runs before "+before.Format(time.DateOnly), `
			delete from delivery_runs where repo = $1 and ($2::text is null or kind = $2) and started_at < $3
		`, repo, of, before)
	}
	kinds := []spanSeriesChange{
		{"a pull request merged before every first day", func(t *testing.T, rng *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			earliest, held := day(t, live, `select min(merged_at) from delivery_pull_requests where merged_at is not null`)
			merged := live.count(t, `select count(*) from delivery_pull_requests where merged_at is not null and created_at is not null`)
			if !held || merged == 0 {
				return nil
			}
			pick, to := rng.IntN(merged), earliest.AddDate(0, 0, -1-rng.IntN(3)).Add(12*time.Hour)
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.exec(t, "merge a pull request before every first day", `
					update delivery_pull_requests request
					set merged_at = $2, created_at = least(request.created_at, $2::timestamptz - interval '1 hour')
					from (select repo, number from delivery_pull_requests where merged_at is not null and created_at is not null
						order by repo, number offset $1 limit 1) picked
					where request.repo = picked.repo and request.number = picked.number
				`, pick, to)
			}
		}},
		{"the first merge day's pull requests merged a day later", func(t *testing.T, _ *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			first, held := day(t, live, `select min(merged_at) from delivery_pull_requests where merged_at is not null`)
			if !held || live.count(t, `select count(distinct (merged_at at time zone 'UTC')::date) from delivery_pull_requests where merged_at is not null`) < 3 {
				return nil
			}
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.exec(t, "merge the first merge day's pull requests a day later", `
					update delivery_pull_requests set merged_at = merged_at + interval '1 day' where merged_at >= $1 and merged_at < $2
				`, first, first.AddDate(0, 0, 1))
			}
		}},
		{"a repository's first deploy runs", func(t *testing.T, rng *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			if len(added) == 3 {
				return nil
			}
			repo, cutoff := fmt.Sprintf("acme/span-%d", len(added)+1), deliveries.From.AddDate(0, 0, 2+rng.IntN(4))
			if len(added) == 0 {
				// The history's second repository, which a project is mapped to and whose pull requests
				// its deploys ship.
				repo = deliveries.Repos[1]
			}
			added = append(added, repo)
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				r.copyRunsBefore(t, deliveries.DeployRepo, repo, delivery.DeliveryRunKindDeploy, &cutoff)
			}
		}},
		{"the runs of a repository's earliest day gone", func(t *testing.T, rng *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			var candidates []string
			for _, repo := range added {
				if live.count(t, runDays, repo) >= 2 {
					candidates = append(candidates, repo)
				}
			}
			if len(candidates) == 0 {
				return nil
			}
			repo := candidates[rng.IntN(len(candidates))]
			since, _ := day(t, live, `select min(started_at) from delivery_runs where repo = $1`, repo)
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				dropRuns(t, r, repo, nil, since.AddDate(0, 0, 1))
			}
		}},
		{"every run of a repository gone", func(t *testing.T, rng *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			var candidates []string
			for _, repo := range added {
				if live.count(t, runDays, repo) >= 1 {
					candidates = append(candidates, repo)
				}
			}
			if len(candidates) == 0 {
				return nil
			}
			repo := candidates[rng.IntN(len(candidates))]
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				dropRuns(t, r, repo, nil, live.now.AddDate(1, 0, 0))
			}
		}},
		{"the deploy repository's deploys of its first deploy day gone", func(t *testing.T, _ *rand.Rand, live *refresherRig) func(*testing.T, *refresherRig) {
			deploy := delivery.DeliveryRunKindDeploy
			first, held := day(t, live, `select min(started_at) from delivery_runs where repo = $1 and kind = $2`, deliveries.DeployRepo, string(deploy))
			if !held || live.count(t, `select count(distinct (started_at at time zone 'UTC')::date) from delivery_runs where repo = $1 and kind = $2`,
				deliveries.DeployRepo, string(deploy)) < 3 {
				return nil
			}
			return func(t *testing.T, r *refresherRig) {
				t.Helper()
				dropRuns(t, r, deliveries.DeployRepo, &deploy, first.AddDate(0, 0, 1))
			}
		}},
	}
	// The series is at least 14 changes, and goes on, a kind not drawn yet first, until every kind
	// is drawn or 30 changes are made: a seed can leave a history that admits one kind never (the
	// runs of a repository's earliest day go only where an added repository holds runs of two days).
	drawn := map[string]bool{}
	defer func() {
		for _, kind := range kinds {
			if !drawn[kind.name] {
				t.Logf("the series drew no change of the kind %q: its history admitted none", kind.name)
			}
		}
	}()
	for step := 1; step <= 14 || len(drawn) < len(kinds) && step <= 30; step++ {
		var name string
		var change func(*testing.T, *refresherRig)
		fresh := step > 14 || rng.IntN(2) == 0
		for _, onlyFresh := range []bool{fresh, false} {
			for _, k := range rng.Perm(len(kinds)) {
				if change != nil || onlyFresh && drawn[kinds[k].name] {
					continue
				}
				if change = kinds[k].draw(t, rng, live); change != nil {
					name = kinds[k].name
				}
			}
		}
		if change == nil {
			t.Fatalf("change %d: the history admits no change of any kind", step)
		}
		drawn[name] = true
		what := fmt.Sprintf("change %d, %s", step, name)
		change(t, live)
		report, _ := live.pass(t)
		if report.walksLeft {
			t.Errorf("%s: the pass left a walk with days to build", what)
		}
		walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
		change(t, walking)
		walking.settle(t)
		live.expectSameAsWalk(t, what, walking)
		if t.Failed() {
			t.Fatalf("%s: the series stops at its first change the pass did not rebuild as a whole walk does", what)
		}
		t.Logf("%s: one pass holds what the whole walk holds", what)
	}
}

// copyRunBack writes, as the run id of repo, a copy of the run from with its jobs, every time of
// them days days earlier; with fail, the copy's jobs named production and the run itself failed.
func (r *refresherRig) copyRunBack(t *testing.T, repo string, from, id int64, days int, production []string, fail bool) {
	t.Helper()
	r.exec(t, fmt.Sprintf("copy run %d as %d, %d days earlier", from, id, days), `
		insert into delivery_runs
		select (jsonb_populate_record(null::delivery_runs, to_jsonb(run) || jsonb_build_object(
			'run_id', $3::bigint, 'url', run.url || '-copy-' || $3::text,
			'started_at', run.started_at - make_interval(days => $4), 'completed_at', run.completed_at - make_interval(days => $4),
			'created_at', run.created_at - make_interval(days => $4), 'head_commit_at', run.head_commit_at - make_interval(days => $4),
			'conclusion', case when $5 then 'failure' else run.conclusion::text end))).*
		from delivery_runs run where run.repo = $1 and run.run_id = $2
	`, repo, from, id, days, fail)
	r.exec(t, fmt.Sprintf("copy the jobs of run %d as %d's", from, id), `
		insert into delivery_run_jobs
		select (jsonb_populate_record(null::delivery_run_jobs, to_jsonb(job) || jsonb_build_object(
			'run_id', $3::bigint,
			'started_at', job.started_at - make_interval(days => $4), 'completed_at', job.completed_at - make_interval(days => $4),
			'conclusion', case when $5 and job.name = any($6) then 'failure' else job.conclusion::text end))).*
		from delivery_run_jobs job where job.repo = $1 and job.run_id = $2
	`, repo, from, id, days, fail, production)
}

// The runs earlyDeploys writes.
const earlyFirstRun, earlyFailedRun, earlyNextRun = 9000001, 9000002, 9000003

// earlyDeploys writes, in the deploy repository, two days before the history's first, a deploy
// that reached production (earlyFirstRun) and one that failed (earlyFailedRun), and the day
// after them a deploy that reached production (earlyNextRun), each a copy of the history's first
// deploy that did, and answers the first of those two days: the start of the repository's
// history, which the deploy that failed holds once the first one's run is gone.
func (r *refresherRig) earlyDeploys(t *testing.T, deliveries deliveryseed.Fixture) time.Time {
	t.Helper()
	if len(deliveries.Applies) == 0 {
		t.Fatal("the history holds no deploy that reached production")
	}
	template := deliveries.Applies[0].RunID
	back := int(measures.Midnight(runStart(t, deliveries.Runs, template)).Sub(deliveries.From).Hours()/24) + 2
	repo, production := deliveries.DeployRepo, deliveries.Settings.ProductionJobNames
	r.copyRunBack(t, repo, template, earlyFirstRun, back, production, false)
	r.copyRunBack(t, repo, template, earlyFailedRun, back, production, true)
	r.copyRunBack(t, repo, template, earlyNextRun, back-1, production, false)
	return deliveries.From.AddDate(0, 0, -2)
}

// mergeBeforeHistory merges a population pull request of the deploy repository that names no
// issue at noon three days before early, the start of the repository's history, and answers the
// pull request as the history holds it. Merged then, it is an item of no day of the repository,
// and the first of the repository's deploys to complete ships it, which then counts, as every
// early deploy does, for the repository's owner key (owners.go, R15): the main project reads its
// rows, the deploy repository being its default, and the deploys' move between days moves no
// owner's first input day, as one through an issue would move the project's own.
func (r *refresherRig) mergeBeforeHistory(t *testing.T, deliveries deliveryseed.Fixture, early time.Time) delivery.DeliveryPullRequest {
	t.Helper()
	index := slices.IndexFunc(deliveries.PullRequests, func(pr delivery.DeliveryPullRequest) bool {
		return pr.Repo == deliveries.DeployRepo && pr.IssueKey == nil && pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial &&
			slices.Contains(deliveries.Settings.PopulationAuthors, pr.Author)
	})
	if index < 0 {
		t.Fatal("the history holds no merged pull request of the deploy repository that names no issue")
	}
	moved := deliveries.PullRequests[index]
	r.exec(t, "merge a pull request before the deploy repository's history", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour')
		where repo = $1 and number = $2
	`, moved.Repo, moved.Number, early.AddDate(0, 0, -3).Add(12*time.Hour))
	return moved
}

// deploysWithPRs is how many deploys with pull requests project's deploy-frequency counts on day
// in its all-repositories rows kept for repo, its own and those of the owner keys it reads
// (eval_project_owners, owners.go), added as a read adds them; -1 where it reads no such row.
func (r *refresherRig) deploysWithPRs(t *testing.T, project, repo string, day time.Time) int {
	t.Helper()
	return r.count(t, `
		select coalesce((select sum((d.counts ->> 'with_prs')::float)::int from eval_project_owners o join eval_days d on d.project_key = o.owner
			where o.project_key = $1 and d.eval_id = o.owner || '/' || $2 and d.deploy_repo = $3 and d.series = $4 and d.repo = '' and d.day = $5), -1)
	`, project, SlugDeployFrequency, repo, measures.SeriesDeploys, day)
}

// TestARunsRemovalRewritesTheDayOfTheDeployThatShipsItsPullRequestsNext: a deploy ships every pull
// request whose first shipping apply it is, whatever day the pull request merged, and a pull
// request merged before the deploy repository's history began is an item of no day, so no item
// names the deploy that ships it. Over a history whose deploy repository holds, two days before
// the rest, a deploy that reached production and one that failed, and the day after them a deploy
// that reached production (earlyDeploys), a pull request merged before them all ships with the
// first: that deploy is one with pull requests, and the one of the day after ships none, each
// counted, as the pull request names no issue, for the deploy repository's owner key, which the
// main project reads (mergeBeforeHistory, deploysWithPRs). The first deploy's run is then deleted.
// Its day still holds a deploy run of that key, so no series' first input day moves and the page
// builds no span; the pull request ships with the deploy of the day after, and the one pass writes
// that deploy's day too (refresh.shippedNeighbours), which no change names: it counts a deploy
// with pull requests, as a second database holds it whose run was deleted before its first walk
// (expectSameAsWalk). Before, that day kept counting none.
func TestARunsRemovalRewritesTheDayOfTheDeployThatShipsItsPullRequestsNext(t *testing.T) {
	t.Parallel()
	remove := func(t *testing.T, r *refresherRig, deliveries deliveryseed.Fixture) {
		t.Helper()
		r.exec(t, "delete the first deploy's jobs", `delete from delivery_run_jobs where repo = $1 and run_id = $2`, deliveries.DeployRepo, earlyFirstRun)
		r.exec(t, "delete the first deploy's run", `delete from delivery_runs where repo = $1 and run_id = $2`, deliveries.DeployRepo, earlyFirstRun)
	}
	live, deliveries := newSettingsHistory(t, walkBatch)
	early := live.earlyDeploys(t, deliveries)
	live.mergeBeforeHistory(t, deliveries, early)
	live.settle(t)
	main, repo := deliveries.Project, deliveries.DeployRepo
	if on, after := live.deploysWithPRs(t, main, repo, early), live.deploysWithPRs(t, main, repo, early.AddDate(0, 0, 1)); on != 1 || after != 0 {
		t.Fatalf("%s counts %d deploys with pull requests on %s and %d the day after, and the check needs 1 and 0: the first deploy ships the pull request merged before the history",
			main, on, early.Format(time.DateOnly), after)
	}
	if _, err := delivery.DrainPending(t.Context(), live.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	seq := live.latestSeq(t)
	remove(t, live, deliveries)
	report, _ := live.pass(t)
	if report.walksLeft {
		t.Errorf("the pass left a walk with days to build")
	}
	if _, err := delivery.DrainPending(t.Context(), live.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), live.database.Pool, seq, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", seq, err)
	}
	for _, change := range changes {
		if change.RunKind == delivery.EvalDaysWrittenByRebuild {
			t.Errorf("the pass built a span (the eval_days change of %s carries the rebuild's mark), and the check needs a page that builds none", change.Ref)
		}
	}
	if on, after := live.deploysWithPRs(t, main, repo, early), live.deploysWithPRs(t, main, repo, early.AddDate(0, 0, 1)); on != 0 || after != 1 {
		t.Errorf("%s counts %d deploys with pull requests on %s and %d the day after, want 0 and 1: the deploy of the day after ships the pull request now",
			main, on, early.Format(time.DateOnly), after)
	}
	walk, _ := newSettingsHistory(t, walkBatch)
	walk.mergeBeforeHistory(t, deliveries, walk.earlyDeploys(t, deliveries))
	remove(t, walk, deliveries)
	walk.settle(t)
	live.expectSameAsWalk(t, "the first deploy's run deleted", walk)
	t.Logf("the pass wrote %v", report.sorted())
}

// TestAPullRequestLeavingTheDaysBeforeItsRepositorysHistoryRewritesTheDeployThatShippedIt: over
// the same history (earlyDeploys), whose first deploy ships a pull request merged before the
// repository's history began, the pull request's merge moves to the moment the history holds it
// at, inside the repository's history. The deploy that ships it there is named by the pull
// request's new merge day; the first deploy, which shipped it and ships none now, was recorded
// with no item, so it is the pull request's merge before the change that names it
// (refresh.shippedNeighbours): its day counts no deploy with pull requests, as a second database
// holds it that never moved the merge (expectSameAsWalk).
func TestAPullRequestLeavingTheDaysBeforeItsRepositorysHistoryRewritesTheDeployThatShippedIt(t *testing.T) {
	t.Parallel()
	live, deliveries := newSettingsHistory(t, walkBatch)
	early := live.earlyDeploys(t, deliveries)
	moved := live.mergeBeforeHistory(t, deliveries, early)
	live.settle(t)
	main, repo := deliveries.Project, deliveries.DeployRepo
	if on := live.deploysWithPRs(t, main, repo, early); on != 1 {
		t.Fatalf("%s counts %d deploys with pull requests on %s, and the check needs 1: the first deploy ships the pull request merged before the history",
			main, on, early.Format(time.DateOnly))
	}
	live.exec(t, "merge the pull request when the history merges it", `
		update delivery_pull_requests set merged_at = $3, created_at = $4 where repo = $1 and number = $2
	`, moved.Repo, moved.Number, moved.MergedAt, moved.CreatedAt)
	report, _ := live.pass(t)
	if report.walksLeft {
		t.Errorf("the pass left a walk with days to build")
	}
	if on := live.deploysWithPRs(t, main, repo, early); on != 0 {
		t.Errorf("%s counts %d deploys with pull requests on %s, want 0: the first deploy ships no pull request now", main, on, early.Format(time.DateOnly))
	}
	walk, _ := newSettingsHistory(t, walkBatch)
	walk.earlyDeploys(t, deliveries)
	walk.settle(t)
	live.expectSameAsWalk(t, "the pull request merged inside the history again", walk)
	t.Logf("the pass wrote %v", report.sorted())
}

// TestTheReadsSettingsMoveWithTheCommitThatBringsTheRowsToThem: over a settled history, a save of
// the delivery settings that moves the deploy repository alone, which moves no row, and one that
// moves the production rule, whose rewrite moves rows, each leave the settings a read of the evals
// takes (DeployRepoIn's deploy repository, BuiltSettingsIn's every field eval_days_settings
// records) at the old settings until the refresher's pass commits, and at the new ones once it
// has: the record moves in the commit that brings the rows to the settings, not at the save. And
// no reader sees the one move without the other: at each commit of the pass, the rows, members,
// recorded settings and the answers of a first paint and an older window all agree with the
// state before the pass or the state after it. Raw stored figures can change during the refold.
func TestTheReadsSettingsMoveWithTheCommitThatBringsTheRowsToThem(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	read := func(what string) (string, delivery.DeliverySettings) {
		t.Helper()
		repo, err := DeployRepoIn(t.Context(), r.database.Pool)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		built, err := BuiltSettingsIn(t.Context(), r.database.Pool)
		if err != nil || built == nil {
			t.Fatalf("%s: the settings a read takes are %v (%v)", what, built, err)
		}
		return repo, *built
	}
	plain := r.registry
	prints := &commitPrints{t: t, rig: r.rig, snapshot: func() string {
		return readSettingsSnapshot(t, plain, deliveries.Project, deliveries.DeployRepo)
	}}
	rows := &commitPrints{t: t, rig: r.rig}
	r.registry = r.tracedStore(t, prints)
	settings := deliveries.Settings
	for _, change := range []struct {
		name   string
		change func(*delivery.DeliverySettings)
		rows   bool
	}{
		{"the deploy repository moved alone", func(s *delivery.DeliverySettings) { s.DeployRepo = deliveries.Repos[1] }, false},
		{"the production rule moved", func(s *delivery.DeliverySettings) { s.ProductionJobNames = []string{"release / a job no run holds"} }, true},
		{"both moved back", func(s *delivery.DeliverySettings) { *s = deliveries.Settings }, true},
	} {
		was := settings
		change.change(&settings)
		r.putSettings(t, settings, alice)
		if repo, built := read(change.name + ", before the pass"); repo != was.DeployRepo || !sameBuiltSettings(built, was) {
			t.Errorf("%s: before the pass a read takes the deploy repository %s and the production jobs %v, want the old ones, %s and %v",
				change.name, repo, built.ProductionJobNames, was.DeployRepo, was.ProductionJobNames)
		}
		rowsBefore, before := rows.read(), prints.read()
		r.pass(t)
		expectTruth(t, change.name, r.storedFigures(t), r.truthFigures(t))
		after := prints.read()
		if repo, built := read(change.name + ", after the pass"); repo != settings.DeployRepo || !sameBuiltSettings(built, settings) {
			t.Errorf("%s: after the pass a read takes the deploy repository %s and the production jobs %v, want the new ones, %s and %v",
				change.name, repo, built.ProductionJobNames, settings.DeployRepo, settings.ProductionJobNames)
		}
		if moved := rows.read() != rowsBefore; moved != change.rows {
			t.Errorf("%s: the pass moved the rows, the members or the figures is %t, want %t", change.name, moved, change.rows)
		}
		if states := prints.others(before, after); states > 0 {
			t.Errorf("%s: a reader between two commits of the pass saw %d states of the rows and the settings that are neither the one before it nor the one after it",
				change.name, states)
		}
	}
}

// TestASettingsChangeSaysTheRowsStandAtItWhereItMovesTheDeployRepository: over a settled history,
// the pass after a save of the delivery settings logs the eval_days change of no eval, the word
// that the rows stand at the settings (delivery.SettingsApplied), which every evals view is
// recomputed for, only where the save moved the deploy repository, the one recorded setting a read
// of the evals takes (DeployRepoIn): none after the production rule moved, whose rewrite moves rows
// and logs a change of each eval it moved, under that eval's project; exactly one after the deploy
// repository moved alone, which moves no row; and exactly one after both moved back together,
// beside the rewrite's own changes. After each, the stored figures are the fold of the day rows.
func TestASettingsChangeSaysTheRowsStandAtItWhereItMovesTheDeployRepository(t *testing.T) {
	t.Parallel()
	r, deliveries := newSettingsRig(t, walkBatch)
	drain := func(what string) {
		t.Helper()
		if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
			t.Fatalf("%s: drain the change log's outbox: %v", what, err)
		}
	}
	settings := deliveries.Settings
	for _, change := range []struct {
		name    string
		change  func(*delivery.DeliverySettings)
		applied int
		rows    bool
	}{
		{"the production rule moved", func(s *delivery.DeliverySettings) { s.ProductionJobNames = []string{"release / a job no run holds"} }, 0, true},
		{"the deploy repository moved alone", func(s *delivery.DeliverySettings) { s.DeployRepo = deliveries.Repos[1] }, 1, false},
		{"both moved back", func(s *delivery.DeliverySettings) { *s = deliveries.Settings }, 1, true},
	} {
		change.change(&settings)
		drain(change.name + ", before the save")
		seq := r.latestSeq(t)
		r.putSettings(t, settings, alice)
		r.pass(t)
		drain(change.name)
		changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 100000)
		if err != nil {
			t.Fatalf("%s: read the delivery changes after %d: %v", change.name, seq, err)
		}
		applied, ofEvals := 0, 0
		for _, one := range changes {
			switch {
			case one.Kind != delivery.ChangeKindEvalDays:
			case delivery.SettingsApplied(one):
				applied++
			case delivery.SettingsRecorded(one):
			default:
				ofEvals++
			}
		}
		if applied != change.applied {
			t.Errorf("%s: the pass logged %d eval_days changes of no eval, want %d", change.name, applied, change.applied)
		}
		if (ofEvals > 0) != change.rows {
			t.Errorf("%s: the pass logged %d eval_days changes of an eval, and the change moves rows is %t", change.name, ofEvals, change.rows)
		}
		expectTruth(t, change.name, r.storedFigures(t), r.truthFigures(t))
		t.Logf("%s: the pass logged %d eval_days changes of an eval and %d of no eval", change.name, ofEvals, applied)
	}
}
