package api

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

// timelineJobsRun is one deploy run the timeline-jobs test adds to the deliverytest history: how
// many hours after the test's base moment, three days before the history's end, GitHub created it,
// its conclusion and its jobs (their times in minutes after that base), and the failed jobs and
// the root failing job the timeline must list it with.
type timelineJobsRun struct {
	id         int64
	hours      int
	conclusion delivery.DeliveryRunConclusion
	jobs       []delivery.DeliveryRunJob
	failed     []string
	root       string
}

// TestTheTimelineAnswersWhatEveryJobOfItsRunsAnswers: the timeline reads each deploy run's
// production jobs and the jobs of it that failed (delivery.ReadMergedRows with failedJobs), never
// every job the run holds, and answers what it answers from every job, byte for byte. Over a
// deliverytest history, beside which deploy runs are added whose failed jobs are two with a guard
// among them, the production job and another, guards alone, and one in a run that succeeded, the
// body deliveryTimelineCompute computes from the population the timeline reads is, for each
// window, the body it computes from that population with each deploy run's every job
// (delivery.ListRunJobs) in place of the jobs read, and GET /api/v1/delivery/timeline answers it.
// The jobs read are fewer than every job, so the read is not every job under another name.
func TestTheTimelineAnswersWhatEveryJobOfItsRunsAnswers(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	pool := database.Pool
	fixture := deliverytest.Seed(t, pool, deliverytest.Spec{})
	ctx := t.Context()
	repo := fixture.DeployRepo
	productionJob := fixture.Settings.ProductionJobNames[0]
	base := fixture.To.Add(-72 * time.Hour)
	at := func(minutes int) *time.Time { v := base.Add(time.Duration(minutes) * time.Minute); return &v }
	success, failure, skipped := delivery.DeliveryJobConclusionSuccess, delivery.DeliveryJobConclusionFailure, delivery.DeliveryJobConclusionSkipped
	added := []timelineJobsRun{
		// Two failed jobs, the guard finishing first, so the root is the other; the production
		// job's caller is skipped.
		{id: 8_000_000_001, hours: 0, conclusion: delivery.DeliveryRunConclusionFailure, jobs: []delivery.DeliveryRunJob{
			{Name: "build / image", StartedAt: at(1), CompletedAt: at(7), Conclusion: &failure},
			{Name: "build / deploy-verdict", StartedAt: at(1), CompletedAt: at(3), Conclusion: &failure},
			{Name: "lint", StartedAt: at(1), CompletedAt: at(2), Conclusion: &success},
			{Name: "production-release", Conclusion: &skipped},
		}, failed: []string{"build / deploy-verdict", "build / image"}, root: "build / image"},
		// The production job failed beside another job: a job both named and failed.
		{id: 8_000_000_002, hours: 2, conclusion: delivery.DeliveryRunConclusionFailure, jobs: []delivery.DeliveryRunJob{
			{Name: "build / image", StartedAt: at(121), CompletedAt: at(126), Conclusion: &success},
			{Name: "staging-e2e / run", StartedAt: at(127), CompletedAt: at(140), Conclusion: &failure},
			{Name: productionJob, StartedAt: at(127), CompletedAt: at(133), Conclusion: &failure},
		}, failed: []string{productionJob, "staging-e2e / run"}, root: productionJob},
		// A deploy whose only failed jobs are guards, so the root is the first of them to finish.
		{id: 8_000_000_003, hours: 4, conclusion: delivery.DeliveryRunConclusionFailure, jobs: []delivery.DeliveryRunJob{
			{Name: "build / image", StartedAt: at(241), CompletedAt: at(246), Conclusion: &success},
			{Name: productionJob, StartedAt: at(247), CompletedAt: at(253), Conclusion: &success},
			{Name: "page-oncall", StartedAt: at(254), CompletedAt: at(258), Conclusion: &failure},
			{Name: "release-verdict", StartedAt: at(254), CompletedAt: at(256), Conclusion: &failure},
		}, failed: []string{"page-oncall", "release-verdict"}, root: "release-verdict"},
		// A run that succeeded though a job of it failed.
		{id: 8_000_000_004, hours: 6, conclusion: delivery.DeliveryRunConclusionSuccess, jobs: []delivery.DeliveryRunJob{
			{Name: "lint", StartedAt: at(361), CompletedAt: at(362), Conclusion: &failure},
			{Name: "build / image", StartedAt: at(361), CompletedAt: at(366), Conclusion: &success},
			{Name: productionJob, StartedAt: at(367), CompletedAt: at(373), Conclusion: &success},
		}, failed: []string{"lint"}, root: "lint"},
	}
	for _, run := range added {
		created := base.Add(time.Duration(run.hours) * time.Hour)
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: repo, RunID: run.id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(run.id, 10),
			HeadCommitAt: created.Add(-time.Minute), StartedAt: created, CreatedAt: &created, CompletedAt: new(created.Add(time.Hour)),
			Conclusion: new(run.conclusion), URL: "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(run.id, 10),
			HeadBranch: new("main"), Event: new("push"),
		}); err != nil {
			t.Fatalf("add run %d: %v", run.id, err)
		}
		for i := range run.jobs {
			run.jobs[i].Repo, run.jobs[i].RunID = repo, run.id
		}
		if err := delivery.UpsertRunJobs(ctx, pool, repo, run.id, run.jobs, true); err != nil {
			t.Fatalf("add run %d's jobs: %v", run.id, err)
		}
	}

	s := directServer(deps)
	for _, days := range []int{84, 28, 7} {
		from, to := fixture.To.AddDate(0, 0, -days), fixture.To
		pop, err := s.readDeliveryPopulation(ctx, fixture.Settings, from, to, url.Values{}, true)
		if err != nil {
			t.Fatalf("%d days: read the timeline's population: %v", days, err)
		}
		every := pop
		every.jobsByRun = make(map[int64][]delivery.DeliveryRunJob, len(pop.applyRuns))
		read, held := 0, 0
		for _, run := range pop.applyRuns {
			jobs, err := delivery.ListRunJobs(ctx, pool, run.Repo, run.RunID)
			if err != nil {
				t.Fatalf("%d days: ListRunJobs %d: %v", days, run.RunID, err)
			}
			every.jobsByRun[run.RunID] = jobs
			read, held = read+len(pop.jobsByRun[run.RunID]), held+len(jobs)
		}
		if read >= held {
			t.Errorf("%d days: the timeline read %d jobs of its %d deploy runs, which hold %d: want the production and the failed ones alone, fewer than every job", days, read, len(pop.applyRuns), held)
		}

		got := deliveryTimelineCompute(fixture.Settings, pop, from, to, to)
		want := deliveryTimelineCompute(fixture.Settings, every, from, to, to)
		gotBody, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		wantBody, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(gotBody, wantBody) {
			for i := range max(len(got.Runs), len(want.Runs)) {
				if read, all := runJSON(t, got.Runs, i), runJSON(t, want.Runs, i); !bytes.Equal(read, all) {
					t.Fatalf("%d days: the timeline from the jobs read differs from the timeline from every job, first at runs[%d]:\n read:  %s\n every: %s", days, i, read, all)
				}
			}
			t.Fatalf("%d days: the timeline from the jobs read differs from the timeline from every job outside runs[]", days)
		}
		window := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
		assertSameTree(t, strconv.Itoa(days)+" days", asJSONTree(t, getRaw(t, handler, "/api/v1/delivery/timeline?"+window)), asJSONTree(t, gotBody))

		// Every added run is in the window and listed as its jobs say, so the comparison above
		// held each shape of failure.
		for _, run := range added {
			index := slices.IndexFunc(want.Runs, func(view delivery.DeliveryRunView) bool { return view.ID == run.id })
			if index < 0 {
				t.Fatalf("%d days: run %d is not on the timeline", days, run.id)
			}
			view := want.Runs[index]
			var failed []string
			for _, job := range view.FailedJobs {
				failed = append(failed, job.Name)
			}
			if !slices.Equal(failed, run.failed) || view.RootFailingJob == nil || view.RootFailingJob.Name != run.root {
				t.Errorf("%d days: run %d lists failed jobs %q and root %+v, want %q and %s", days, run.id, failed, view.RootFailingJob, run.failed, run.root)
			}
		}
	}
}

// runJSON is the JSON of runs[i], or "(none)" past the list's end.
func runJSON(t *testing.T, runs []delivery.DeliveryRunView, i int) []byte {
	t.Helper()
	if i >= len(runs) {
		return []byte("(none)")
	}
	data, err := json.Marshal(runs[i])
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}
