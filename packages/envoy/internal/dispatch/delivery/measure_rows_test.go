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
