package evals

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestADaysPartOfAWalksDeployRunsIsTheDaysOwnRead: the deploy runs a walk's batch reads once from
// its first day on (delivery.ReadApplyRows), cut to each later day of the batch
// (delivery.ApplyRows.Since), are the rows that day's own read reads (delivery.ReadApplyRows from
// that day): the same runs in the same order and the same jobs, over every day of deliverytest's
// history.
func TestADaysPartOfAWalksDeployRunsIsTheDaysOwnRead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	fixture := deliverytest.Seed(t, pool, deliverytest.Spec{Days: 5})
	settings, err := delivery.GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	applies, err := delivery.ReadApplyRows(ctx, pool, settings, fixture.From)
	if err != nil {
		t.Fatalf("read the deploy runs from %s on: %v", fixture.From, err)
	}
	runs := 0
	for day := fixture.From; day.Before(fixture.To); day = day.AddDate(0, 0, 1) {
		own, err := delivery.ReadApplyRows(ctx, pool, settings, day)
		if err != nil {
			t.Fatalf("read the runs of %s: %v", day, err)
		}
		cut, err := applies.Since(day)
		if err != nil {
			t.Fatalf("cut the runs of %s: %v", day, err)
		}
		if !reflect.DeepEqual(own, cut) {
			t.Errorf("%s: the walk's cut holds %d runs, the day's own read %d, and they differ",
				day.Format("2006-01-02"), len(cut.Runs), len(own.Runs))
		}
		runs += len(own.Runs)
	}
	if runs == 0 {
		t.Fatalf("no day of the history reads a deploy run, so the comparison compares nothing")
	}
	t.Logf("%.0f days, %d day-reads of deploy runs, each equal to its cut of the one read from %s", fixture.To.Sub(fixture.From).Hours()/24, runs, fixture.From.Format("2006-01-02"))
}

