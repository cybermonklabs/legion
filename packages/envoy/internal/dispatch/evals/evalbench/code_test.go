package evalbench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival"
	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// codeNow is the midnight the code survival histories of these tests end at.
var codeNow = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

// codeMarkDays is how long after a change a commit's removal of its lines is a pair of it
// (survival.Mark).
const codeMarkDays = 30

// codeIssues is a history of two INFRA issues opened a day, each closed five days after it opens but
// every third, which the draws of an issue open at a commit's moment (issueOpenAt) read.
func codeIssues(days int) projectHistory {
	var history projectHistory
	for i := range 2 * days {
		opened := codeNow.AddDate(0, 0, -codeDays+i/2).Add(time.Duration(i%2) * 6 * time.Hour)
		issue := plannedIssue{number: i + 1, key: fmt.Sprintf("INFRA-%d", i+1), opened: opened}
		if i%3 != 0 {
			closed := opened.Add(5 * 24 * time.Hour)
			issue.closed = &closed
		}
		history.issues = append(history.issues, issue)
	}
	return history
}

// near reports whether got is within share of want.
func near(got, want, share float64) bool { return math.Abs(got-want) <= share*want }

// TestTheCodeHistoryHoldsTheMeasuredRates: at INFRA's stated rate the plan holds the rates of
// cybermonklabs/legion's reference window, 63.4 first-parent commits a day, 1,962 changes in 3,356
// commits, 905 of the changes the agents', and 27,967 pairs in 90 days, over the 365 days before
// Now's, of which the days that began to wait by Now's day are measured and no more.
func TestTheCodeHistoryHoldsTheMeasuredRates(t *testing.T) {
	t.Parallel()
	spec := Spec{Now: codeNow, Seed: 2916, Scale: 1}
	plan := planCode(spec, codeIssues(codeDays))

	if want := 63.4 * codeDays; !near(float64(len(plan.chain)), want, 0.001) {
		t.Errorf("the chain holds %d commits, want %.0f at 63.4 a day for %d days", len(plan.chain), want, codeDays)
	}
	if share := float64(len(plan.changes)) / float64(len(plan.chain)); !near(share, 1962.0/3356.0, 0.03) {
		t.Errorf("%d of %d commits are changes (%.4f), want 1,962 of 3,356 (%.4f)", len(plan.changes), len(plan.chain), share, 1962.0/3356.0)
	}
	if want := plan.first.AddDate(0, 0, codeDays); !want.Equal(codeNow) {
		t.Errorf("the chain starts at %s, want the 365 days before %s", plan.first, codeNow)
	}
	if got := plan.measuredDays; got != codeDays-32 {
		t.Errorf("%d days are measured, want the %d that began to wait by %s (day + 33 days)", got, codeDays-32, codeNow)
	}
	newest := plan.first.AddDate(0, 0, plan.measuredDays-1)
	if survival.WaitsFrom(newest).After(codeNow) || !survival.WaitsFrom(newest.AddDate(0, 0, 1)).After(codeNow) {
		t.Errorf("the newest measured day %s is not the newest day that waited by %s", newest, codeNow)
	}
	if len(plan.fingerprints) != codeDays {
		t.Fatalf("%d fingerprints, want one for each of %d days", len(plan.fingerprints), codeDays)
	}
	commits := 0
	for _, fingerprint := range plan.fingerprints {
		commits += fingerprint.Commits
	}
	if commits != len(plan.chain) {
		t.Errorf("the fingerprints count %d commits, the chain holds %d", commits, len(plan.chain))
	}

	// The pairs of the 90 newest measured days.
	pairs, agents := 0, 0
	for _, pair := range plan.pairs {
		if survival.DayOf(plan.changes[pair.adder].committed).After(newest.AddDate(0, 0, -90)) {
			pairs++
		}
	}
	if !near(float64(pairs), 27_967, 0.03) {
		t.Errorf("%d pairs in the 90 newest measured days, want about 27,967", pairs)
	}

	// Every way a group takes a change is held, and the agents' share is the reference window's.
	groups := delivery.SurvivalGroups(codeGroups)
	ways := map[string]int{}
	for _, change := range plan.changes {
		group, ok := survival.GroupOf(survival.ChangeFacts{PR: change.pr, HeadRef: change.headRef, Author: change.author, IssueKey: change.issue}, groups)
		if !ok {
			t.Fatalf("no group takes the change of %s by %s", change.headRef, change.author)
		}
		prefix := strings.HasPrefix(change.headRef, codeGroups[0].BranchPrefixes[0])
		login := slices.Contains(codeGroups[0].AuthorLogins, change.author)
		way := map[[2]bool]string{{true, true}: "prefix and login", {true, false}: "prefix alone", {false, true}: "login alone", {false, false}: "neither"}[[2]bool{prefix, login}]
		ways[group+" by "+way]++
		if group == codeGroups[0].Name {
			agents++
		}
	}
	for _, way := range []string{"Legion by prefix and login", "Legion by prefix alone", "Legion by login alone", "Other by neither"} {
		if ways[way] == 0 {
			t.Errorf("no change is taken as %q: %v", way, ways)
		}
	}
	if share := float64(agents) / float64(len(plan.changes)); !near(share, 905.0/1962.0, 0.05) {
		t.Errorf("%d of %d changes are the agents' (%.4f), want 905 of 1,962 (%.4f)", agents, len(plan.changes), share, 905.0/1962.0)
	}
	t.Logf("%d commits, %d changes (%d the agents'), %d of them measured, %d pairs in all and %d in the 90 newest measured days, %d measured days, %d days in all",
		len(plan.chain), len(plan.changes), agents, plan.measured, len(plan.pairs), pairs, plan.measuredDays, codeDays)
}

