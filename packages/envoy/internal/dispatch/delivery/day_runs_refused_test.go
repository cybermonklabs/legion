package delivery

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestADayPastTheRunBoundIsRefusedByNameAndTheNextDayAnswered holds the clause of R2 of
// dispatch://INFRA-6225 that bounds each day's own runs separately: answer a day at the bound
// whole, refuse a day past it alone and by name with no rows, and still answer the next day whole.
func TestADayPastTheRunBoundIsRefusedByNameAndTheNextDayAnswered(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	names := settings.Production().JudgedJobNames()
	days := []time.Time{time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)}
	end := days[1].AddDate(0, 0, 1)

	// The test database does not need a change-log row per seeded run.
	if _, err := pool.Exec(ctx, `alter table delivery_runs disable trigger delivery_runs_changed`); err != nil {
		t.Fatalf("stop logging the runs' changes: %v", err)
	}
	seedFirstDay := func(t *testing.T, from, to int) {
		t.Helper()
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
				from generate_series($6::int, $7::int - 1) n, lateral (select $8::timestamptz + n * interval '500 milliseconds' as at) moment
			`, settings.DeployRepo, seed.first, seed.kind, seed.branch, seed.event, from, to, days[0].Add(time.Minute)); err != nil {
				t.Fatalf("seed %d %s runs on the first day: %v", to-from, seed.kind, err)
			}
		}
	}
	analyze := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `analyze delivery_runs, delivery_run_jobs`); err != nil {
			t.Fatalf("analyze the runs: %v", err)
		}
	}

	seedFirstDay(t, 0, MaxRunsPerWindow)
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
	analyze(t)

	t.Run("at the bound", func(t *testing.T) {
		// Only ReadCIDays and ReadDaysDeploys without applies count exactly MaxRunsPerWindow
		// in each first-day list. The apply part would also count the second day's shipping run.
		ci, err := ReadCIDays(ctx, pool, settings.DeployRepo, days, names)
		if err != nil {
			t.Fatal(err)
		}
		wantCI, err := ReadCIRows(ctx, pool, settings.DeployRepo, days[0], days[1], names)
		if err != nil {
			t.Fatal(err)
		}
		if len(ci) != len(days) {
			t.Fatalf("ReadCIDays answered %d days, want %d", len(ci), len(days))
		}
		if ci[0].Refused != nil || ci[1].Refused != nil {
			t.Errorf("ReadCIDays refused a day at or below the bound: %v, %v", ci[0].Refused, ci[1].Refused)
		}
		if len(ci[0].Checks.Runs) != MaxRunsPerWindow || len(ci[0].Deploys.Runs) != MaxRunsPerWindow || !reflect.DeepEqual(ci[0].CIRows, wantCI) {
			t.Errorf("first day's CI holds %d checks and %d pushes, want all %d of each in ReadCIRows' order and with its jobs",
				len(ci[0].Checks.Runs), len(ci[0].Deploys.Runs), MaxRunsPerWindow)
		}

		deploys, err := ReadDaysDeploys(ctx, pool, settings, days, false)
		if err != nil {
			t.Fatal(err)
		}
		wantDeploys, err := ReadDeploys(ctx, pool, settings, days[0], days[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(deploys.Deploys) != len(days) || len(deploys.Refused) != len(days) {
			t.Fatalf("ReadDaysDeploys answered %d days with %d refusal slots, want %d of each", len(deploys.Deploys), len(deploys.Refused), len(days))
		}
		if deploys.Refused[0] != nil || deploys.Refused[1] != nil {
			t.Errorf("ReadDaysDeploys refused a day at or below the bound: %v, %v", deploys.Refused[0], deploys.Refused[1])
		}
		if len(deploys.Deploys[0].Started.Runs) != MaxRunsPerWindow || !reflect.DeepEqual(deploys.Deploys[0], wantDeploys) {
			t.Errorf("first day's deploys hold %d runs, want all %d in ReadDeploys' order and with its jobs", len(deploys.Deploys[0].Started.Runs), MaxRunsPerWindow)
		}
	})

	t.Run("one past the bound", func(t *testing.T) {
		seedFirstDay(t, MaxRunsPerWindow, MaxRunsPerWindow+1)
		analyze(t)
		firstWindow := fmt.Sprintf("[%s, %s)", days[0].Format(time.RFC3339Nano), days[1].Format(time.RFC3339Nano))
		checkRefusal := func(t *testing.T, got *RefusedWindow, want RefusedRows) {
			t.Helper()
			if got == nil || !got.From.Equal(days[0]) || !got.To.Equal(days[1]) || got.Repo != settings.DeployRepo || got.Bound != MaxRunsPerWindow || got.Rows != want {
				t.Fatalf("first day's refusal is %#v, want %s of %s in %s, bound %d", got, want, settings.DeployRepo, firstWindow, MaxRunsPerWindow)
			}
			text := got.String()
			for _, part := range []string{firstWindow, strconv.Itoa(MaxRunsPerWindow), settings.DeployRepo, string(want)} {
				if !strings.Contains(text, part) {
					t.Errorf("refusal %q does not name %q", text, part)
				}
			}
		}

		ci, err := ReadCIDays(ctx, pool, settings.DeployRepo, days, names)
		if err != nil {
			t.Fatal(err)
		}
		wantCI, err := ReadCIRows(ctx, pool, settings.DeployRepo, days[1], end, names)
		if err != nil {
			t.Fatal(err)
		}
		if len(ci) != len(days) {
			t.Fatalf("ReadCIDays answered %d days, want %d", len(ci), len(days))
		}
		checkRefusal(t, ci[0].Refused, RefusedPRChecksRuns) // PR-checks win when both parts overflow.
		if !reflect.DeepEqual(ci[0].CIRows, CIRows{}) {
			t.Error("ReadCIDays returned rows for the refused first day")
		}
		if ci[1].Refused != nil || !reflect.DeepEqual(ci[1].CIRows, wantCI) {
			t.Errorf("ReadCIDays did not match the second day's ReadCIRows (refusal: %v)", ci[1].Refused)
		}

		wantDeploys, err := ReadDeploys(ctx, pool, settings, days[1], end)
		if err != nil {
			t.Fatal(err)
		}
		for _, applies := range []bool{false, true} {
			t.Run(fmt.Sprintf("applies=%v", applies), func(t *testing.T) {
				deploys, err := ReadDaysDeploys(ctx, pool, settings, days, applies)
				if err != nil {
					t.Fatal(err)
				}
				if len(deploys.Deploys) != len(days) || len(deploys.Refused) != len(days) {
					t.Fatalf("ReadDaysDeploys answered %d days with %d refusal slots, want %d of each", len(deploys.Deploys), len(deploys.Refused), len(days))
				}
				checkRefusal(t, deploys.Refused[0], RefusedDeployRuns)
				if !reflect.DeepEqual(deploys.Deploys[0], Deploys{}) {
					t.Error("ReadDaysDeploys returned deploys for the refused first day")
				}
				if deploys.Refused[1] != nil || !reflect.DeepEqual(deploys.Deploys[1], wantDeploys) {
					t.Errorf("ReadDaysDeploys did not match the second day's ReadDeploys (refusal: %v)", deploys.Refused[1])
				}
				if applies {
					if _, err := deploys.Applies.Since(days[0]); err == nil {
						t.Error("Applies.Since answered apply runs for the refused first day")
					}
					if runs := deploys.Applies.Runs; len(runs) != 1 || runs[0].RunID != 5000002 {
						t.Errorf("shared apply list has %d runs, want only run 5000002 from the answered second day", len(runs))
					}
				}
			})
		}
	})
}
