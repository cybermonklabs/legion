package evals

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

// TestADaysPartOfAWalksDeployRunsIsTheDaysOwnRead: the deploy runs a walk's batch reads once from
// its first day on (delivery.ReadApplyRows), cut to each later day of the batch
// (delivery.MergedRowsWith), are the rows that day's own read reads (delivery.ReadMergedRows):
// the same pull requests, the same runs in the same order and the same jobs, over every day of
// deliverytest's history.
func TestADaysPartOfAWalksDeployRunsIsTheDaysOwnRead(t *testing.T) {
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
		end := day.AddDate(0, 0, 1)
		own, err := delivery.ReadMergedRows(ctx, pool, settings, day, end, false)
		if err != nil {
			t.Fatalf("read the rows of %s: %v", day, err)
		}
		cut, err := delivery.MergedRowsWith(ctx, pool, applies, day, end)
		if err != nil {
			t.Fatalf("cut the rows of %s: %v", day, err)
		}
		if !reflect.DeepEqual(own, cut) {
			t.Errorf("%s: the walk's cut holds %d pull requests and %d runs, the day's own read %d and %d, and they differ",
				day.Format("2006-01-02"), len(cut.PullRequests), len(cut.ApplyRuns), len(own.PullRequests), len(own.ApplyRuns))
		}
		runs += len(own.ApplyRuns)
	}
	if runs == 0 {
		t.Fatalf("no day of the history reads a deploy run, so the comparison compares nothing")
	}
	t.Logf("%.0f days, %d day-reads of deploy runs, each equal to its cut of the one read from %s", fixture.To.Sub(fixture.From).Hours()/24, runs, fixture.From.Format("2006-01-02"))
}
