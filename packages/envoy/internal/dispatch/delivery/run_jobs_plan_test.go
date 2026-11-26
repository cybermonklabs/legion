package delivery

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The run jobs read names the jobs it reads in SQL, and the name is a key of the read and not a
// filter after it: `EXPLAIN (ANALYZE, BUFFERS)` of ListRunJobsForRuns' statement reads
// delivery_run_jobs through delivery_run_jobs_named (repo, name, run_id) alone, an index-only scan
// under an index condition that holds the repository, the names and the runs, which fetches no heap
// page once the table is vacuumed (requireTheNamedJobsReadIsIndexOnly), and answers only the named
// jobs of each run. Unfiltered, this read cost 660 ms a call at ten times the stated run rate
// (dispatch://INFRA-2916, R13), most of it the pull-request runs' dozens of jobs no caller of it
// reads.
func TestTheRunJobsReadKeysTheJobsItNames(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	success := DeliveryJobConclusionSuccess
	runs := []int64{820, 821, 822}
	held := []string{"build", "lint", "production / production-apply", "staging / staging-apply", "test / unit", "test / e2e"}
	for _, id := range runs {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, started, started)); err != nil {
			t.Fatalf("UpsertRun %d: %v", id, err)
		}
		jobs := make([]DeliveryRunJob, len(held))
		for i, name := range held {
			jobs[i] = DeliveryRunJob{Repo: "acme/widgets", RunID: id, Name: name, StartedAt: &started, CompletedAt: &started, Conclusion: &success}
		}
		if err := UpsertRunJobs(ctx, pool, "acme/widgets", id, jobs, true); err != nil {
			t.Fatalf("UpsertRunJobs %d: %v", id, err)
		}
	}
	named := []string{"production / production-apply", "staging / staging-apply"}

	requireTheNamedJobsReadIsIndexOnly(t, ctx, pool, runs, named)

	byRun, err := ListRunJobsForRuns(ctx, pool, "acme/widgets", runs, named)
	if err != nil {
		t.Fatalf("ListRunJobsForRuns: %v", err)
	}
	for _, id := range runs {
		var got []string
		for _, job := range byRun[id] {
			got = append(got, job.Name)
		}
		if !slices.Equal(got, named) {
			t.Errorf("run %d's jobs read by name = %v, want %v", id, got, named)
		}
	}
	if none, err := ListRunJobsForRuns(ctx, pool, "acme/widgets", runs, nil); err != nil || len(none) != 0 {
		t.Errorf("ListRunJobsForRuns with no name = %v, %v; want no job and no error", none, err)
	}
	every, err := listEveryRunJobForRuns(ctx, pool, "acme/widgets", runs)
	if err != nil {
		t.Fatalf("listEveryRunJobForRuns: %v", err)
	}
	if got := len(every[runs[0]]); got != len(held) {
		t.Errorf("listEveryRunJobForRuns' run %d holds %d jobs, want all %d", runs[0], got, len(held))
	}
}

