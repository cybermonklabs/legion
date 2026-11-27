package delivery

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestAWindowsReadAnswersWhatItsTwoReadsAnswer: a delivery route's one read of a window
// (ReadWindowRows) answers what the two reads it stands for answer (ReadMergedRows and
// ReadDeploys, which read every apply run to the present and the window's deploys apart), over
// generated histories of deploy runs on main and off it, re-runs, rows stored before created_at,
// production jobs that succeeded, failed, were cancelled or stood down, failed guards beside them,
// and pull requests merged anywhere among them, for windows that end inside the history and one
// that ends past it: the same pull requests and the same Deploys, earlier rows included; apply
// runs that are the first of every apply run, the rest created after the window ends; each apply
// run's production jobs, with failedJobs its failed jobs too, as every job it holds says; and every
// pull request of the window and of the earlier rows shipping first with the run it ships first
// with over every apply run, and each deploy shipping the same pull requests.
//
// The two reads share code with the read they are compared to, so each window is also held to the
// seeded history read whole and judged apart from every window read (windowTruth.check): the
// deploys are the deploy runs on main started in the window, in start order, judged from every
// job they hold; the pull requests are those stored as merged in the window; and every pull request
// that a deploy of the window ships first, whenever it merged, is among the window's or the
// earlier rows and is shipped by that deploy alone. The read with deploys false (the Pipeline's
// lead times) answers the same rows and no Deploys.
//
// The comparison fails where no window's apply runs stopped short of every apply run, and where no
// pull request of any window or of its earlier rows is first shipped by a run created after the
// window's end: the bound that keeps those runs, which a cut at the window's end would get wrong,
// was then never tested. Each round's seed is in its failure.
func TestAWindowsReadAnswersWhatItsTwoReadsAnswer(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	production := settings.Production()
	judged := production.JudgedJobNames()
	bounded, windows, shippedAfterEnd := 0, 0, 0
	for seed := range uint64(24) {
		random := rand.New(rand.NewPCG(seed, 2916))
		seedWindowHistory(t, ctx, pool, random, base)
		truth := readWindowTruth(t, ctx, pool, production)
		ends := []time.Time{base.Add(15 * 24 * time.Hour)}
		for range 3 {
			ends = append(ends, base.Add(time.Duration(2*24*60+random.IntN(10*24*60))*time.Minute))
		}
		for _, to := range ends {
			from := to.Add(-time.Duration(24*60+random.IntN(3*24*60)) * time.Minute)
			label := fmt.Sprintf("seed %d, [%s, %s)", seed, from.Format(time.RFC3339), to.Format(time.RFC3339))
			wantMerged, err := ReadMergedRows(ctx, pool, settings, from, to)
			if err != nil {
				t.Fatalf("%s: ReadMergedRows: %v", label, err)
			}
			wantDeploys, err := ReadDeploys(ctx, pool, settings, from, to)
			if err != nil {
				t.Fatalf("%s: ReadDeploys: %v", label, err)
			}
			wantApplies := ProductionApplies(wantMerged.ApplyRuns, wantMerged.ApplyJobs, production).Applies
			for _, failedJobs := range []bool{false, true} {
				label := label + ", failed jobs " + strconv.FormatBool(failedJobs)
				merged, deploys, err := ReadWindowRows(ctx, pool, settings, from, to, failedJobs, true)
				if err != nil {
					t.Fatalf("%s: ReadWindowRows: %v", label, err)
				}
				windows++
				if !reflect.DeepEqual(merged.PullRequests, wantMerged.PullRequests) {
					t.Fatalf("%s: the pull requests differ from ReadMergedRows'", label)
				}
				if !reflect.DeepEqual(deploys, wantDeploys) {
					t.Fatalf("%s: the deploys differ from ReadDeploys':\n got:  %+v\n want: %+v", label, deploys, wantDeploys)
				}
				cut := len(merged.ApplyRuns)
				if cut > len(wantMerged.ApplyRuns) || !reflect.DeepEqual(merged.ApplyRuns, wantMerged.ApplyRuns[:cut]) {
					t.Fatalf("%s: the apply runs %v are not the first of every apply run %v", label, runIDsOf(merged.ApplyRuns), runIDsOf(wantMerged.ApplyRuns))
				}
				for _, run := range wantMerged.ApplyRuns[cut:] {
					if !run.Created().After(to) {
						t.Fatalf("%s: apply run %d, created %s, is left out though created by the window's end", label, run.RunID, run.Created().Format(time.RFC3339))
					}
				}
				if cut < len(wantMerged.ApplyRuns) {
					bounded++
				}
				for _, run := range merged.ApplyRuns {
					want := slices.DeleteFunc(slices.Clone(truth.jobs[run.RunID]), func(job DeliveryRunJob) bool {
						failed := job.Conclusion != nil && *job.Conclusion == DeliveryJobConclusionFailure
						return !slices.Contains(judged, job.Name) && !(failedJobs && failed)
					})
					if got, held := merged.ApplyJobs[run.RunID]; len(want) == 0 && held || len(want) > 0 && !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: apply run %d's jobs = %v, want %v", label, run.RunID, jobNames(got), jobNames(want))
					}
				}
				applies := ProductionApplies(merged.ApplyRuns, merged.ApplyJobs, production).Applies
				for _, prs := range [][]DeliveryPullRequest{merged.PullRequests, deploys.Earlier.PullRequests} {
					for _, pr := range prs {
						if got, want := ContainingRun(pr, waitingRepo, applies), ContainingRun(pr, waitingRepo, wantApplies); !reflect.DeepEqual(got, want) {
							t.Fatalf("%s: %s#%d ships first with %+v, over every apply run with %+v", label, pr.Repo, pr.Number, got, want)
						}
					}
				}
				if got, want := shippedIDs(deploys.ShippedBy(merged, settings)), shippedIDs(wantDeploys.ShippedBy(wantMerged, settings)); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: the deploys ship %v, over every apply run %v", label, got, want)
				}
				shippedAfterEnd += truth.check(t, label, settings, from, to, merged, deploys)

				bare, none, err := ReadWindowRows(ctx, pool, settings, from, to, failedJobs, false)
				if err != nil {
					t.Fatalf("%s: ReadWindowRows without deploys: %v", label, err)
				}
				if !reflect.DeepEqual(none, Deploys{}) {
					t.Fatalf("%s: the read without deploys answers the deploys %+v, want none", label, none)
				}
				if !reflect.DeepEqual(bare, merged) {
					t.Fatalf("%s: the read without deploys answers %d pull requests and the apply runs %v, want the read with them's %d and %v",
						label, len(bare.PullRequests), runIDsOf(bare.ApplyRuns), len(merged.PullRequests), runIDsOf(merged.ApplyRuns))
				}
			}
		}
	}
	if bounded == 0 {
		t.Fatalf("none of %d windows read fewer apply runs than every apply run, so the bound was never tested", windows)
	}
	if shippedAfterEnd == 0 {
		t.Fatalf("no pull request of any of %d windows, nor of their earlier rows, is first shipped by a run created after the window's end, so the bound that keeps those runs was never tested", windows)
	}
	t.Logf("%d windows, %d of them reading fewer apply runs than every apply run, %d pull requests first shipped by a run created after their window's end", windows, bounded, shippedAfterEnd)
}

