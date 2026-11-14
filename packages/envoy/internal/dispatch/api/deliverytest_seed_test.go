package api

import (
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestTheDeliveryHistoryAnswersTheDeliveryRoutes: a deliverytest history, read back through
// GET /api/v1/delivery/measures and GET /api/v1/delivery/pipeline over the last 28 days of it,
// answers the deploys, the pull requests and the lead times the fixture says it wrote, counted by
// the base's own containment over the fixture's own rows, with some deploys shipping pull requests
// and some none; PR-check cells of every outcome and ranked slowest jobs with their timed results;
// the production card's three kinds of result and every other card's timed results; and each
// repository's own pull requests under its facet. The fixture is held to holding pull requests
// still waiting at the end; its failing-jobs list is logged, not checked. The test lives in api
// rather than beside deliverytest because the routes' test handler is this package's.
func TestTheDeliveryHistoryAnswersTheDeliveryRoutes(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{})
	to := fixture.To
	from := to.AddDate(0, 0, -28)
	window := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}

	// What the fixture wrote in the window, judged by the rules the routes judge stored rows by.
	var windowDeployRuns []delivery.DeliveryRun
	for _, run := range fixture.Runs {
		if run.Kind == delivery.DeliveryRunKindDeploy && !run.StartedAt.Before(from) && run.StartedAt.Before(to) {
			windowDeployRuns = append(windowDeployRuns, run)
		}
	}
	deploys := len(delivery.ProductionApplies(windowDeployRuns, fixture.Jobs, fixture.Settings.Production()).Applies)
	merged, shipped := 0, 0
	for _, pr := range fixture.PullRequests {
		if pr.MergedAt.Before(from) || !pr.MergedAt.Before(to) {
			continue
		}
		merged++
		if delivery.ContainingRun(pr, fixture.DeployRepo, fixture.Applies) != nil {
			shipped++
		}
	}
	if deploys == 0 || merged == 0 || shipped == 0 {
		t.Fatalf("the fixture wrote %d deploys, %d merged and %d shipped pull requests in %s..%s, which proves nothing",
			deploys, merged, shipped, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if fixture.Waiting == 0 || fixture.ShippedNone == 0 || fixture.Deployed <= fixture.Waiting {
		t.Errorf("the fixture holds %d deployed and %d waiting pull requests and %d deploys that shipped none, want most deployed, some waiting and some deploys shipping none",
			fixture.Deployed, fixture.Waiting, fixture.ShippedNone)
	}

	got := getDeliveryMeasures(t, handler, window.Encode()).Measures
	if got.DeployFrequency.SuccessfulDeploys != deploys {
		t.Errorf("successful_deploys = %d, want the %d the fixture wrote", got.DeployFrequency.SuccessfulDeploys, deploys)
	}
	if withPRs := got.DeployFrequency.DeploysWithPRs; withPRs == 0 || withPRs >= deploys {
		t.Errorf("deploys_with_prs = %d of %d deploys, want some deploys shipping pull requests and some shipping none", withPRs, deploys)
	}
	if got.LeadTime.MergeToProduction.MedianMinutes == nil || got.LeadTime.OpenedToMerge.MedianMinutes == nil {
		t.Errorf("lead_time = %+v, want a merge-to-production and an open-to-merge spread", got.LeadTime)
	}

	_, pipeline := getDeliveryPipeline(t, handler, window.Encode())
	if n := pipeline.OpenedToMerge.Total.N; n != merged {
		t.Errorf("opened_to_merge.total.n = %d, want the %d pull requests the fixture merged in the window", n, merged)
	}
	if n := pipeline.MergeToProduction.Total.N; n != shipped {
		t.Errorf("merge_to_production.total.n = %d, want the %d pull requests a deploy shipped", n, shipped)
	}
	checks := pipeline.PRChecks
	if checks.Run.Total.N == 0 || checks.Run.Total.Failure == 0 || checks.Run.Total.Cancelled == 0 {
		t.Errorf("pr_checks.run.total = %+v, want timed, failed and cancelled runs", checks.Run.Total)
	}
	if len(checks.SlowestJobs) == 0 {
		t.Errorf("pr_checks.slowest_jobs is empty, want the jobs with %d or more timed results", measures.SlowestMinResults)
	}
	for _, job := range checks.SlowestJobs {
		if job.Total.N < measures.SlowestMinResults || job.Total.MedianMinutes == nil {
			t.Errorf("slowest job %s holds %d timed results and median %v, want %d or more and a median", job.Name, job.Total.N, job.Total.MedianMinutes, measures.SlowestMinResults)
		}
	}
	if len(checks.FailingJobs) == 0 {
		t.Error("pr_checks.failing_jobs is empty, want the checks that failed over the window's last day")
	}
	if card := pipeline.ProductionApply.Total; card.Success == 0 || card.Failure == 0 || card.Cancelled == 0 {
		t.Errorf("production_apply.total = %+v, want successes, failures and cancellations", card)
	}
	for name, card := range map[string]*measures.DeployJobTiming{"staging_e2e": pipeline.StagingE2E, "staging_apply": pipeline.StagingApply} {
		if card == nil || card.Total.N == 0 {
			t.Errorf("%s = %+v, want a card with timed results", name, card)
		}
	}

	for _, repo := range fixture.Repos {
		faceted := url.Values{"from": window["from"], "to": window["to"], "repo": {repo}}
		lead := getDeliveryMeasures(t, handler, faceted.Encode()).Measures.LeadTime
		if lead.OpenedToMerge.MedianMinutes == nil {
			t.Errorf("repo=%s: opened_to_merge = %+v, want that repository's pull requests", repo, lead.OpenedToMerge)
		}
		if tracked := repo == fixture.DeployRepo; tracked != (lead.MergeToProduction.MedianMinutes != nil) {
			t.Errorf("repo=%s: merge_to_production = %+v, want a spread only for the deploy repository", repo, lead.MergeToProduction)
		}
	}

	t.Logf("over %s..%s: %d deploys (%d with pull requests), %d pull requests merged and %d shipped, %d PR-check runs timed, %d slowest jobs, %d failing jobs",
		from.Format(time.DateOnly), to.Format(time.DateOnly), got.DeployFrequency.SuccessfulDeploys, got.DeployFrequency.DeploysWithPRs,
		merged, shipped, checks.Run.Total.N, len(checks.SlowestJobs), len(checks.FailingJobs))
}
