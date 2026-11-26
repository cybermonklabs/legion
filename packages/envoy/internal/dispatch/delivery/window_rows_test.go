package delivery

import (
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
// with over every apply run, and each deploy shipping the same pull requests. The comparison fails
// where no window's apply runs stopped short of every apply run, since the bound was then never
// tested. Each round's seed is in its failure.
func TestAWindowsReadAnswersWhatItsTwoReadsAnswer(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	settings := DeliverySettings{DeployRepo: waitingRepo, ProductionJobNames: []string{waitingJob}, ProductionApplyStepName: new(waitingStep)}
	production := settings.Production()
	judged := production.JudgedJobNames()
	bounded, windows := 0, 0
	for seed := range uint64(24) {
		random := rand.New(rand.NewPCG(seed, 2916))
		seedWindowHistory(t, ctx, pool, random, base)
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
				every, err := listEveryRunJobForRuns(ctx, pool, waitingRepo, runIDsOf(merged.ApplyRuns))
				if err != nil {
					t.Fatalf("%s: listEveryRunJobForRuns: %v", label, err)
				}
				for _, run := range merged.ApplyRuns {
					want := slices.DeleteFunc(every[run.RunID], func(job DeliveryRunJob) bool {
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
			}
		}
	}
	if bounded == 0 {
		t.Fatalf("none of %d windows read fewer apply runs than every apply run, so the bound was never tested", windows)
	}
	t.Logf("%d windows, %d of them reading fewer apply runs than every apply run", windows, bounded)
}

// TestTheWindowsRunsReadIsBoundedThroughItsIndexes: under the generic plan pgx's cached statement
// settles on, the read of a window's apply runs reads delivery_runs_kind_created from the window's
// start to the bound of the first deploy created at or after its end, and the read of its deploys
// beside them delivery_runs_kind_started_at over the window, so neither reads every run created
// since the window began nor scans delivery_runs; and the read of several days' runs
// (daysRunsQuery) reads each day's the same way, from the day's start.
//
// The plans are taken over a year of deploy runs, each with its production job, analyzed: over an
// empty table every index costs one row, and which of two indexes leading with (repo, kind) the
// planner takes is a tie the plan would settle by chance.
func TestTheWindowsRunsReadIsBoundedThroughItsIndexes(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
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

// runIDsOf is the ids of runs, in order.
func runIDsOf(runs []DeliveryRun) []int64 {
	ids := make([]int64, len(runs))
	for i, run := range runs {
		ids[i] = run.RunID
	}
	return ids
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