// windowTruth is the seeded history read whole, apart from every window read: each deploy run on
// main with every job it holds, the deploys among them judged from those jobs, and every stored
// pull request.
type windowTruth struct {
	runs    []DeliveryRun
	jobs    map[int64][]DeliveryRunJob
	applies []Apply
	prs     []DeliveryPullRequest
}

// readWindowTruth reads the stored history whole: every deploy run on main, every one of their
// jobs, and every pull request merged from the epoch to the year 2100.
func readWindowTruth(t *testing.T, ctx context.Context, pool *store.Pool, production Production) windowTruth {
	t.Helper()
	epoch, end := time.Unix(0, 0), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	runs, err := listRuns(ctx, pool, waitingRepo, DeliveryRunKindDeploy, "main", epoch, nil)
	if err != nil {
		t.Fatalf("read every deploy run on main: %v", err)
	}
	jobs, err := listEveryRunJobForRuns(ctx, pool, waitingRepo, runIDsOf(runs))
	if err != nil {
		t.Fatalf("read every job of every deploy run: %v", err)
	}
	prs, err := ListPullRequestsInWindow(ctx, pool, epoch, end)
	if err != nil {
		t.Fatalf("read every pull request: %v", err)
	}
	return windowTruth{runs: runs, jobs: jobs, applies: ProductionApplies(runs, jobs, production).Applies, prs: prs}
}

