package delivery

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

// TestEachDaysReadAnswersWhatItsWindowsReadAnswers checks R2 of dispatch://INFRA-6225:
// the day and history reads keep the window reads' answers and refuse an overfull day
// without partial rows.
func TestEachDaysReadAnswersWhatItsWindowsReadAnswers(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	production := settings.Production()
	names := production.JudgedJobNames()
	days := []time.Time{base.AddDate(0, 0, 2), base.AddDate(0, 0, 3), base.AddDate(0, 0, 4)}
	seedWindowHistory(t, ctx, pool, rand.New(rand.NewPCG(0, 2916)), base)
	if _, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at,
			started_at, completed_at, created_at, url, head_branch, event, conclusion)
		select $1::text, 10000 + n, 'pr_checks', n::int, lpad(n::text, 40, '0'), day,
			day, day + interval '10 minutes', day,
			'https://github.com/' || $1::text || '/actions/runs/' || (10000 + n), 'feature/checks', 'pull_request', 'success'
		from unnest($2::timestamptz[]) with ordinality as days(day, n)
	`, settings.DeployRepo, days); err != nil {
		t.Fatalf("seed the days' PR-checks runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select repo, run_id, 'test / unit', started_at, completed_at, 'success'
		from delivery_runs where repo = $1 and kind = 'pr_checks'
	`, settings.DeployRepo); err != nil {
		t.Fatalf("seed the days' PR-checks jobs: %v", err)
	}

	checkPullRequests := func(t *testing.T, overfull int) {
		t.Helper()
		got, refused, err := ListPullRequestsOnDays(ctx, pool, days)
		if err != nil {
			t.Fatalf("ListPullRequestsOnDays: %v", err)
		}
		if len(got) != len(days) || len(refused) != len(days) {
			t.Fatalf("ListPullRequestsOnDays returned %d lists and %d refusals for %d days", len(got), len(refused), len(days))
		}
		for i, day := range days {
			to := day.AddDate(0, 0, 1)
			if i == overfull {
				r := refused[i]
				window := "[" + day.Format(time.RFC3339Nano) + ", " + to.Format(time.RFC3339Nano) + ")"
				if len(got[i]) != 0 || r == nil || !r.From.Equal(day) || !r.To.Equal(to) || r.Repo != "" ||
					r.Bound != maxPullRequestsPerWindow || r.Window() != window {
					t.Errorf("%s: got %d pull requests and refusal %+v, want no rows and a refusal of %s at bound %d",
						day, len(got[i]), r, window, maxPullRequestsPerWindow)
				}
				continue
			}
			want, err := unrenamed().ListPullRequestsInWindow(ctx, pool, day, to)
			if err != nil {
				t.Fatalf("%s: ListPullRequestsInWindow: %v", day, err)
			}
			if refused[i] != nil || !reflect.DeepEqual(got[i], want) {
				t.Errorf("%s: got %d pull requests and refusal %+v, want the window's %d pull requests in order and no refusal",
					day, len(got[i]), refused[i], len(want))
			}
		}
	}

	for _, test := range []struct {
		name string
		read func(t *testing.T)
	}{
		{"ListPullRequestsOnDays", func(t *testing.T) {
			checkPullRequests(t, -1)
		}},
		{"ReadDaysDeploys", func(t *testing.T) {
			got, err := ReadDaysDeploys(ctx, pool, settings, days, false)
			if err != nil {
				t.Fatalf("ReadDaysDeploys without applies: %v", err)
			}
			if len(got.Deploys) != len(days) || len(got.Refused) != len(days) {
				t.Fatalf("ReadDaysDeploys returned %d deploy lists and %d refusals for %d days", len(got.Deploys), len(got.Refused), len(days))
			}
			for i, day := range days {
				to := day.AddDate(0, 0, 1)
				want, err := unrenamed().ReadDeploys(ctx, pool, settings, day, to)
				if err != nil {
					t.Fatalf("%s: ReadDeploys: %v", day, err)
				}
				if got.Refused[i] != nil || !reflect.DeepEqual(got.Deploys[i], want) {
					t.Errorf("%s: deploys = %+v, refusal = %+v, want the window's deploys %+v and no refusal", day, got.Deploys[i], got.Refused[i], want)
				}
				merged, err := unrenamed().ReadMergedRows(ctx, pool, settings, day, to)
				if err != nil {
					t.Fatalf("%s: ReadMergedRows: %v", day, err)
				}
				if gotShipped, wantShipped := shippedIDs(got.Deploys[i].ShippedBy(merged, settings)), shippedIDs(want.ShippedBy(merged, settings)); !reflect.DeepEqual(gotShipped, wantShipped) {
					t.Errorf("%s: the day's deploys ship %v, want %v", day, gotShipped, wantShipped)
				}
			}
		}},
		{"ReadDaysDeploys with applies", func(t *testing.T) {
			got, err := ReadDaysDeploys(ctx, pool, settings, days, true)
			if err != nil {
				t.Fatalf("ReadDaysDeploys with applies: %v", err)
			}
			truth := readWindowTruth(t, ctx, pool, production)
			shipped := 0
			for _, day := range days {
				part, err := got.Applies.Since(day)
				if err != nil {
					t.Fatalf("%s: take the day's applies: %v", day, err)
				}
				whole, err := ReadApplyRows(ctx, pool, settings, day)
				if err != nil {
					t.Fatalf("%s: ReadApplyRows: %v", day, err)
				}
				applies := ProductionApplies(part.Runs, part.Jobs, production).Applies
				wantApplies := ProductionApplies(whole.Runs, whole.Jobs, production).Applies
				to := day.AddDate(0, 0, 1)
				for _, pr := range truth.prs {
					if !pr.MergedAt.Before(to) {
						continue
					}
					want := ContainingRun(pr, settings.DeployRepo, wantApplies)
					if want != nil {
						shipped++
					}
					if got := ContainingRun(pr, settings.DeployRepo, applies); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: %s#%d ships first with %+v, want %+v from the full apply read", day, pr.Repo, pr.Number, got, want)
					}
				}
			}
			if shipped == 0 {
				t.Fatal("the fixture has no pull request that ships in the apply reads")
			}
		}},
		{"ReadApplyRows and Since", func(t *testing.T) {
			from := days[0]
			applies, err := ReadApplyRows(ctx, pool, settings, from)
			if err != nil {
				t.Fatalf("ReadApplyRows: %v", err)
			}
			for _, day := range days {
				got, err := applies.Since(day)
				if err != nil {
					t.Fatalf("%s: Since: %v", day, err)
				}
				want, err := ReadApplyRows(ctx, pool, settings, day)
				if err != nil {
					t.Fatalf("%s: ReadApplyRows: %v", day, err)
				}
				if !reflect.DeepEqual(got.Runs, want.Runs) || !reflect.DeepEqual(got.Jobs, want.Jobs) {
					t.Errorf("%s: Since returned runs %v and jobs %v, want runs %v and jobs %v", day, runIDsOf(got.Runs), got.Jobs, runIDsOf(want.Runs), want.Jobs)
				}
			}
			if _, err := applies.Since(from.AddDate(0, 0, -1)); err == nil {
				t.Errorf("Since accepted a day before the read's start %s", from)
			}
		}},
		{"ReadCIDays", func(t *testing.T) {
			got, err := ReadCIDays(ctx, pool, settings.DeployRepo, days, names)
			if err != nil {
				t.Fatalf("ReadCIDays: %v", err)
			}
			if len(got) != len(days) {
				t.Fatalf("ReadCIDays returned %d rows for %d days", len(got), len(days))
			}
			for i, day := range days {
				want, err := ReadCIRows(ctx, pool, settings.DeployRepo, day, day.AddDate(0, 0, 1), names)
				if err != nil {
					t.Fatalf("%s: ReadCIRows: %v", day, err)
				}
				id := int64(10001 + i)
				if len(want.Checks.Runs) != 1 || want.Checks.Runs[0].RunID != id || len(want.Checks.Jobs[id]) != 1 {
					t.Fatalf("%s: the window's checks = %+v, want seeded run %d and its job", day, want.Checks, id)
				}
				if !reflect.DeepEqual(got[i].Checks, want.Checks) {
					t.Errorf("%s: checks = %+v, want the window's %+v", day, got[i].Checks, want.Checks)
				}
				if !reflect.DeepEqual(got[i].Deploys, want.Deploys) {
					t.Errorf("%s: CI deploys = %+v, want the window's %+v", day, got[i].Deploys, want.Deploys)
				}
			}
		}},
		{"ReadCheckRows", func(t *testing.T) {
			from, to := days[0], days[len(days)-1].AddDate(0, 0, 1)
			got, err := ReadCheckRows(ctx, pool, settings.DeployRepo, from, to)
			if err != nil {
				t.Fatalf("ReadCheckRows: %v", err)
			}
			want, err := ReadCIRows(ctx, pool, settings.DeployRepo, from, to, names)
			if err != nil {
				t.Fatalf("ReadCIRows: %v", err)
			}
			if !reflect.DeepEqual(got, want.Checks) {
				t.Errorf("checks = %+v, want ReadCIRows' checks %+v", got, want.Checks)
			}
		}},
		{"ReadPullRequestsMergedBefore", func(t *testing.T) {
			to := days[len(days)-1].AddDate(0, 0, 1)
			got, err := ReadPullRequestsMergedBefore(ctx, pool, to)
			if err != nil {
				t.Fatalf("ReadPullRequestsMergedBefore: %v", err)
			}
			history, err := ReadHistoryRows(ctx, pool, nil, nil, to)
			if err != nil {
				t.Fatalf("ReadHistoryRows: %v", err)
			}
			if !reflect.DeepEqual(got, history.PullRequests) {
				t.Errorf("pull requests merged before %s differ from ReadHistoryRows (%d rows versus %d)", to, len(got), len(history.PullRequests))
			}
			want, err := unrenamed().ListPullRequestsInWindow(ctx, pool, time.Time{}, to)
			if err != nil {
				t.Fatalf("ListPullRequestsInWindow before %s: %v", to, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("pull requests merged before %s differ from ListPullRequestsInWindow's read of them (%d rows versus %d)", to, len(got), len(want))
			}
		}},
		// Run the overfull row last so only it reads the extra pull requests.
		{"ListPullRequestsOnDays past the bound", func(t *testing.T) {
			const overfull = 1
			day := days[overfull]
			prs, err := unrenamed().ListPullRequestsInWindow(ctx, pool, day, day.AddDate(0, 0, 1))
			if err != nil {
				t.Fatalf("read the day before adding pull requests: %v", err)
			}
			seedPullRequestsASecondApart(t, ctx, pool, "acme/overflow", 1000, maxPullRequestsPerWindow+1-len(prs), day)
			checkPullRequests(t, overfull)
		}},
	} {
		t.Run(test.name, test.read)
	}
}