// TestEveryRowOfTheCodeHistoryHoldsTheTablesChecks: the plan's rows are the ones the tables hold
// (migration 20261126103753): a 40-character SHA, a positive pull request, lines that fit in what
// the change added, which is exactly what was alive at its mark and what was removed before it, a
// pair of a measured adder and a later remover within the 30 days of its mark, and no pair twice.
func TestEveryRowOfTheCodeHistoryHoldsTheTablesChecks(t *testing.T) {
	t.Parallel()
	for _, scale := range []float64{1, 0.05, 0.02} {
		t.Run(fmt.Sprintf("scale %g", scale), func(t *testing.T) {
			t.Parallel()
			plan := planCode(Spec{Now: codeNow, Seed: 7, Scale: scale}, codeIssues(codeDays))
			if plan.measured == 0 || len(plan.pairs) == 0 {
				t.Fatalf("the plan holds %d measured changes and %d pairs", plan.measured, len(plan.pairs))
			}
			shaShape := regexp.MustCompile(`^[0-9a-f]{40}$`)
			removed := make([]int, len(plan.changes))
			seen := map[[2]int]bool{}
			for _, pair := range plan.pairs {
				if pair.adder >= plan.measured || pair.remover <= pair.adder || pair.remover >= len(plan.changes) {
					t.Fatalf("a pair of the adder %d and the remover %d, of %d measured changes among %d", pair.adder, pair.remover, plan.measured, len(plan.changes))
				}
				if key := [2]int{pair.adder, pair.remover}; seen[key] {
					t.Fatalf("the pair of the adder %d and the remover %d is written twice", pair.adder, pair.remover)
				} else {
					seen[key] = true
				}
				if pair.lines <= 0 || pair.daysBetween <= 0 || pair.daysBetween > codeMarkDays {
					t.Fatalf("a pair of %d lines %g days apart", pair.lines, pair.daysBetween)
				}
				removed[pair.adder] += pair.lines
			}
			for i, change := range plan.changes {
				switch {
				case !shaShape.MatchString(change.sha):
					t.Fatalf("the change %d has the SHA %q", i, change.sha)
				case change.pr <= 0 || change.headRef == "" || change.author == "":
					t.Fatalf("the change %d has the pull request %d on %q by %q", i, change.pr, change.headRef, change.author)
				case change.issue != "" && !strings.HasPrefix(change.issue, "INFRA-"):
					t.Fatalf("the change %d names the issue %q", i, change.issue)
				case i < plan.measured && change.alive+change.removedByNonPR+removed[i] != change.added:
					t.Fatalf("the change %d added %d lines, and %d were alive, %d removed by no pull request and %d by pairs", i, change.added, change.alive, change.removedByNonPR, removed[i])
				case change.alive < 0 || change.removedByNonPR < 0:
					t.Fatalf("the change %d has %d alive and %d removed by no pull request", i, change.alive, change.removedByNonPR)
				}
				if i > 0 && change.committed.Before(plan.changes[i-1].committed) {
					t.Fatalf("the change %d is committed before the change before it", i)
				}
			}
		})
	}
}