// check holds one window's ReadWindowRows answer with deploys (merged and deploys) to the history
// read whole, and answers how many pull requests of the window or of its earlier rows are first
// shipped by a run created after to, each of which it also holds to be among the apply runs read.
func (h windowTruth) check(t *testing.T, label string, settings DeliverySettings, from, to time.Time, merged MergedRows, deploys Deploys) int {
	t.Helper()
	id := func(pr DeliveryPullRequest) string { return pr.Repo + "#" + strconv.Itoa(pr.Number) }
	var started []DeliveryRun
	for _, run := range h.runs {
		if !run.StartedAt.Before(from) && run.StartedAt.Before(to) {
			started = append(started, run)
		}
	}
	slices.SortFunc(started, func(a, b DeliveryRun) int {
		return cmp.Or(a.StartedAt.Compare(b.StartedAt), cmp.Compare(a.RunID, b.RunID))
	})
	if got, want := runIDsOf(deploys.Started.Runs), runIDsOf(started); !slices.Equal(got, want) {
		t.Fatalf("%s: the window's runs = %v, want the deploy runs on main started in it, %v", label, got, want)
	}
	wantApplies := ProductionApplies(started, h.jobs, settings.Production()).Applies
	if !slices.Equal(deploys.Applies, wantApplies) {
		t.Fatalf("%s: the window's deploys = %+v, want those judged from every job, %+v", label, deploys.Applies, wantApplies)
	}
	var wantWindow, gotWindow []string
	for _, pr := range h.prs {
		if !pr.MergedAt.Before(from) && pr.MergedAt.Before(to) {
			wantWindow = append(wantWindow, id(pr))
		}
	}
	read := map[string]bool{}
	for _, pr := range merged.PullRequests {
		gotWindow = append(gotWindow, id(pr))
		read[id(pr)] = true
	}
	if !slices.Equal(gotWindow, wantWindow) {
		t.Fatalf("%s: the window's pull requests = %v, want those stored as merged in it, %v", label, gotWindow, wantWindow)
	}
	for _, pr := range deploys.Earlier.PullRequests {
		read[id(pr)] = true
	}
	deployIDs := map[int64]bool{}
	for _, apply := range wantApplies {
		deployIDs[apply.RunID] = true
	}
	applyRunIDs := map[int64]bool{}
	for _, run := range merged.ApplyRuns {
		applyRunIDs[run.RunID] = true
	}
	wantShipped, afterEnd := map[int64][]string{}, 0
	for _, pr := range h.prs {
		apply := ContainingRun(pr, settings.DeployRepo, h.applies)
		if apply == nil {
			continue
		}
		if deployIDs[apply.RunID] {
			if !read[id(pr)] {
				t.Fatalf("%s: %s is first shipped by deploy run %d of the window and is in neither the window's pull requests nor the earlier rows", label, id(pr), apply.RunID)
			}
			wantShipped[apply.RunID] = append(wantShipped[apply.RunID], id(pr))
		}
		if read[id(pr)] && apply.CreatedAt.After(to) {
			afterEnd++
			if !applyRunIDs[apply.RunID] {
				t.Fatalf("%s: run %d, created %s after the window's end, first ships %s and is not among the apply runs read", label, apply.RunID, apply.CreatedAt.Format(time.RFC3339), id(pr))
			}
		}
	}
	gotShipped := shippedIDs(deploys.ShippedBy(merged, settings))
	for _, ids := range gotShipped {
		slices.Sort(ids)
	}
	for _, ids := range wantShipped {
		slices.Sort(ids)
	}
	if !reflect.DeepEqual(gotShipped, wantShipped) {
		t.Fatalf("%s: the deploys ship %v, want what each ships first among every stored pull request, %v", label, gotShipped, wantShipped)
	}
	return afterEnd
}

