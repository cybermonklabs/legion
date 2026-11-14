package delivery

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The run jobs read names the jobs it reads in SQL, and the name is a key of the read and not a
// filter after it: `EXPLAIN (ANALYZE, BUFFERS)` of ListRunJobsForRuns' statement reads
// delivery_run_jobs through its primary key (repo, run_id, name) under an index condition that
// holds the name predicate, and answers only the named jobs of each run. Unfiltered, this read
// cost 660 ms a call at ten times the stated run rate (dispatch://INFRA-2916, R13), most of it the
// pull-request runs' dozens of jobs no caller of it reads.
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

	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, runJobsNamedForRunsQuery, "acme/widgets", runs, named)
	if read := scannedRelations(t, plan); !slices.Equal(read, []string{"delivery_run_jobs"}) {
		t.Fatalf("the run jobs read reads %v; want delivery_run_jobs alone:\n%s", read, raw)
	}
	scan := indexScanOf(t, plan, raw, "delivery_run_jobs", "delivery_run_jobs_pkey")
	for _, key := range []string{"repo =", "run_id = ANY", "name = ANY"} {
		if !strings.Contains(scan.Cond, key) {
			t.Fatalf("the run jobs read's index condition %q holds no %q; want each of repo, run_id and name to key the primary key:\n%s", scan.Cond, key, raw)
		}
	}

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

// The timeline's read of its deploy runs' jobs (runJobsNamedOrFailedForRunsQuery) reads the named
// jobs of each run through the primary key, as the read by name does, and each run's failed jobs
// through delivery_run_jobs_failed, the partial index that holds the failed jobs alone, so it
// reads in proportion to the runs' failures rather than every job they ran: `EXPLAIN (ANALYZE,
// BUFFERS)` of its statement reads delivery_run_jobs through each of the two indexes once and
// through nothing else. Postgres plans no condition a partial index's predicate implies, so the
// failed part's index condition is the repository and the run, and its failure predicate is the
// index's own, read from pg_index. Each run's jobs it answers are its named jobs and its failed
// ones, in ListRunJobs' order, and FailedJobs, RootFailingJob, Production.Job and the deploys
// answer from them what they answer from every job the run holds.
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

	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, runJobsNamedOrFailedForRunsQuery, "acme/widgets", runs, names)
	if read := scannedRelations(t, plan); !slices.Equal(read, []string{"delivery_run_jobs"}) {
		t.Fatalf("the timeline's run jobs read reads %v; want delivery_run_jobs alone:\n%s", read, raw)
	}
	if explaintest.SeqScansRelation(t, plan, "delivery_run_jobs") {
		t.Fatalf("the timeline's run jobs read scans delivery_run_jobs whole:\n%s", raw)
	}
	named := indexScanOf(t, plan, raw, "delivery_run_jobs", "delivery_run_jobs_pkey")
	for _, key := range []string{"repo =", "run_id = ANY", "name = ANY"} {
		if !strings.Contains(named.Cond, key) {
			t.Fatalf("the named part's index condition %q holds no %q; want each of repo, run_id and name to key the primary key:\n%s", named.Cond, key, raw)
		}
	}
	failed := indexScanOf(t, plan, raw, "delivery_run_jobs", "delivery_run_jobs_failed")
	for _, key := range []string{"repo =", "run_id = ANY"} {
		if !strings.Contains(failed.Cond, key) {
			t.Fatalf("the failed part's index condition %q holds no %q; want repo and run_id to key delivery_run_jobs_failed:\n%s", failed.Cond, key, raw)
		}
	}
	var predicate string
	if err := pool.QueryRow(ctx, `select coalesce(pg_get_expr(indpred, indrelid), '') from pg_index where indexrelid = 'delivery_run_jobs_failed'::regclass`).Scan(&predicate); err != nil {
		t.Fatalf("read delivery_run_jobs_failed's predicate: %v", err)
	}
	if predicate != "(conclusion = 'failure'::text)" {
		t.Fatalf("delivery_run_jobs_failed's predicate = %q, want (conclusion = 'failure'::text): the index holds the failed jobs alone", predicate)
	}

	byRun, err := listRunJobsNamedOrFailedForRuns(ctx, pool, "acme/widgets", runs, names)
	if err != nil {
		t.Fatalf("listRunJobsNamedOrFailedForRuns: %v", err)
	}
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

	none, err := listRunJobsNamedOrFailedForRuns(ctx, pool, "acme/widgets", runs, nil)
	if err != nil {
		t.Fatalf("listRunJobsNamedOrFailedForRuns with no name: %v", err)
	}
	failures := 0
	for _, id := range runs {
		failures += len(FailedJobs(every[id]))
		for _, job := range none[id] {
			if job.Conclusion == nil || *job.Conclusion != failure {
				t.Errorf("listRunJobsNamedOrFailedForRuns with no name answers run %d's %s, which did not fail", id, job.Name)
			}
		}
	}
	answered := 0
	for _, jobs := range none {
		answered += len(jobs)
	}
	if answered != failures {
		t.Errorf("listRunJobsNamedOrFailedForRuns with no name answers %d jobs, want the %d that failed", answered, failures)
	}
}
