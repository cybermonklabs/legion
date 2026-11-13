package delivery

import (
	"slices"
	"strings"
	"testing"
	"time"
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

	plan, raw := analyzedPlan(t, ctx, pool, runJobsNamedForRunsQuery, "acme/widgets", runs, named)
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
	every, err := ListEveryRunJobForRuns(ctx, pool, "acme/widgets", runs)
	if err != nil {
		t.Fatalf("ListEveryRunJobForRuns: %v", err)
	}
	if got := len(every[runs[0]]); got != len(held) {
		t.Errorf("ListEveryRunJobForRuns' run %d holds %d jobs, want all %d", runs[0], got, len(held))
	}
}