// TestTheWindowsRunsReadIsBoundedThroughItsIndexes is an eligibility test, and proves no more than
// that. Each read's statement (and the read of several days' runs, daysRunsQuery) is planned under
// the generic plan pgx's cached statement settles on, with sequential and bitmap scans switched
// off (set local enable_seqscan = off, enable_bitmapscan = off), over a year of deploy runs, each
// with its production job, analyzed. With those off the plan can only read delivery_runs through an
// index, so the test shows what it can: the statement gives delivery_runs_kind_created a condition
// from the window's start to the bound of the first deploy created at or after its end, and
// delivery_runs_kind_started_at one over the window (each day's the same way, from the day's start),
// and no node of the plan reads delivery_runs without such a condition. It does not show that the
// planner takes those indexes when it may take a scan: it takes a scan of a table this small when
// that costs less, with no fault in the statement. That the answers are what a read of every run
// answers is TestAWindowsReadAnswersWhatItsTwoReadsAnswer's; that the planner reads a table the
// size of production's by an index, with nothing switched off, is
// TestTheLatestShipBeforeReadStartsAtItsMomentWhateverThePlan's for the one read it plans and
// TestTheRunJobsReadsUseTheirIndexesOnEveryPlanAtScale's for the run-jobs reads.
func TestTheWindowsRunsReadIsBoundedThroughItsIndexes(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAYearOfDeployRuns(t, ctx, pool)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	window := struct{ parameters, literals []string }{
		[]string{"text", "timestamptz", "timestamptz", "text[]", "text", "integer"},
		[]string{"'acme/deploy'", "now() - interval '84 days'", "now()", "array['production']", "null", "50000"},
	}
	days := struct{ parameters, literals []string }{
		[]string{"text", "timestamptz[]", "timestamptz[]", "text[]", "text", "integer"},
		[]string{"'acme/deploy'", "array[now() - interval '84 days', now() - interval '40 days']",
			"array[now() - interval '83 days', now() - interval '39 days']", "array['production']", "null", "50000"},
	}
	for _, read := range []struct {
		name, statement      string
		parameters, literals []string
		// start is how an index condition names the start of the window it reads.
		start   string
		indexes map[string][]string
	}{
		{"window_apply_runs", windowApplyRunsQuery, window.parameters, window.literals, "$2",
			map[string][]string{"delivery_runs_kind_created": {">=", "<="}}},
		{"window_runs", windowRunsQuery, window.parameters, window.literals, "$2",
			map[string][]string{"delivery_runs_kind_created": {">=", "<="}, "delivery_runs_kind_started_at": {">=", "<"}}},
		{"days_runs", daysRunsQuery, days.parameters, days.literals, "from_at",
			map[string][]string{"delivery_runs_kind_created": {">=", "<="}, "delivery_runs_kind_started_at": {">=", "<"}}},
	} {
		plan := explaintest.GenericPlan(t, ctx, connection, read.name, read.statement, read.parameters, read.literals,
			"set local enable_seqscan = off", "set local enable_bitmapscan = off")
		if explaintest.SeqScansRelation(t, plan, "delivery_runs") {
			t.Fatalf("%s scans delivery_runs whole:\n%s", read.name, plan)
		}
		scans := explaintest.IndexScans(t, plan)
		for _, scan := range scans {
			if scan.Relation == "delivery_runs" && scan.Cond == "" {
				t.Fatalf("%s reads delivery_runs through %s with no index condition:\n%s", read.name, scan, plan)
			}
		}
		for _, bitmap := range explaintest.BitmapHeapScans(t, plan, "delivery_runs") {
			for _, index := range bitmap.BitmapIndexScans {
				if index.IndexCond == "" {
					t.Fatalf("%s reads delivery_runs through a bitmap of %s with no index condition:\n%s", read.name, index, plan)
				}
			}
		}
		for index, bounds := range read.indexes {
			found := slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool {
				if scan.Relation != "delivery_runs" || scan.Index != index || !strings.Contains(scan.Cond, read.start) {
					return false
				}
				for _, bound := range bounds {
					if !strings.Contains(scan.Cond, bound) {
						return false
					}
				}
				return true
			})
			if !found {
				t.Fatalf("%s reads %v, want delivery_runs through %s from the window's start bounded by %q:\n%s", read.name, scans, index, bounds, plan)
			}
		}
		t.Logf("%s reads %v", read.name, scans)
	}
}