// TestTheCodeHistoryIsTheSameForOneSeedAndMovesWithAnother: two plans of one seed hold the same
// commits, changes and pairs, and another seed holds others, so a base run and a head run over one
// -seed read one history.
func TestTheCodeHistoryIsTheSameForOneSeedAndMovesWithAnother(t *testing.T) {
	t.Parallel()
	plan := func(seed uint64) codePlan {
		return planCode(Spec{Now: codeNow, Seed: seed, Scale: 0.05}, codeIssues(codeDays))
	}
	first, again, other := plan(2916), plan(2916), plan(2917)
	if !reflect.DeepEqual(first, again) {
		t.Error("two plans of one seed differ")
	}
	if reflect.DeepEqual(first.chain, other.chain) {
		t.Error("two seeds drew one chain")
	}
}

// TestNoCodeAndTheBaseModesWriteNoCodeResults: the load writes the code survival results unless a
// load leaves them out, and a load of the pre-registry base or of the one-project fixture writes
// none, whatever the spec says.
func TestNoCodeAndTheBaseModesWriteNoCodeResults(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		spec Spec
		code bool
	}{
		{Spec{}, true},
		{Spec{NoCode: true}, false},
		{Spec{Base: true}, false},
		{Spec{OneProject: true}, false},
		{Spec{Base: true, NoCode: true}, false},
	} {
		if got := row.spec.WritesCode(); got != row.code {
			t.Errorf("%+v writes the code survival results: %t, want %t", row.spec, got, row.code)
		}
	}
}

// codeRig is a database with INFRA written as far as the code survival results need it: its
// project, issues and delivery settings.
func codeRig(t *testing.T, spec Spec) (*store.Store, projectHistory) {
	t.Helper()
	database := storetest.Open(t)
	ctx := t.Context()
	history, err := writeProject(ctx, database.Pool, spec, infraFixture)
	if err != nil {
		t.Fatalf("write INFRA's issues: %v", err)
	}
	if err := putDeliverySettings(ctx, database.Pool); err != nil {
		t.Fatalf("write INFRA's delivery settings: %v", err)
	}
	return database, history
}

