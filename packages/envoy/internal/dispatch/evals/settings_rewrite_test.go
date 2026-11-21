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
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The checks of the refresher's answer to a change of the delivery settings (settings_rewrite.go):
// one pass rewrites what the change moves as the whole delivery walk would write it, and begins no
// walk; a change that moves no eval writes nothing; and the members write sends only the members
// that moved.

// settingsApplyStep is the apply step newSettingsRig records on the production jobs.
const settingsApplyStep = "terraform apply"

// newSettingsRig is a refresher over a database of its own loaded with deliverytest's history of
// five days and settled by its passes, each walking batch days. Every production job that succeeded
// records an apply step, settingsApplyStep, which succeeded but on the runs whose id ends in 3,
// where it was skipped; no setting names the step, so each job counts by its own conclusion.
func newSettingsRig(t *testing.T, batch int) (*refresherRig, deliverytest.Fixture) {
	t.Helper()
	r := newRefresherRig(t)
	r.batch = batch
	deliveries := deliverytest.Seed(t, r.database.Pool, deliverytest.Spec{Days: 5})
	r.exec(t, "record the production jobs' apply step", `
		update delivery_run_jobs
		set apply_step = $2, apply_step_conclusion = case when run_id % 10 = 3 then 'skipped' else 'success' end
		where repo = $1 and name = any($3) and conclusion = 'success'
	`, deliveries.DeployRepo, settingsApplyStep, deliveries.Settings.ProductionJobNames)
	r.settle(t)
	return r, deliveries
}

// putSettings saves settings as actor, as PUT /api/v1/settings/delivery does.
func (r *refresherRig) putSettings(t *testing.T, settings delivery.DeliverySettings, actor model.Actor) {
	t.Helper()
	if _, err := delivery.PutSettings(t.Context(), r.database.Pool, settings, actor); err != nil {
		t.Fatalf("save the delivery settings: %v", err)
	}
}

// isDeliveryEval reports whether evalID is a delivery or CI eval's, one a delivery day writes.
func isDeliveryEval(evalID string) bool {
	_, slug, _ := strings.Cut(evalID, "/")
	return slices.Contains(deliveryDaySlugs, slug)
}