// TestTheDaysRunsReadBuildsEachDaysListOnce: the read of several days' runs (daysRunsQuery) reads
// the days once, so it builds each day's list of runs, and the bound of the day's apply runs, once
// a day, under either plan-cache mode, with every join a nested loop (ExplainAfter), the join the
// planner took over a fresh database's tables no ANALYZE had read. The deploy runs here all failed
// their production job, so no day's bound finds a deploy that shipped after the day, and each walks
// every later run and probes its jobs, as the bounds did while a fixture's jobs were not yet
// committed in the eval day-row refresher's snapshot (dispatch://INFRA-2916). The read answers the
// runs its control answers: the statement as it stood, the days' lists inside the IN of the read of
// the runs, which must break the rule under the custom plan, since it built every day's lists again
// for each deploy run.
func TestTheDaysRunsReadBuildsEachDaysListOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const runs = 60
	if _, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, url, head_branch, event)
		select 'acme/deploy', n, 'deploy', lpad(n::text, 40, '0'), at - interval '5 minutes', at, at,
			'https://github.com/acme/deploy/actions/runs/' || n, 'main', 'push'
		from generate_series(1, $1::int) n, lateral (select timestamptz '2026-11-01 00:00:00+00' + n * interval '6 hours' as at) moment
	`, runs); err != nil {
		t.Fatalf("seed the deploy runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select repo, run_id, 'production', started_at, started_at + interval '20 minutes', 'failure'
		from delivery_runs where repo = 'acme/deploy'
	`); err != nil {
		t.Fatalf("seed the runs' failed production jobs: %v", err)
	}
	var days []time.Time
	for day := range 15 {
		days = append(days, time.Date(2026, 11, 1+day, 0, 0, 0, 0, time.UTC))
	}
	from, to, err := dayWindows(days)
	if err != nil {
		t.Fatal(err)
	}
	args := []any{"acme/deploy", from, to, []string{"production"}, (*string)(nil), maxRunsPerWindow * len(days)}
	control := `
		select ` + RunColumns + `
		from delivery_runs r
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.run_id in (` + daysRunsListed + `
		)
		order by ` + runCreatedAt + `, r.run_id
		limit $6`
	// readsAgain is each read of statement's plan under mode that ran more often than once a day:
	// the scan of the days more than once, or the jobs probed more than once a day for each run.
	readsAgain := func(statement, mode string) (again []string, outline string) {
		plan := explaintest.ExplainAfter(t, ctx, pool, nil, explaintest.Statement{SQL: statement, Args: args}, mode, "json")
		for _, node := range explaintest.Nodes(t, plan) {
			switch {
			case node.Type == "Function Scan" && node.Loops > 1:
				again = append(again, fmt.Sprintf("%s ran %.0f times", node, node.Loops))
			case node.Relation == "delivery_run_jobs" && node.Loops > float64(runs*len(days)):
				again = append(again, fmt.Sprintf("%s ran %.0f times, more than %d runs a day for %d days", node, node.Loops, runs, len(days)))
			}
		}
		return again, explaintest.Outline(t, plan)
	}
	for _, mode := range explaintest.PlanCacheModes {
		if again, outline := readsAgain(daysRunsQuery, mode); len(again) > 0 {
			t.Errorf("under %s the read of the days' runs builds the days' lists again for each run: %v\n%s", mode, again, outline)
		}
	}
	again, outline := readsAgain(control, "force_custom_plan")
	if len(again) == 0 {
		t.Fatalf("the control, the days' lists inside the IN, reads nothing again under the custom plan, so the check cannot fail:\n%s", outline)
	}
	t.Logf("the control reads again: %v", again)
	answered := func(statement string) []int64 {
		rows, err := pool.Query(ctx, statement, args...)
		if err != nil {
			t.Fatalf("read the days' runs: %v", err)
		}
		read, err := collectRuns(rows)
		if err != nil {
			t.Fatalf("read the days' runs: %v", err)
		}
		return runIDsOf(read)
	}
	if got, want := answered(daysRunsQuery), answered(control); len(got) != runs || !slices.Equal(got, want) {
		t.Errorf("the read answers runs %v; its control answers %v, of %d runs", got, want, runs)
	}
}

// seedAYearOfDeployRuns writes 4,000 deploy runs on main of acme/deploy, one every two hours over
// the year that ends about a month before now, each with a production job and a build job, and
// analyzes the tables: over an empty table every index costs one row, and which of two indexes
// leading with (repo, kind) the planner takes is a tie the plan would settle by chance.
func seedAYearOfDeployRuns(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, url, head_branch, event)
		select 'acme/deploy', n, 'deploy', lpad(n::text, 40, '0'), at - interval '5 minutes', at, at,
			'https://github.com/acme/deploy/actions/runs/' || n, 'main', 'push'
		from generate_series(1, 4000) n, lateral (select now() - interval '365 days' + n * interval '2 hours' as at) moment
	`); err != nil {
		t.Fatalf("seed the deploy runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select repo, run_id, job.name, started_at, started_at + interval '20 minutes', 'success'
		from delivery_runs, (values ('production'), ('build')) job(name)
		where repo = 'acme/deploy'
	`); err != nil {
		t.Fatalf("seed the runs' jobs: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze delivery_runs, delivery_run_jobs`); err != nil {
		t.Fatalf("analyze the seeded runs: %v", err)
	}
}