// TestAReadOfSeveralDaysAnswersEachDaysOwnReads: over every day of deliverytest's history read at
// once, and every other day read at once, each day's part of the reads of several days
// (delivery.ListPullRequestsOnDays, ReadDaysDeploys and ReadCIDays) is what that day's own reads
// answer (ListPullRequestsInWindow, ReadDeploys and ReadCIRows), and each day's delivery rows as a
// page reads them (readDeliveryDays, the deploy runs of the days' windows alone, bounded as the
// routes bound a window's) are the rows the day builds of every deploy run created from its start
// on (delivery.ReadApplyRows, which the walk reads once a batch).
func TestAReadOfSeveralDaysAnswersEachDaysOwnReads(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	fixture := deliverytest.Seed(t, pool, deliverytest.Spec{Days: 8})
	settings, err := delivery.GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	repos, err := readDeployRepos(ctx, pool)
	if err != nil {
		t.Fatalf("read the repositories that hold a delivery run: %v", err)
	}
	cards := measures.DeployCardJobNames(measures.DeployCards(settings))
	var every, others []time.Time
	for day := fixture.From; day.Before(fixture.To); day = day.AddDate(0, 0, 1) {
		if len(every)%2 == 0 {
			others = append(others, day)
		}
		every = append(every, day)
	}
	prsRead, deploysRead, earlierRead, checksRead, shipped := 0, 0, 0, 0, 0
	for _, days := range [][]time.Time{every, others} {
		prs, err := delivery.ListPullRequestsOnDays(ctx, pool, days)
		if err != nil {
			t.Fatalf("read the pull requests of %d days: %v", len(days), err)
		}
		deploys, err := delivery.ReadDaysDeploys(ctx, pool, settings, days, true)
		if err != nil {
			t.Fatalf("read the deploys of %d days: %v", len(days), err)
		}
		ci, err := delivery.ReadCIDays(ctx, pool, settings.DeployRepo, days, cards)
		if err != nil {
			t.Fatalf("read the CI runs of %d days: %v", len(days), err)
		}
		reads, err := r.registry.readDeliveryDays(ctx, settings, repos, days, nil)
		if err != nil {
			t.Fatalf("read the delivery rows of %d days: %v", len(days), err)
		}
		for i, day := range days {
			what, end := day.Format(time.DateOnly), day.AddDate(0, 0, 1)
			ownPRs, err := delivery.ListPullRequestsInWindow(ctx, pool, day, end)
			if err != nil {
				t.Fatalf("%s: read the pull requests: %v", what, err)
			}
			if !reflect.DeepEqual(prs[i], ownPRs) {
				t.Errorf("%s: the read of %d days holds %d pull requests of the day, the day's own read %d, and they differ", what, len(days), len(prs[i]), len(ownPRs))
			}
			ownDeploys, err := delivery.ReadDeploys(ctx, pool, settings, day, end)
			if err != nil {
				t.Fatalf("%s: read the deploys: %v", what, err)
			}
			if !reflect.DeepEqual(deploys.Deploys[i], ownDeploys) {
				t.Errorf("%s: the read of %d days holds %d deploy runs of the day and %d earlier pull requests, the day's own read %d and %d, and they differ",
					what, len(days), len(deploys.Deploys[i].Started.Runs), len(deploys.Deploys[i].Earlier.PullRequests), len(ownDeploys.Started.Runs),
					len(ownDeploys.Earlier.PullRequests))
			}
			ownCI, err := delivery.ReadCIRows(ctx, pool, settings.DeployRepo, day, end, cards)
			if err != nil {
				t.Fatalf("%s: read the CI runs: %v", what, err)
			}
			if !reflect.DeepEqual(ci[i], ownCI) {
				t.Errorf("%s: the read of %d days holds %d PR-checks and %d push runs of the day, the day's own read %d and %d, and they differ",
					what, len(days), len(ci[i].Checks.Runs), len(ci[i].Deploys.Runs), len(ownCI.Checks.Runs), len(ownCI.Deploys.Runs))
			}
			walked := map[string]*delivery.ApplyRows{}
			for _, repo := range repos {
				one := settings
				one.DeployRepo = repo.name
				applies, err := delivery.ReadApplyRows(ctx, pool, one, day)
				if err != nil {
					t.Fatalf("%s: read the deploy runs of %s from the day on: %v", what, repo.name, err)
				}
				walked[repo.name] = &applies
			}
			own, err := r.registry.readDeliveryDays(ctx, settings, repos, []time.Time{day}, walked)
			if err != nil {
				t.Fatalf("%s: read the delivery rows with every deploy run from the day on: %v", what, err)
			}
			if !reflect.DeepEqual(reads[i], own[0]) {
				t.Errorf("%s: the delivery rows the read of %d days builds of the day differ from those it builds of every deploy run created from the day on",
					what, len(days))
			}
			prsRead, deploysRead, earlierRead, checksRead = prsRead+len(ownPRs), deploysRead+len(ownDeploys.Started.Runs),
				earlierRead+len(ownDeploys.Earlier.PullRequests), checksRead+len(ownCI.Checks.Runs)
			for _, prs := range ownDeploys.ShippedBy(delivery.MergedRows{PullRequests: ownPRs, ApplyRuns: walked[settings.DeployRepo].Runs,
				ApplyJobs: walked[settings.DeployRepo].Jobs}, settings) {
				shipped += len(prs)
			}
		}
	}
	if deploysRead == 0 || earlierRead == 0 || checksRead == 0 || shipped == 0 {
		t.Fatalf("the days read %d deploy runs, %d earlier pull requests, %d PR-checks runs and %d shipped pull requests, and each must be some for the comparison to compare them",
			deploysRead, earlierRead, checksRead, shipped)
	}
	t.Logf("%d days read at once and %d every other: %d pull requests, %d deploy runs shipping %d, %d earlier pull requests and %d PR-checks runs, each day's as its own reads read it",
		len(every), len(others), prsRead, deploysRead, shipped, earlierRead, checksRead)
}