// deliveryValues is every stored row of a delivery or CI eval, by rowKey, without the moment and
// the build of its last write: what two databases' rows compare by.
func (r *refresherRig) deliveryValues(t *testing.T) map[string]DayRow {
	t.Helper()
	values := map[string]DayRow{}
	for key, row := range r.dayRows(t) {
		if isDeliveryEval(row.EvalID) {
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

// deliveryFigureValues is the fold key and figures of each stored figures row of a delivery or CI
// eval, by its line: what two databases' figures compare by, since the oldest updated_at and the
// stamp a row records are each database's own moments.
func deliveryFigureValues(stored map[string]figureRow) map[string]string {
	values := map[string]string{}
	for line, row := range stored {
		if isDeliveryEval(line[strings.LastIndex(line, " ")+1:]) {
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

// walkXmin is the transaction that last wrote the rig's build's record of the delivery walk, which
// a walk begun again rewrites.
func (r *refresherRig) walkXmin(t *testing.T) string {
	t.Helper()
	var xmin string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select xmin::text from eval_days_walks where family = $1 and project_key = '' and built_by = $2
	`, walkDelivery, r.build).Scan(&xmin); err != nil {
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

// changedEvals is how many eval_days changes each eval logged after seq.
func (r *refresherRig) changedEvals(t *testing.T, seq int64) map[string]int {
	t.Helper()
	changed := map[string]int{}
	for _, line := range r.evalDayChanges(t, seq) {
		evalID, _, _ := strings.Cut(line, " ")
		changed[evalID]++
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

// expectSameAsWalk checks that r holds the delivery and CI day rows, the members and the stored
// figures walk holds, and that r's stored figures are the fold of its rows.
func (r *refresherRig) expectSameAsWalk(t *testing.T, what string, walk *refresherRig) {
	t.Helper()
	if differ := sameValues(r.deliveryValues(t), walk.deliveryValues(t), rowFacts); len(differ) > 0 {
		t.Errorf("%s: the day rows differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
	if differ := sameValues(r.memberLines(t), walk.memberLines(t), func(at string) string { return at }); len(differ) > 0 {
		t.Errorf("%s: the members differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
	stored := r.storedFigures(t)
	expectTruth(t, what, stored, r.truthFigures(t))
	if differ := sameValues(deliveryFigureValues(stored), deliveryFigureValues(walk.storedFigures(t)), func(figures string) string { return figures }); len(differ) > 0 {
		t.Errorf("%s: the stored figures differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
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
// every eval whose rows moved is one settingsMoves names; and each has exactly one eval_days
// change of the pass, and no other eval one, so an evals view hears of the change once. Then the
// deploy repository is moved to one with pull requests and no run, and back: the pass writes no
// row and no member, logs no eval_days change and begins no walk, the read of the evals keyed by
// the deploy repository answers none of their rows and then the old repository's again, and the
// first still holds what the second's whole walk under the new settings holds.
func TestASettingsChangeRewritesInOnePassWhatAWholeWalkWrites(t *testing.T) {
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
		if _, next := rewriting.pass(t); next.Equal(rewriting.now) {
			t.Errorf("%s: the pass asked for another at once, which a walk with days left does", step.name)
		}
		if rewriting.walkXmin(t) != walk {
			t.Errorf("%s: the pass began the delivery walk again", step.name)
		}
		moved, changed := movedEvals(before, rewriting.dayRows(t)), rewriting.changedEvals(t, seq)
		if len(moved) == 0 {
			t.Errorf("%s: no eval's rows moved, and the change moves %v", step.name, slugs)
		}
		for _, evalID := range moved {
			if _, slug, _ := strings.Cut(evalID, "/"); !slices.Contains(slugs, slug) {
				t.Errorf("%s: %s's rows moved, and the change moves %v alone", step.name, evalID, slugs)
			}
		}
		if got := slices.Sorted(maps.Keys(changed)); !slices.Equal(got, moved) || slices.ContainsFunc(got, func(evalID string) bool { return changed[evalID] != 1 }) {
			t.Errorf("%s: the pass logged the eval_days changes %v, want one of each eval whose rows moved, %v", step.name, changed, moved)
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
		if _, next := rewriting.pass(t); next.Equal(rewriting.now) {
			t.Errorf("%s: the pass asked for another at once, which a walk with days left does", move.name)
		}
		show := func(xmin string) string { return "written by " + xmin }
		if differ := sameValues(rewriting.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("%s: the pass wrote day rows, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(rewriting.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("%s: the pass wrote members, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if changed := rewriting.evalDayChanges(t, seq); len(changed) > 0 {
			t.Errorf("%s: the pass logged the eval_days changes %v, want none", move.name, changed)
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

// dayRowXmins and memberXmins read the transaction that last wrote each day row and each member,
// by its key (xmins).
const (
	dayRowXmins = `select project_key || ' ' || eval_id || ' ' || deploy_repo || ' ' || series || ' ' || repo || ' ' || day::text, xmin::text from eval_days`
	memberXmins = `select project_key || ' ' || family || ' ' || deploy_repo || ' ' || row_key, xmin::text from eval_day_members`
)

// keyedRead is the day rows the eval list reads of project's evals keyed by the deploy repository
// where the delivery settings name deployRepo (DayWindow over every day through today), each as
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
	rows, err := r.registry.DayWindow(t.Context(), []string{project}, keyed, deployRepo, nil,
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

// TestASettingsChangeThatMovesNoEvalWritesNothing: over a settled history, each change of a setting
// no day row reads (the two workflow paths, the population rule's authors and excluded
// repositories, the roll-up job), and a production job name no run holds, listed beside the one
// that is, logs its settings change, and the pass after it builds no delivery day (its report
// holds none: the rewrite records there each day it builds and writes, as the walk does), writes
// no day row, member or stored figure, logs no eval_days change, begins no walk, and leaves the
// settings the rows stand built under agreeing with the new ones on every field a day row reads.
// The same settings saved again by another person log no change at all (migration
// 20261120111107), and the pass after them writes nothing either.
func TestASettingsChangeThatMovesNoEvalWritesNothing(t *testing.T) {
	r, deliveries := newSettingsRig(t, walkBatch)
	settings := deliveries.Settings
	for _, change := range []struct {
		name   string
		change func(*delivery.DeliverySettings)
		actor  model.Actor
		logs   bool
	}{
		{"the deploy workflow's path", func(s *delivery.DeliverySettings) { s.DeployWorkflowPath = ".github/workflows/deploy-next.yml" }, alice, true},
		{"the PR-checks workflow's path", func(s *delivery.DeliverySettings) { s.PRChecksWorkflowPath = ".github/workflows/pr-checks-next.yml" }, alice, true},
		{"one population author fewer", func(s *delivery.DeliverySettings) {
			s.PopulationAuthors = s.PopulationAuthors[:len(s.PopulationAuthors)-1]
		}, alice, true},
		{"one excluded repository more", func(s *delivery.DeliverySettings) { s.ExcludedRepos = []string{"acme/elsewhere"} }, alice, true},
		{"the roll-up job renamed", func(s *delivery.DeliverySettings) { s.PRChecksRollupJobName = new("checks-complete-v2") }, alice, true},
		{"a production job name no run holds", func(s *delivery.DeliverySettings) {
			s.ProductionJobNames = append(slices.Clone(s.ProductionJobNames), "production-release / apply-v2")
		}, alice, true},
		{"the same settings saved by another person", func(*delivery.DeliverySettings) {}, implementer, false},
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
		if changed := r.evalDayChanges(t, seq); len(changed) > 0 {
			t.Errorf("%s: the pass logged the eval_days changes %v, want none", change.name, changed)
		}
		if r.walkXmin(t) != walk {
			t.Errorf("%s: the pass began the delivery walk again", change.name)
		}
		recorded, err := r.registry.readBuiltUnder(t.Context())
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

// TestOnlyTheSettingsRewritesEvalDaysChangesCarryItsMark: over a settled history, the eval_days
// changes of the passes that walked it carry no cause in run_kind; the pass after the apply step
// is named rewrites the evals that moves, and each eval_days change it logs carries the rewrite's
// mark (delivery.EvalDaysWrittenBySettingsRewrite), for which the Delivery page's live hub
// recomputes an evals view at once; and the ordinary pass after a merged pull request's opening
// moves an hour earlier rewrites its merge day and logs eval_days changes that carry none. Each
// pass's changes are read from the change log once a drain has moved the outbox's rows there.
func TestOnlyTheSettingsRewritesEvalDaysChangesCarryItsMark(t *testing.T) {
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
// the pool it traces: delivery.ReadHistoryRows' read of a repository's jobs, the one statement of
// delivery_run_jobs that names the deploy runs on main by a subquery, with the job names it asks
// for and the rows it answers.
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
	if !strings.Contains(data.SQL, "from delivery_run_jobs") || !strings.Contains(data.SQL, "head_branch = 'main'") || len(data.Args) < 2 {
		return ctx
	}
	names, ok := data.Args[1].([]string)
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
// the production rule and its card, and the read asks for no name a staging card follows, so it
// answers no staging card's job. The staging gate renamed moves the gate alone, and the read asks
// for its old and new names and the production rule's, whose verdicts decide which projects each
// deploy counts for and so which rows each project's day holds, and not the staging deploy's.
// Each read is logged with the rows it answered beside those a read of both rules' names and of
// every card's under both settings answers.
func TestASettingsChangeReadsTheJobsOfTheCardsItMovesAlone(t *testing.T) {
	r, deliveries := newSettingsRig(t, walkBatch)
	reads := &historyJobReads{}
	r.registry = New(storetest.TracedStore(t, r.database, reads), r.documents, r.registry.events)
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
			slices.Concat(leadTimeSlugs, []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugProductionDeploy}),
			original.ProductionJobNames, []string{gate, deploy}},
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
	r.exec(t, "copy the "+string(kind)+" runs of "+from+" to "+to, `
		insert into delivery_runs
		select (jsonb_populate_record(null::delivery_runs, to_jsonb(run) || jsonb_build_object('repo', $2::text))).*
		from delivery_runs run where run.repo = $1 and run.kind = $3`, from, to, string(kind))
	r.exec(t, "copy the jobs of those runs", `
		insert into delivery_run_jobs
		select (jsonb_populate_record(null::delivery_run_jobs, to_jsonb(job) || jsonb_build_object('repo', $2::text))).*
		from delivery_run_jobs job join delivery_runs run on run.repo = job.repo and run.run_id = job.run_id
		where job.repo = $1 and run.kind = $3`, from, to, string(kind))
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
	rigs := make([]*refresherRig, 2)
	var deliveries deliverytest.Fixture
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

// deliveryState is what a database's delivery and CI evals hold: their day rows (deliveryValues),
// the members and their stored figures (deliveryFigureValues), none with the moment or the build of
// its write.
type deliveryState struct {
	rows    map[string]DayRow
	members map[string]string
	figures map[string]string
}

func (r *refresherRig) deliveryState(t *testing.T) deliveryState {
	t.Helper()
	return deliveryState{rows: r.deliveryValues(t), members: r.memberLines(t), figures: deliveryFigureValues(r.storedFigures(t))}
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
func newRuleChangeRig(t *testing.T) (*refresherRig, deliverytest.Fixture) {
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
// databases load one history under a production rule whose one job no run holds, and walk it
// whole: no deploy reaches production and no pull request ships, so the deploy repository's lead
// times hold no row and no first day. The rule then names the history's production job, under which
// the repository's first pull request ships first: containment ships every pull request merged
// before an apply's creation, so a rule that ships any ships the first, and the first shipped pull
// request moves earlier only from none. One pass of the first rewrites the change, and the lead
// times' rows of every day from the new first day up to the old one, which is none, so through
// today, are the second's whole walk's: today's zero rows among them, which no input of today moves
// (movedDeliveryDays' first days). The first then holds what the second's whole walk holds.
func TestAProductionRuleThatShipsAnEarlierPullRequestFirstRewritesTheDaysUpToTheOldFirstDay(t *testing.T) {
	rigs := make([]*refresherRig, 2)
	var deliveries deliverytest.Fixture
	for i := range rigs {
		rigs[i] = newRefresherRig(t)
		deliveries = deliverytest.Seed(t, rigs[i].database.Pool, deliverytest.Spec{Days: 5})
		none := deliveries.Settings
		none.ProductionJobNames = []string{"production-release / apply-v0"}
		rigs[i].putSettings(t, none, alice)
		rigs[i].settle(t)
	}
	rewriting, walking := rigs[0], rigs[1]
	leadTimes := func(r *refresherRig) map[string]DayRow {
		rows := map[string]DayRow{}
		for key, row := range r.deliveryValues(t) {
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
// of each is the same under both; the first then holds what the second's whole walk holds.
func TestAStagingGateRenamedToAJobSomePushRunsHoldRewritesWhatAWholeWalkWrites(t *testing.T) {
	const legacy, renamed = "staging-e2e / legacy", "staging-e2e / run-v2"
	rigs := make([]*refresherRig, 2)
	var deliveries deliverytest.Fixture
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
	written := len(movedRows(before, rewriting.dayRows(t)))
	walking.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walking.settle(t)
	rewriting.expectSameAsWalk(t, "the staging gate renamed to a job some push runs hold", walking)
	t.Logf("the staging gate renamed to %s: the pass built the days %v and wrote %d day rows", renamed, built, written)
}

// TestAParallelFoldOfTheStoredFiguresWritesWhatOneWorkerWrites: over a settled history of thirty
// days, foldMovedDays folds the stored figures of every day of every eval and repository of a
// project that holds a listed row, once on one worker and once on figureFoldWorkers, and the two
// answer the same rows, byte for byte, in the order writeFigures sends them (sortByKey): a group's
// folds share its rows and parts and write into neither (figurePlan.fold). Run it under -race, by
// name, which fails a fold that writes into what the others read.
func TestAParallelFoldOfTheStoredFiguresWritesWhatOneWorkerWrites(t *testing.T) {
	r := newRefresherRig(t)
	deliveries := deliverytest.Seed(t, r.database.Pool, deliverytest.Spec{Days: 30})
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
		select distinct eval_id, deploy_repo, repo, day from eval_days where project_key = $1 and listed and day < $2
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
		ranges[figureGroupKey{eval: row.eval, deployRepo: row.deployRepo, repo: row.repo}]++
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
