package delivery

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestThePRChecksReadByCompletionReadsTheCompletionIndex (dispatch://INFRA-2918, D9): the read of
// pr-ci's failing jobs takes the PR-checks runs of a range that completed in the 24 hours before
// its end through delivery_runs_kind_completed_at (migration 20261123042253), one range of the
// index for the repository, the kind and the completion, and not through
// delivery_runs_kind_started_at's range of every run started in the 84 days. Over 84 days of a run
// an hour, each done half an hour after it started, beside a deploy run of each day and a run of
// the day still going, `EXPLAIN (ANALYZE, BUFFERS)` of ReadPRChecksCompletedIn's statement reads
// delivery_runs through that index alone, under an index condition on completed_at, and answers
// the 24 hours' runs; ReadPRChecksCompletedIn answers those runs, both ends of the 24 hours
// counted, oldest start first, each with every job it ran.
func TestThePRChecksReadByCompletionReadsTheCompletionIndex(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	end := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	from := end.AddDate(0, 0, -84)
	if _, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at,
			conclusion, url, head_branch, event)
		select 'acme/widgets', hour, 'pr_checks', 7, lpad(hour::text, 40, '0'), $1::timestamptz + hour * interval '1 hour',
			$1::timestamptz + hour * interval '1 hour', $1::timestamptz + hour * interval '1 hour',
			$1::timestamptz + hour * interval '1 hour' + interval '30 minutes', 'failure',
			'https://github.com/acme/widgets/actions/runs/' || hour, 'feature', 'pull_request'
		from generate_series(0, 84 * 24 - 1) as hour
		union all
		select 'acme/widgets', 100000 + day, 'deploy', null, lpad((100000 + day)::text, 40, '0'), $1::timestamptz + day * interval '1 day',
			$1::timestamptz + day * interval '1 day', $1::timestamptz + day * interval '1 day',
			$1::timestamptz + day * interval '1 day' + interval '20 minutes', 'success',
			'https://github.com/acme/widgets/actions/runs/' || (100000 + day), 'main', 'push'
		from generate_series(0, 83) as day
		union all
		select 'acme/widgets', 200000, 'pr_checks', 7, lpad('200000', 40, '0'), $2::timestamptz - interval '1 hour',
			$2::timestamptz - interval '1 hour', $2::timestamptz - interval '1 hour', null, null,
			'https://github.com/acme/widgets/actions/runs/200000', 'feature', 'pull_request'
	`, from, end); err != nil {
		t.Fatalf("seed the runs: %v", err)
	}
	horizon := end.Add(-24 * time.Hour)
	var want []int64
	for hour := int64(0); hour < 84*24; hour++ {
		if completed := from.Add(time.Duration(hour)*time.Hour + 30*time.Minute); !completed.Before(horizon) && !completed.After(end) {
			want = append(want, hour)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select 'acme/widgets', run_id, job.name, started_at, completed_at, job.conclusion
		from delivery_runs, (values ('build / unit', 'failure'), ('lint', 'success')) as job (name, conclusion)
		where repo = 'acme/widgets' and run_id = any($1)
	`, want); err != nil {
		t.Fatalf("seed the jobs of the 24 hours' runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze delivery_runs`); err != nil {
		t.Fatalf("analyze delivery_runs: %v", err)
	}

	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, prChecksCompletedInQuery,
		"acme/widgets", string(DeliveryRunKindPRChecks), "pull_request", from, end, horizon, end, MaxRunsPerWindow)
	if read := scannedRelations(t, plan); !slices.Equal(read, []string{"delivery_runs"}) {
		t.Fatalf("the read reads %v; want delivery_runs alone:\n%s", read, raw)
	}
	scan := indexScanOf(t, plan, raw, "delivery_runs_kind_completed_at")
	for _, key := range []string{"repo =", "kind =", "completed_at >=", "completed_at <="} {
		if !strings.Contains(scan.Cond, key) {
			t.Errorf("the read's index condition %q holds no %q:\n%s", scan.Cond, key, raw)
		}
	}
	for _, other := range explaintest.IndexScans(t, plan) {
		if other.Index != "delivery_runs_kind_completed_at" {
			t.Errorf("the read also reads %s:\n%s", other, raw)
		}
	}
	for _, read := range explaintest.Scans(t, plan) {
		if read.Relation == "delivery_runs" && int(read.Rows) != len(want) {
			t.Errorf("the read answers %v runs; want the %d of the 24 hours:\n%s", read.Rows, len(want), raw)
		}
	}

	read, err := ReadPRChecksCompletedIn(ctx, pool, "acme/widgets", from, end, horizon, end)
	if err != nil {
		t.Fatalf("ReadPRChecksCompletedIn: %v", err)
	}
	got := make([]int64, len(read.Runs))
	for index, run := range read.Runs {
		got[index] = run.RunID
		if names := len(read.Jobs[run.RunID]); names != 2 {
			t.Errorf("run %d answers %d jobs; want both it ran", run.RunID, names)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadPRChecksCompletedIn answers the runs %v; want %v, those completed in %s..%s", got, want,
			horizon.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	t.Logf("the read takes %d runs through %s (%s) of %d runs of the range", len(got), scan.Index, scan.Cond, 84*24+84+1)
}
