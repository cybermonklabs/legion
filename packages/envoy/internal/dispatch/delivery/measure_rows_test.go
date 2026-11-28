package delivery

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestADeployHistoryReadAFewRunsAtATimeAnswersWhatOneReadAnswers: ReadDeployHistory hands each
// repository's runs to the readers of their jobs a few at a time while it lists on, so every
// repository's deploy runs on main and their jobs of the names asked, each list's last short
// handful of runs among them, come back as ReadHistoryRows' one read answers them, and no run of
// another branch or kind, nor a job of another name.
func TestADeployHistoryReadAFewRunsAtATimeAnswersWhatOneReadAnswers(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	began := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	branch, event, other := "main", "push", "feature"
	success := DeliveryJobConclusionSuccess
	repos := []string{"acme/widgets", "acme/gadgets"}
	for r, repo := range repos {
		for i := range 7 {
			id := int64(1000*(r+1) + i)
			at := began.Add(time.Duration(i) * time.Hour)
			done := at.Add(10 * time.Minute)
			run := DeliveryRun{Repo: repo, RunID: id, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("sha%d", id), HeadCommitAt: at,
				StartedAt: at, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id), HeadBranch: &branch, Event: &event}
			switch i {
			case 3:
				run.HeadBranch = &other
			case 5:
				run.Kind = DeliveryRunKindPRChecks
			}
			if err := UpsertRun(ctx, pool, run); err != nil {
				t.Fatalf("UpsertRun %d: %v", id, err)
			}
			var jobs []DeliveryRunJob
			for _, name := range []string{"apply", "plan", "notify"} {
				if name == "plan" && i%2 == 0 {
					continue
				}
				jobs = append(jobs, DeliveryRunJob{Repo: repo, RunID: id, Name: name, StartedAt: &at, CompletedAt: &done, Conclusion: &success})
			}
			if err := UpsertRunJobs(ctx, pool, repo, id, jobs, true); err != nil {
				t.Fatalf("UpsertRunJobs %d: %v", id, err)
			}
		}
	}
	names := []string{"apply", "plan"}
	want, err := ReadHistoryRows(ctx, pool, repos, names, began.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ReadHistoryRows: %v", err)
	}
	got, _, err := ReadDeployHistory(ctx, pool, repos, names, 3, 2)
	if err != nil {
		t.Fatalf("ReadDeployHistory: %v", err)
	}
	for _, repo := range repos {
		if runs := len(want.Deploys[repo].Runs); runs != 5 {
			t.Fatalf("ReadHistoryRows read %d deploy runs on main of %s, want 5", runs, repo)
		}
		if !reflect.DeepEqual(got[repo], want.Deploys[repo]) {
			t.Errorf("ReadDeployHistory of %s read %d runs with the jobs of %d, ReadHistoryRows %d runs with the jobs of %d:\n%+v\nwant %+v",
				repo, len(got[repo].Runs), len(got[repo].Jobs), len(want.Deploys[repo].Runs), len(want.Deploys[repo].Jobs), got[repo], want.Deploys[repo])
		}
	}
}

// TestADaysPartOfApplyRowsIsTheEndOfItsRunsShared (dispatch://INFRA-2918, D25 step 6): the part of
// one read of apply rows a reader of many days takes for each day (ApplyRows.Since) is the runs
// created from that day on, in the read's order, with their jobs and no other run's; and its runs
// are the end of the read's own, shared, which an append to the part cannot write past, where a
// copy of every run created from each day on made a read of many days (the narrowed evals'
// read-time rows, evals.ComputeAtRead) grow with the days times the runs. Runs created in one
// instant, runs whose creation is unknown (read by their start), runs with no job and the day
// after the last run are among them.
func TestADaysPartOfApplyRowsIsTheEndOfItsRunsShared(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	read := ApplyRows{from: from, Jobs: map[int64][]DeliveryRunJob{}}
	for i := range 40 {
		created := from.Add(time.Duration(i/2) * 7 * time.Hour)
		run := DeliveryRun{Repo: "acme/widgets", RunID: int64(i + 1), Kind: DeliveryRunKindDeploy, StartedAt: created.Add(time.Minute), CreatedAt: &created}
		if i%5 == 0 {
			run.CreatedAt, run.StartedAt = nil, created
		}
		read.Runs = append(read.Runs, run)
		if i%3 != 0 {
			read.Jobs[run.RunID] = []DeliveryRunJob{{Repo: run.Repo, RunID: run.RunID, Name: "deploy / production"}}
		}
	}
	parts := 0
	for day := from; !day.After(from.AddDate(0, 0, 7)); day = day.AddDate(0, 0, 1) {
		part, err := read.Since(day)
		if err != nil {
			t.Fatalf("the part of %s: %v", day.Format(time.DateOnly), err)
		}
		want := ApplyRows{from: day, Runs: []DeliveryRun{}, Jobs: map[int64][]DeliveryRunJob{}}
		for _, run := range read.Runs {
			if !run.Created().Before(day) {
				want.Runs = append(want.Runs, run)
				if jobs, held := read.Jobs[run.RunID]; held {
					want.Jobs[run.RunID] = jobs
				}
			}
		}
		if !reflect.DeepEqual(part, want) {
			t.Errorf("the part of %s holds %d runs with the jobs of %d, want %d with the jobs of %d:\n%+v\nwant %+v",
				day.Format(time.DateOnly), len(part.Runs), len(part.Jobs), len(want.Runs), len(want.Jobs), part, want)
		}
		if n := len(part.Runs); n > 0 {
			parts++
			if &part.Runs[0] != &read.Runs[len(read.Runs)-n] {
				t.Errorf("the part of %s copies the %d runs created from it on, want the end of the read's runs, shared", day.Format(time.DateOnly), n)
			}
			if cap(part.Runs) != n {
				t.Errorf("the part of %s holds %d runs with room for %d, so an append to it would write over the read's", day.Format(time.DateOnly), n, cap(part.Runs))
			}
		}
	}
	if parts == 0 {
		t.Fatalf("no day's part holds a run, so the parts compare nothing")
	}
	t.Logf("%d days' parts of %d runs, each the end of the read's runs", parts, len(read.Runs))
}