// TestTheEarlierPullRequestsAreTheDeployRepositorysAloneBeforeTheRowCap: the pull requests merged
// before a window that its deploys can ship (Deploys.Earlier) are the deploy repository's, chosen in
// SQL before the row cap (listRepoPullRequestsInWindow), since a deploy ships no pull request of
// another repository (ContainingRun). Before the first successful deploy of the deploy repository
// that read runs from the beginning of time, so every pull request of another repository merged
// since stood ahead of the deploy repository's newest in the cap's oldest-first order. Here a
// deploy of the window ships the one pull request of the deploy repository merged an hour before
// it, behind one more pull request of another repository than the cap holds, all merged earlier.
// The control is the same read with the repository left out of the predicate (ListPullRequestsInWindow
// over the same span): the cap takes the other repository's pull requests alone and leaves out the
// one the deploy ships. The window's own pull requests, which the routes' population is, keep
// every repository's.
func TestTheEarlierPullRequestsAreTheDeployRepositorysAloneBeforeTheRowCap(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	from := time.Date(2024, 5, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	runs := []waitingRun{{
		id: 1, branch: "main", head: from.Add(30 * time.Minute), created: new(from.Add(time.Hour)),
		started: from.Add(time.Hour), done: from.Add(2 * time.Hour), result: DeliveryJobConclusionSuccess,
	}}
	shipped := waitingPR{waitingRepo, 1, from.Add(-time.Hour)}
	elsewhere := waitingPR{"acme/other", 2, from.Add(time.Hour)}
	seedWaitingHistory(t, ctx, pool, runs, []waitingPR{shipped, elsewhere})
	seedPullRequestsASecondApart(t, ctx, pool, "acme/other", 1000, maxPullRequestsPerWindow+1, from.Add(-40*time.Hour))

	control, err := ListPullRequestsInWindow(ctx, pool, time.Time{}, from)
	if err != nil {
		t.Fatalf("ListPullRequestsInWindow: %v", err)
	}
	if len(control) != maxPullRequestsPerWindow {
		t.Fatalf("the control read answers %d pull requests, want the cap's %d: the fixture does not reach the cap", len(control), maxPullRequestsPerWindow)
	}
	for _, pr := range control {
		if pr.Repo == waitingRepo {
			t.Fatalf("the control read holds %s#%d, which the cap should have left out behind %d older pull requests of another repository", pr.Repo, pr.Number, maxPullRequestsPerWindow)
		}
	}

	merged, deploys, err := ReadWindowRows(ctx, pool, settings, from, to, false, true)
	if err != nil {
		t.Fatalf("ReadWindowRows: %v", err)
	}
	if len(deploys.Applies) != 1 || deploys.Applies[0].RunID != 1 {
		t.Fatalf("the window's deploys = %+v, want run 1 alone", deploys.Applies)
	}
	if earlier := deploys.Earlier.PullRequests; len(earlier) != 1 || earlier[0].Repo != waitingRepo || earlier[0].Number != 1 {
		t.Fatalf("the earlier pull requests number %d, want %s#1 alone: another repository's pull requests must not stand in them or fill their cap", len(earlier), waitingRepo)
	}
	if got, want := shippedIDs(deploys.ShippedBy(merged, settings)), map[int64][]string{1: {waitingRepo + "#1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the deploys ship %v, want %v", got, want)
	}
	if got := merged.PullRequests; len(got) != 1 || got[0].Repo != "acme/other" || got[0].Number != 2 {
		t.Fatalf("the window's pull requests number %d, want acme/other#2 alone: the window keeps every repository's", len(got))
	}
}

// TestEachDaysEarlierPullRequestsAreBoundedByTheirOwnWindow: the read of several days' deploys
// (ReadDaysDeploys) reads the deploy repository's pull requests merged before each day that the
// day's deploys can ship (Deploys.Earlier), every day's in one statement, and bounds each day's at
// maxPullRequestsPerWindow, the bound of one window's read, whatever the other days hold. Each of
// two days holds a deploy. The first day's is the repository's first to succeed, so its earlier
// window runs from the beginning of time; the second day's runs from the first deploy's creation
// and holds one pull request, merged after that deploy. A pull request of another repository
// merged before the first day is in neither. At the bound, the first day's earlier rows are every
// one of its pull requests, oldest merge first, and its deploy ships them all, the newest
// included; the second day's are its one, which its deploy ships. One pull request more before the
// first day is past the bound, and the read is refused, naming that window and the bound, with no
// day's rows answered, although the second day's window leaves all but one of its share unused: a
// limit the windows shared, twice the bound here, answered the first day one pull request more
// than a window's read holds, and with more still kept the oldest of both days' together, leaving
// out the newest, which the deploys ship (dispatch://INFRA-2916).
func TestEachDaysEarlierPullRequestsAreBoundedByTheirOwnWindow(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	days := []time.Time{time.Date(2024, 5, 10, 0, 0, 0, 0, time.UTC), time.Date(2024, 5, 11, 0, 0, 0, 0, time.UTC)}
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	var runs []waitingRun
	for i, day := range days {
		runs = append(runs, waitingRun{
			id: int64(i + 1), branch: "main", head: day.Add(30 * time.Minute), created: new(day.Add(time.Hour)),
			started: day.Add(time.Hour), done: day.Add(2 * time.Hour), result: DeliveryJobConclusionSuccess,
		})
	}
	seedWaitingHistory(t, ctx, pool, runs, []waitingPR{{waitingRepo, 1, days[0].Add(3 * time.Hour)}, {"acme/other", 1, days[0].Add(-time.Hour)}})
	seedPullRequestsASecondApart(t, ctx, pool, waitingRepo, 1000, maxPullRequestsPerWindow, days[0].Add(-40*time.Hour))
	firstWindow := fmt.Sprintf("[%s, %s)", time.Time{}.Format(time.RFC3339Nano), days[0].Format(time.RFC3339Nano))

	read, err := ReadDaysDeploys(ctx, pool, settings, days, true)
	if err != nil {
		t.Fatalf("ReadDaysDeploys with %d pull requests, the bound, in the first day's earlier window %s: %v", maxPullRequestsPerWindow, firstWindow, err)
	}
	first, second := read.Deploys[0].Earlier.PullRequests, read.Deploys[1].Earlier.PullRequests
	if len(first) != maxPullRequestsPerWindow || first[0].Number != 1001 || first[len(first)-1].Number != 1000+maxPullRequestsPerWindow {
		t.Fatalf("the first day's earlier pull requests number %d, want %s#1001 to #%d, every one of its window's, oldest merge first", len(first), waitingRepo, 1000+maxPullRequestsPerWindow)
	}
	if len(second) != 1 || second[0].Repo != waitingRepo || second[0].Number != 1 {
		t.Fatalf("the second day's earlier pull requests number %d, want %s#1 alone", len(second), waitingRepo)
	}
	shipped := func(day int) map[int64][]string {
		window, err := read.Applies.Since(days[day])
		if err != nil {
			t.Fatalf("the apply runs of %s: %v", days[day].Format(time.DateOnly), err)
		}
		return shippedIDs(read.Deploys[day].ShippedBy(MergedRows{ApplyRuns: window.Runs, ApplyJobs: window.Jobs}, settings))
	}
	newest := fmt.Sprintf("%s#%d", waitingRepo, 1000+maxPullRequestsPerWindow)
	if got := shipped(0); len(got) != 1 || len(got[1]) != maxPullRequestsPerWindow || got[1][len(got[1])-1] != newest {
		t.Fatalf("the first day's deploy ships %d of its earlier pull requests, want run 1 to ship all %d, %s the last", len(got[1]), maxPullRequestsPerWindow, newest)
	}
	if got, want := shipped(1), map[int64][]string{2: {waitingRepo + "#1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the second day's deploys ship %v, want %v", got, want)
	}

	seedPullRequestsASecondApart(t, ctx, pool, waitingRepo, 1000+maxPullRequestsPerWindow, 1, days[0].Add(-2*time.Second))
	read, err = ReadDaysDeploys(ctx, pool, settings, days, true)
	if err == nil {
		t.Fatalf("ReadDaysDeploys answered %d earlier pull requests of the first day and %d of the second with no error: the first day's window %s holds %d, past the bound of %d, and the share the second day's window leaves unused is not the first's",
			len(read.Deploys[0].Earlier.PullRequests), len(read.Deploys[1].Earlier.PullRequests), firstWindow, maxPullRequestsPerWindow+1, maxPullRequestsPerWindow)
	}
	if !strings.Contains(err.Error(), firstWindow) || !strings.Contains(err.Error(), strconv.Itoa(maxPullRequestsPerWindow)) {
		t.Fatalf("ReadDaysDeploys: %v, want a refusal naming the first day's earlier window %s and the bound %d", err, firstWindow, maxPullRequestsPerWindow)
	}
	if !reflect.DeepEqual(read, DaysDeploys{}) {
		t.Fatalf("ReadDaysDeploys answered %d days' deploys beside its refusal, want none", len(read.Deploys))
	}
	t.Logf("past the bound: %v", err)
}

// seedPullRequestsASecondApart writes count merged pull requests of repo in one statement, numbered
// from first+1 and merged a second apart from a second after after, each opened an hour before
// its merge. It first stops the trigger that logs each change, which takes a lock and writes a row
// for each of a row cap's fifty thousand rows: a test's database, its own, needs none logged.
func seedPullRequestsASecondApart(t *testing.T, ctx context.Context, pool *store.Pool, repo string, first, count int, after time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `alter table delivery_pull_requests disable trigger delivery_pull_requests_changed`); err != nil {
		t.Fatalf("stop logging the pull requests' changes: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at)
		select $1::text, $2::int + n, 'feat: a widget', 'https://github.com/' || $1::text || '/pull/' || ($2::int + n), 'octocat',
			merged - interval '1 hour', merged
		from generate_series(1, $3::int) n, lateral (select $4::timestamptz + n * interval '1 second' as merged) moment
	`, repo, first, count, after); err != nil {
		t.Fatalf("seed %d pull requests of %s: %v", count, repo, err)
	}
}

// seedWindowHistory replaces the stored runs, their jobs and the pull requests with a generated
// history of two weeks from base (TestAWindowsReadAnswersWhatItsTwoReadsAnswer says what it holds).
// Each job completes after its run was created, as GitHub's do (latestShipBefore), and each
// production job at its own instant, since the SQL and the Go rule may break a tie apart only by
// a run id that no answer shows.
func seedWindowHistory(t *testing.T, ctx context.Context, pool *store.Pool, random *rand.Rand, base time.Time) {
	t.Helper()
	minutes := func(lo, hi int) time.Duration { return time.Duration(lo+random.IntN(hi-lo+1)) * time.Minute }
	success, failure := DeliveryJobConclusionSuccess, DeliveryJobConclusionFailure
	var runs []DeliveryRun
	var runJobs [][]DeliveryRunJob
	for i := range 30 + random.IntN(30) {
		id := int64(900 + i)
		created := base.Add(minutes(0, 14*24*60))
		started := created.Add(time.Duration(random.IntN(30)) * time.Second)
		run := DeliveryRun{
			Repo: waitingRepo, RunID: id, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", id),
			HeadCommitAt: created.Add(-minutes(0, 60)), StartedAt: started, CreatedAt: &created,
			URL: fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id), HeadBranch: new("main"), Event: new("push"),
		}
		switch random.IntN(10) {
		case 0:
			run.HeadBranch = new("feature/x")
		case 1:
			run.StartedAt = created.Add(minutes(14, 54)) // a re-run
		case 2:
			run.CreatedAt = nil // stored before created_at, so read by its start
		}
		done := run.StartedAt.Add(minutes(5, 6*60) + time.Duration(i)*time.Millisecond)
		buildDone := run.StartedAt.Add(3 * time.Minute)
		jobs := []DeliveryRunJob{{Name: "build", StartedAt: &run.StartedAt, CompletedAt: &buildDone, Conclusion: &success}}
		production := DeliveryRunJob{Name: waitingJob, StartedAt: &run.StartedAt, CompletedAt: &done, ApplyStep: new(waitingStep)}
		switch draw := random.IntN(20); {
		case draw < 14:
			production.Conclusion, production.ApplyStepConclusion = &success, &success
		case draw < 17:
			production.Conclusion, production.ApplyStepConclusion = &failure, &failure
		case draw < 18:
			production.Conclusion, production.ApplyStepConclusion = new(DeliveryJobConclusionCancelled), new(DeliveryJobConclusionCancelled)
		default:
			production.Conclusion, production.ApplyStepConclusion = &success, new(DeliveryJobConclusionSkipped) // stood down
		}
		jobs = append(jobs, production)
		failed := *production.Conclusion == failure
		if random.IntN(10) < 3 {
			guardDone := done.Add(time.Minute)
			jobs = append(jobs, DeliveryRunJob{Name: []string{"test / unit", "deploy-verdict"}[random.IntN(2)], StartedAt: &run.StartedAt, CompletedAt: &guardDone, Conclusion: &failure})
			failed = true
		}
		conclusion := DeliveryRunConclusionSuccess
		if failed {
			conclusion = DeliveryRunConclusionFailure
		}
		run.Conclusion, run.CompletedAt = &conclusion, new(done.Add(2*time.Minute))
		for j := range jobs {
			jobs[j].Repo, jobs[j].RunID = waitingRepo, id
		}
		runs, runJobs = append(runs, run), append(runJobs, jobs)
	}
	var prs []waitingPR
	for i := range 30 + random.IntN(20) {
		repo := waitingRepo
		if random.IntN(8) == 0 {
			repo = "acme/other"
		}
		prs = append(prs, waitingPR{repo: repo, number: 600 + i, merged: base.Add(minutes(0, 14*24*60)).Truncate(time.Second)})
	}
	// seedWaitingHistory clears the tables and writes the pull requests. The runs are written
	// here, since a waitingRun gives each of its jobs the run's one conclusion and so cannot hold a
	// failed guard beside a successful production job.
	seedWaitingHistory(t, ctx, pool, nil, prs)
	for i, run := range runs {
		if err := UpsertRun(ctx, pool, run); err != nil {
			t.Fatalf("seed run %d: %v", run.RunID, err)
		}
		if err := UpsertRunJobs(ctx, pool, waitingRepo, run.RunID, runJobs[i], true); err != nil {
			t.Fatalf("seed run %d's jobs: %v", run.RunID, err)
		}
	}
}

// shippedIDs is ShippedBy's answer by pull request id, each deploy's in order.
func shippedIDs(shipped map[int64][]*DeliveryPullRequest) map[int64][]string {
	ids := make(map[int64][]string, len(shipped))
	for runID, prs := range shipped {
		for _, pr := range prs {
			ids[runID] = append(ids[runID], pr.Repo+"#"+strconv.Itoa(pr.Number))
		}
	}
	return ids
}