// The timeline's reads of its deploy runs' jobs read the named jobs of each run through
// delivery_run_jobs_named (runJobsNamedForRunsQuery, requireTheNamedJobsReadIsIndexOnly) and each
// run's failed jobs through delivery_run_jobs_failed (runJobsFailedForRunsQuery), the partial index
// that holds the failed jobs alone, so they read in proportion to the runs' failures rather than
// every job they ran: `EXPLAIN (ANALYZE, BUFFERS)` of each statement reads delivery_run_jobs
// through its one index and through nothing else. Postgres plans no condition a partial index's
// predicate implies, so the failed read's index condition is the repository and the run, and its
// failure predicate is the index's own, read from pg_index. Each run's jobs the two answer
// (namedAndFailedJobs) are its named jobs and its failed ones, in ListRunJobs' order, and
// FailedJobs, RootFailingJob, Production.Job and the deploys answer from them what they answer from
// every job the run holds.
func TestTheTimelinesRunJobsReadKeysTheNamedJobsAndTheFailedOnes(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time { v := started.Add(time.Duration(minutes) * time.Minute); return &v }
	success, failure, skipped := DeliveryJobConclusionSuccess, DeliveryJobConclusionFailure, DeliveryJobConclusionSkipped
	cancelled, timedOut := DeliveryJobConclusionCancelled, DeliveryJobConclusionTimedOut
	production := Production{JobNames: []string{"production / apply"}}
	held := map[int64][]DeliveryRunJob{
		// Two failed jobs, the guard finishing first, and the production job's caller skipped.
		840: {
			{Name: "build", StartedAt: at(0), CompletedAt: at(4), Conclusion: &success},
			{Name: "test / unit", StartedAt: at(4), CompletedAt: at(9), Conclusion: &failure},
			{Name: "lint", StartedAt: at(4), CompletedAt: at(6), Conclusion: &success},
			{Name: "deploy-verdict", StartedAt: at(4), CompletedAt: at(5), Conclusion: &failure},
			{Name: "production", Conclusion: &skipped},
			{Name: "notify-release", Conclusion: &skipped},
		},
		// The production job failed: a job both named and failed, answered once.
		841: {
			{Name: "build", StartedAt: at(0), CompletedAt: at(4), Conclusion: &success},
			{Name: "production / apply", StartedAt: at(10), CompletedAt: at(14), Conclusion: &failure},
			{Name: "notify-release", Conclusion: &skipped},
		},
		// A deploy whose only failed jobs are guards, so the root failing job is the first of them.
		842: {
			{Name: "build", StartedAt: at(0), CompletedAt: at(4), Conclusion: &success},
			{Name: "production / apply", StartedAt: at(10), CompletedAt: at(14), Conclusion: &success},
			{Name: "release-verdict", StartedAt: at(15), CompletedAt: at(18), Conclusion: &failure},
			{Name: "page-oncall", StartedAt: at(15), CompletedAt: at(20), Conclusion: &failure},
		},
		// No job failed: a job timed out and one cancelled are no failures.
		843: {
			{Name: "build", StartedAt: at(0), CompletedAt: at(4), Conclusion: &success},
			{Name: "test / e2e", StartedAt: at(4), CompletedAt: at(49), Conclusion: &timedOut},
			{Name: "production / apply", StartedAt: at(50), CompletedAt: at(52), Conclusion: &cancelled},
		},
	}
	runs := []int64{840, 841, 842, 843}
	listed := make([]DeliveryRun, len(runs))
	for i, id := range runs {
		listed[i] = changeTestRun(id, DeliveryRunKindDeploy, started, started)
		if err := UpsertRun(ctx, pool, listed[i]); err != nil {
			t.Fatalf("UpsertRun %d: %v", id, err)
		}
		for j := range held[id] {
			held[id][j].Repo, held[id][j].RunID = "acme/widgets", id
		}
		if err := UpsertRunJobs(ctx, pool, "acme/widgets", id, held[id], true); err != nil {
			t.Fatalf("UpsertRunJobs %d: %v", id, err)
		}
	}
	names := production.JudgedJobNames()

	// The failed read is planned before the vacuum the named read's plan needs
	// (requireTheNamedJobsReadIsIndexOnly): once the one page of these sixteen jobs is all-visible,
	// Postgres 18.6 reads their failures by an index-only scan of delivery_run_jobs_named, which
	// holds every column the read answers, rather than through the partial index and the heap page.
	// The partial index's case rests on its holding a few in a hundred of a repository's jobs, which
	// a fixture of sixteen cannot show.
	failedPlan, failedRaw := explaintest.AnalyzedPlan(t, ctx, pool, runJobsFailedForRunsQuery, "acme/widgets", runs, names)
	if scanned := scannedRelations(t, failedPlan); !slices.Equal(scanned, []string{"delivery_run_jobs"}) {
		t.Fatalf("the timeline's failed jobs read reads %v; want delivery_run_jobs alone:\n%s", scanned, failedRaw)
	}
	if explaintest.SeqScansRelation(t, failedPlan, "delivery_run_jobs") {
		t.Fatalf("the timeline's failed jobs read scans delivery_run_jobs whole:\n%s", failedRaw)
	}
	scan := indexScanOf(t, failedPlan, failedRaw, "delivery_run_jobs_failed")
	for _, key := range []string{"repo =", "run_id = ANY"} {
		if !strings.Contains(scan.Cond, key) {
			t.Fatalf("the failed read's index condition %q holds no %q; want the repository and the run to key delivery_run_jobs_failed:\n%s", scan.Cond, key, failedRaw)
		}
	}
	requireTheNamedJobsReadIsIndexOnly(t, ctx, pool, runs, names)
	var predicate string
	if err := pool.QueryRow(ctx, `select coalesce(pg_get_expr(indpred, indrelid), '') from pg_index where indexrelid = 'delivery_run_jobs_failed'::regclass`).Scan(&predicate); err != nil {
		t.Fatalf("read delivery_run_jobs_failed's predicate: %v", err)
	}
	if predicate != "(conclusion = 'failure'::text)" {
		t.Fatalf("delivery_run_jobs_failed's predicate = %q, want (conclusion = 'failure'::text): the index holds the failed jobs alone", predicate)
	}

	namedJobs, err := ListRunJobsForRuns(ctx, pool, "acme/widgets", runs, names)
	if err != nil {
		t.Fatalf("ListRunJobsForRuns: %v", err)
	}
	failedJobs, err := listRunJobsFailedForRuns(ctx, pool, "acme/widgets", runs, names)
	if err != nil {
		t.Fatalf("listRunJobsFailedForRuns: %v", err)
	}
	byRun := namedAndFailedJobs(listed, namedJobs, failedJobs)
	every, err := listEveryRunJobForRuns(ctx, pool, "acme/widgets", runs)
	if err != nil {
		t.Fatalf("listEveryRunJobForRuns: %v", err)
	}
	for id, want := range map[int64][]string{
		840: {"deploy-verdict", "test / unit", "production"},
		841: {"production / apply"},
		842: {"production / apply", "page-oncall", "release-verdict"},
		843: {"production / apply"},
	} {
		var got []string
		for _, job := range byRun[id] {
			got = append(got, job.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("run %d's named and failed jobs = %q, want %q", id, got, want)
		}
		if got, want := FailedJobs(byRun[id]), FailedJobs(every[id]); !reflect.DeepEqual(got, want) {
			t.Errorf("run %d's FailedJobs of the read = %+v, of every job %+v", id, got, want)
		}
		if got, want := RootFailingJob(byRun[id]), RootFailingJob(every[id]); !reflect.DeepEqual(got, want) {
			t.Errorf("run %d's RootFailingJob of the read = %+v, of every job %+v", id, got, want)
		}
		if got, want := production.Job(byRun[id]), production.Job(every[id]); !reflect.DeepEqual(got, want) {
			t.Errorf("run %d's production job of the read = %+v, of every job %+v", id, got, want)
		}
	}
	if got, want := ProductionApplies(listed, byRun, production).Applies, ProductionApplies(listed, every, production).Applies; !reflect.DeepEqual(got, want) {
		t.Errorf("the deploys of the read = %+v, of every job %+v", got, want)
	}
	// The fixture tells the two apart: a root failing job past a guard, a guard as the root, a
	// deploy, and jobs that ended otherwise than in failure.
	if root := RootFailingJob(byRun[840]); root == nil || root.Name != "test / unit" {
		t.Errorf("run 840's root failing job = %+v, want test / unit, past the guard that failed first", root)
	}
	if root := RootFailingJob(byRun[842]); root == nil || root.Name != "release-verdict" {
		t.Errorf("run 842's root failing job = %+v, want release-verdict, the first of its guards to finish", root)
	}
	if applies := ProductionApplies(listed, every, production).Applies; len(applies) != 1 || applies[0].RunID != 842 {
		t.Errorf("the deploys of every job = %+v, want run 842's alone", applies)
	}

	none, err := listRunJobsFailedForRuns(ctx, pool, "acme/widgets", runs, nil)
	if err != nil {
		t.Fatalf("listRunJobsFailedForRuns with no name: %v", err)
	}
	failures := 0
	for _, id := range runs {
		failures += len(FailedJobs(every[id]))
		for _, job := range none[id] {
			if job.Conclusion == nil || *job.Conclusion != failure {
				t.Errorf("listRunJobsFailedForRuns with no name answers run %d's %s, which did not fail", id, job.Name)
			}
		}
	}
	answered := 0
	for _, jobs := range none {
		answered += len(jobs)
	}
	if answered != failures {
		t.Errorf("listRunJobsFailedForRuns with no name answers %d jobs, want the %d that failed", answered, failures)
	}
}

// requireTheNamedJobsReadIsIndexOnly vacuums delivery_run_jobs, as autovacuum keeps a table that is
// written, and holds the plan of runJobsNamedForRunsQuery over runs and names to one Index Only
// Scan of delivery_run_jobs_named, whose index condition holds the repository, the names and the
// runs, and which fetches no row from the heap: the vacuum marked every page of the table
// all-visible in the visibility map, which an index-only scan reads in place of the heap.
//
// A vacuum marks a page all-visible only where each row's committed hint is set, and the hint of a
// row whose transaction committed asynchronously waits for that commit to be flushed, as the test
// servers' synchronous_commit off leaves the jobs' writes for a moment. A synchronous commit
// flushes the log past every commit before it, but only where its transaction wrote to the log:
// a temporary table's catalog rows are such a write, and the commit drops the table.
func requireTheNamedJobsReadIsIndexOnly(t *testing.T, ctx context.Context, pool *store.Pool, runs []int64, names []string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the synchronous commit: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{`set local synchronous_commit = on`, `create temporary table log_flush () on commit drop`} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit synchronously: %v", err)
	}
	if _, err := pool.Exec(ctx, `vacuum (analyze) delivery_run_jobs`); err != nil {
		t.Fatalf("vacuum delivery_run_jobs: %v", err)
	}
	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, runJobsNamedForRunsQuery, "acme/widgets", runs, names)
	if read := scannedRelations(t, plan); !slices.Equal(read, []string{"delivery_run_jobs"}) {
		t.Fatalf("the run jobs read by name reads %v; want delivery_run_jobs alone:\n%s", read, raw)
	}
	scan := indexScanOf(t, plan, raw, "delivery_run_jobs_named")
	if scan.NodeType != "Index Only Scan" {
		t.Fatalf("the run jobs read by name reads delivery_run_jobs_named by an %s; want an Index Only Scan:\n%s", scan.NodeType, raw)
	}
	for _, key := range []string{"repo =", "name = ANY", "run_id = ANY"} {
		if !strings.Contains(scan.Cond, key) {
			t.Fatalf("the run jobs read's index condition %q holds no %q; want each of repo, name and run_id to key delivery_run_jobs_named:\n%s", scan.Cond, key, raw)
		}
	}
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Index == "delivery_run_jobs_named" && node.HeapFetches != 0 {
			t.Fatalf("the run jobs read by name fetched %g rows from the heap of the vacuumed table; want none:\n%s", node.HeapFetches, raw)
		}
	}
}