// TestTheCodeResultsAreWrittenAndTheRegroupBuildsINFRAsDayRowsFromThem: writeCode stores INFRA's
// two code groups in its delivery settings, the repository checked on Now's day, a row of every
// measured day and of the 30 days after the newest, the changes and the pairs, and leaves
// code_projects empty; and a regroup pass of the project, with no GitHub client and no App, builds
// the day rows of both code evals for every measured day from those rows, stores the groups'
// digest, and then finds nothing left to build. That is the server's start in the bench.
func TestTheCodeResultsAreWrittenAndTheRegroupBuildsINFRAsDayRowsFromThem(t *testing.T) {
	spec := Spec{Now: codeNow, Seed: 2916, Scale: 0.02, BuiltBy: "evalbench-test"}
	rig, history := codeRig(t, spec)
	ctx, pool := t.Context(), rig.Pool
	if err := writeCode(ctx, pool, spec, history); err != nil {
		t.Fatalf("write the code survival results: %v", err)
	}
	plan := planCode(spec, history)

	counts, err := count(ctx, pool, countedTables)
	if err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	for table, want := range map[string]int64{
		"code_repos": 1, "code_days": int64(plan.measuredDays + codeTailDays), "code_changes": int64(plan.measured),
		"code_pairs": int64(len(plan.pairs)), "code_projects": 0,
	} {
		if counts[table] != want {
			t.Errorf("%s holds %d rows, want %d", table, counts[table], want)
		}
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `select code_groups from project_delivery_settings where project_key = $1`, Project).Scan(&stored); err != nil {
		t.Fatalf("read INFRA's code groups: %v", err)
	}
	var groups []delivery.CodeGroup
	if err := json.Unmarshal(stored, &groups); err != nil || !reflect.DeepEqual(groups, codeGroups) {
		t.Errorf("INFRA's settings hold the code groups %s (%v), want %+v", stored, err, codeGroups)
	}
	var checkedOn, lastMeasuredOn, startDay time.Time
	var state string
	if err := pool.QueryRow(ctx, `select state, checked_on, last_measured_on, start_day from code_repos where repo = $1`, strings.ToLower(Repos[0])).
		Scan(&state, &checkedOn, &lastMeasuredOn, &startDay); err != nil {
		t.Fatalf("read the repository's row: %v", err)
	}
	if state != "readable" || !checkedOn.Equal(codeNow) || !lastMeasuredOn.Equal(codeNow) || !startDay.Equal(plan.first) {
		t.Errorf("the repository is %s, checked on %s, last measured on %s, from %s; want readable, checked and measured on %s, from %s",
			state, checkedOn, lastMeasuredOn, startDay, codeNow, plan.first)
	}
	var measured, replayed int
	if err := pool.QueryRow(ctx, `select count(*) filter (where state = 'measured'), count(*) filter (where state = 'replayed') from code_days`).Scan(&measured, &replayed); err != nil {
		t.Fatalf("count the days: %v", err)
	}
	if measured != plan.measuredDays || replayed != codeTailDays {
		t.Errorf("%d measured and %d replayed days, want %d and %d", measured, replayed, plan.measuredDays, codeTailDays)
	}

	// The regroup, as the server's start runs it: INFRA's evals are seeded, and no GitHub is
	// reached, since the regroup holds no client.
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: rig, Events: broker, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	if _, err := evals.New(rig, documents, broker, delivery.NewAttributor(refs.Renames{})).Ensure(ctx, Project); err != nil {
		t.Fatalf("seed INFRA's evals: %v", err)
	}
	regroup := &codesurvival.Regroup{Pool: pool, Build: "evalbench-test", Now: func() time.Time { return codeNow.Add(time.Hour) }}
	if retry := regroup.Pass(ctx); !retry.IsZero() {
		t.Fatalf("the regroup pass asks for another at %s, so it left a project unbuilt", retry)
	}
	built, err := codesurvival.ProjectDigest(ctx, pool, Project)
	if err != nil || !built.Built() {
		t.Fatalf("INFRA's day rows are not built after the regroup: %+v, %v", built, err)
	}
	for _, slug := range []string{evals.SlugCodeSurvival, evals.SlugPostMergeRework} {
		var days, rows int
		if err := pool.QueryRow(ctx, `select count(distinct day), count(*) from eval_days where project_key = $1 and eval_id = $2 and repo = ''`,
			Project, evals.EvalID(Project, slug)).Scan(&days, &rows); err != nil {
			t.Fatalf("count the day rows of %s: %v", slug, err)
		}
		if days != plan.measuredDays || rows <= days {
			t.Errorf("%s holds %d rows on %d days, want rows of every one of the %d measured days, each with its group series", slug, rows, days, plan.measuredDays)
		}
	}
	if retry := regroup.Pass(ctx); !retry.IsZero() {
		t.Errorf("a second regroup pass asks for another at %s", retry)
	}
}

