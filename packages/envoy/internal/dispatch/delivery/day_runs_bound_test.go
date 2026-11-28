package delivery

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestOneDaysRunsPastTheBoundLeaveEveryOtherDayAnswered: a read of several days answers each day
// what its own window's read answers (ReadCIDays what ReadCIRows answers, ReadDaysDeploys what
// ReadDeploys answers), and one day's runs past the bound of a window's runs (maxRunsPerWindow)
// leave every other day's runs answered, as listPullRequestsInWindows leaves every other day's
// pull requests answered past maxPullRequestsPerWindow. The first day holds 2*maxRunsPerWindow
// PR-checks runs and as many deploy pushes on main; the second day holds one PR-checks run with its
// job and one deploy that ships. A limit the days share (maxRunsPerWindow times their number,
// oldest run first) spends itself on the first day and answers the second day none of its runs,
// with no error and no refusal (dispatch://INFRA-6225, the tester's round at 238703c7).
func TestOneDaysRunsPastTheBoundLeaveEveryOtherDayAnswered(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	names := settings.Production().JudgedJobNames()
	days := []time.Time{time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)}
	full := 2 * maxRunsPerWindow
	// The change log's trigger is off for the seed, as seedPullRequestsASecondApart turns it off.
	if _, err := pool.Exec(ctx, `alter table delivery_runs disable trigger delivery_runs_changed`); err != nil {
		t.Fatalf("stop logging the runs' changes: %v", err)
	}
	for _, seed := range []struct {
		kind, branch, event string
		first               int
	}{{"pr_checks", "feature/x", "pull_request", 1}, {"deploy", "main", "push", 1_000_000}} {
		if _, err := pool.Exec(ctx, `
			insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at,
				started_at, completed_at, created_at, url, head_branch, event, conclusion)
			select $1::text, $2::bigint + n, $3::text, case when $3::text = 'pr_checks' then (n % 1000)::int + 1 end,
				lpad(($2::bigint + n)::text, 40, '0'), at, at, at + interval '1 minute', at,
				'https://github.com/' || $1::text || '/actions/runs/' || ($2::bigint + n), $4::text, $5::text, 'success'
			from generate_series(0, $6::int - 1) n, lateral (select $7::timestamptz + n * interval '500 milliseconds' as at) moment
		`, settings.DeployRepo, seed.first, seed.kind, seed.branch, seed.event, full, days[0].Add(time.Minute)); err != nil {
			t.Fatalf("seed %d %s runs on the first day: %v", full, seed.kind, err)
		}
	}
	second := days[1].Add(2 * time.Hour)
	if _, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at,
			started_at, completed_at, created_at, url, head_branch, event, conclusion)
		values ($1, 5000001, 'pr_checks', 7, lpad('5000001', 40, '0'), $2, $2, $2::timestamptz + interval '10 minutes', $2,
			'https://github.com/' || $1 || '/actions/runs/5000001', 'feature/y', 'pull_request', 'success'),
		($1, 5000002, 'deploy', null, lpad('5000002', 40, '0'), $2, $2, $2::timestamptz + interval '1 hour', $2,
			'https://github.com/' || $1 || '/actions/runs/5000002', 'main', 'push', 'success')
	`, settings.DeployRepo, second); err != nil {
		t.Fatalf("seed the second day's runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion, apply_step, apply_step_conclusion)
		values ($1, 5000001, 'test / unit', $2, $2::timestamptz + interval '10 minutes', 'success', null, null),
		($1, 5000002, $3, $2, $2::timestamptz + interval '1 hour', 'success', $4, 'success')
	`, settings.DeployRepo, second, waitingJob, waitingStep); err != nil {
		t.Fatalf("seed the second day's jobs: %v", err)
	}
	// The planner reads the tables as a database that has run its autovacuum does.
	if _, err := pool.Exec(ctx, `analyze delivery_runs, delivery_run_jobs`); err != nil {
		t.Fatalf("analyze the runs: %v", err)
	}
	day := days[1]

	t.Run("ReadCIDays", func(t *testing.T) {
		got, err := ReadCIDays(ctx, pool, settings.DeployRepo, days, names)
		if err != nil {
			t.Fatalf("ReadCIDays: %v", err)
		}
		want, err := ReadCIRows(ctx, pool, settings.DeployRepo, day, day.AddDate(0, 0, 1), names)
		if err != nil {
			t.Fatalf("ReadCIRows of the second day: %v", err)
		}
		if len(want.Checks.Runs) != 1 || len(want.Deploys.Runs) != 1 {
			t.Fatalf("the second day's own read holds %d PR-checks runs and %d deploy pushes, want the seeded 1 and 1", len(want.Checks.Runs), len(want.Deploys.Runs))
		}
		if !reflect.DeepEqual(got[1].Checks, want.Checks) {
			t.Errorf("the second day's PR-checks runs number %d, want its window's %d, whatever the first day holds (%d runs read for it)",
				len(got[1].Checks.Runs), len(want.Checks.Runs), len(got[0].Checks.Runs))
		}
		if !reflect.DeepEqual(got[1].Deploys, want.Deploys) {
			t.Errorf("the second day's deploy pushes number %d, want its window's %d, whatever the first day holds (%d runs read for it)",
				len(got[1].Deploys.Runs), len(want.Deploys.Runs), len(got[0].Deploys.Runs))
		}
	})
	for _, applies := range []bool{false, true} {
		t.Run(fmt.Sprintf("ReadDaysDeploys applies=%v", applies), func(t *testing.T) {
			got, err := ReadDaysDeploys(ctx, pool, settings, days, applies)
			if err != nil {
				t.Fatalf("ReadDaysDeploys: %v", err)
			}
			want, err := unrenamed().ReadDeploys(ctx, pool, settings, day, day.AddDate(0, 0, 1))
			if err != nil {
				t.Fatalf("ReadDeploys of the second day: %v", err)
			}
			if len(want.Started.Runs) != 1 || len(want.Applies) != 1 {
				t.Fatalf("the second day's own read holds %d deploy runs and %d deploys, want the seeded 1 and 1", len(want.Started.Runs), len(want.Applies))
			}
			if got.Refused[1] != nil || !reflect.DeepEqual(got.Deploys[1], want) {
				t.Errorf("the second day holds %d deploy runs and %d deploys (refusal %v), want its window's %d and %d, whatever the first day holds (%d runs read for it)",
					len(got.Deploys[1].Started.Runs), len(got.Deploys[1].Applies), got.Refused[1], len(want.Started.Runs), len(want.Applies), len(got.Deploys[0].Started.Runs))
			}
		})
	}
}