// TestTheServersRunAsksGitHubForNothingAfterNowsDay: the repository's row the load writes says the
// next daily pass may not start before a year after Now (retry_at), so a pass of the server's run on
// the day after Now's, as a job that runs past UTC midnight or an EVALBENCH_NOW before the server's
// day gives it, does nothing: it sends GitHub no request and leaves the row as the load wrote it,
// where a pass would find the stand-in's 404 and store the repository unreadable.
func TestTheServersRunAsksGitHubForNothingAfterNowsDay(t *testing.T) {
	spec := Spec{Now: codeNow, Seed: 2916, Scale: 0.02, BuiltBy: "evalbench-test"}
	rig, history := codeRig(t, spec)
	ctx, pool := t.Context(), rig.Pool
	if err := writeCode(ctx, pool, spec, history); err != nil {
		t.Fatalf("write the code survival results: %v", err)
	}
	repo := strings.ToLower(Repos[0])
	loaded, found, err := codesurvival.GetRepo(ctx, pool, repo)
	if err != nil || !found {
		t.Fatalf("read the repository's row: found %t, %v", found, err)
	}
	if want := codeNow.AddDate(1, 0, 0); !loaded.RetryAt.Equal(want) {
		t.Errorf("the repository's row holds retry_at %s, want a year after Now, %s", loaded.RetryAt, want)
	}
	github := codesurvivaltest.New(t)
	run := &codesurvival.Run{Pool: pool, Client: github.Client(t), Attributor: delivery.NewAttributor(refs.Renames{}), Build: "evalbench-test",
		Now: func() time.Time { return codeNow.AddDate(0, 0, 1).Add(time.Hour) }, Logger: slog.New(slog.DiscardHandler)}
	run.Pass(ctx)
	if requests := github.Requests(); len(requests) != 0 {
		t.Errorf("the run's pass on the day after Now's sent GitHub %d requests: %v", len(requests), requests)
	}
	if after, found, err := codesurvival.GetRepo(ctx, pool, repo); err != nil || !found || after != loaded {
		t.Errorf("the run's pass on the day after Now's left the repository's row %+v (found %t, %v), want it as loaded, %+v", after, found, err, loaded)
	}
}

// TestTheDeliverySettingsAreWrittenWhereTheSettingsHoldNoCodeGroups: a database whose
// project_delivery_settings holds no code_groups, a commit of this branch before migration
// 20261126133047 that k17's control loads with -code=false, takes INFRA's delivery settings as the
// rows a database of this head takes, less that column.
func TestTheDeliverySettingsAreWrittenWhereTheSettingsHoldNoCodeGroups(t *testing.T) {
	spec := Spec{Now: codeNow, Seed: 2916, Scale: 0.02, BuiltBy: "evalbench-test"}
	head, _ := codeRig(t, spec)
	base := storetest.Open(t)
	ctx := t.Context()
	if _, err := base.Pool.Exec(ctx, `alter table project_delivery_settings drop column code_groups`); err != nil {
		t.Fatalf("drop code_groups: %v", err)
	}
	if _, err := writeProject(ctx, base.Pool, spec, infraFixture); err != nil {
		t.Fatalf("write INFRA's issues: %v", err)
	}
	if err := putDeliverySettings(ctx, base.Pool); err != nil {
		t.Fatalf("write INFRA's delivery settings where they hold no code_groups: %v", err)
	}
	settings := func(pool *store.Pool) string {
		t.Helper()
		var written string
		if err := pool.QueryRow(ctx, `
			select (to_jsonb(s) - 'code_groups' - 'updated_at')::text || ' ' ||
				(select jsonb_agg(to_jsonb(d) - 'id' order by d.position)::text from project_deploy_sources d where d.project_key = s.project_key)
			from project_delivery_settings s where s.project_key = $1`, Project).Scan(&written); err != nil {
			t.Fatalf("read INFRA's delivery settings: %v", err)
		}
		return written
	}
	if got, want := settings(base.Pool), settings(head.Pool); got != want {
		t.Errorf("INFRA's delivery settings with no code_groups are\n%s\nwant this head's less code_groups\n%s", got, want)
	}
}

// TestALoadOfThisHeadRefusesTheCodeResultsWhereTheTablesAreMissing: a database that lacks a table
// of the results is a database of a base that predates them, whose load takes -code=false, and the
// load says so by the tables it lacks before it writes a row.
func TestALoadOfThisHeadRefusesTheCodeResultsWhereTheTablesAreMissing(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	if err := checkCodeSchema(ctx, database.Pool); err != nil {
		t.Fatalf("a database of this head was refused: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `drop table code_projects, code_pairs`); err != nil {
		t.Fatalf("drop tables: %v", err)
	}
	err := checkCodeSchema(ctx, database.Pool)
	if err == nil {
		t.Fatal("a database that lacks the code survival tables was accepted")
	}
	for _, want := range []string{"code_pairs", "code_projects", "-code=false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}
