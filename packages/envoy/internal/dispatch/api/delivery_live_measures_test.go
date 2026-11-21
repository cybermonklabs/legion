package api

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestAMeasuresViewIsMarkedByAMergeBeforeItsWindow: a measures view's deploys_with_prs counts each
// deploy of its window that ships a pull request, whenever that pull request merged
// (delivery.Deploys.ShippedBy), so a pull request change dated before the window, which a late
// store of a pull request the window's deploy ships makes, marks the view, as one dated inside it
// does; one dated at or after the window's end, which no deploy of the window can ship, does not.
func TestAMeasuresViewIsMarkedByAMergeBeforeItsWindow(t *testing.T) {
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	view := &deliveryView{spec: deliveryLiveSpec{kind: deliveryLiveMeasures, from: from, to: to}}
	now := to.Add(48 * time.Hour)
	for _, merged := range []struct {
		what string
		at   time.Time
		want bool
	}{
		{"a week before the window", from.AddDate(0, 0, -7), true},
		{"inside the window", from.Add(36 * time.Hour), true},
		{"at the window's end", to, false},
	} {
		change := delivery.Change{Seq: 1, Kind: "pull_request", Repo: "acme/widgets", Ref: "1", TNew: &merged.at}
		if got := view.touchedBy(change, now); got != merged.want {
			t.Errorf("a pull request merged %s marks the measures view of %s..%s: %t, want %t", merged.what,
				from.Format(time.DateOnly), to.Format(time.DateOnly), got, merged.want)
		}
	}
}

// TestAMeasuresViewIsMarkedByEveryDeployRunChange: a measures view's deploys_with_prs counts a
// deploy of its window by the pull requests it ships first, which the deploy runs created before
// the window decide as well (pop.windowShipped reads the runs created from the window's start, and
// delivery.Deploys.ShippedBy the rows before it): a run created before the window whose production
// job lands late can ship a pull request a deploy of the window shipped until then. So a change of
// a deploy run or of one of its jobs marks every measures view, whenever the run was created, as it
// marks every timeline view. A pipeline view, which reads the deploy runs started in its shown days
// alone, is not marked by a run created before them. A pipeline view names no window, only an at,
// and shows the seven UTC days ending on at's day; the one here is at the window's last second, so
// its seven days are the window's and the run created inside the window falls in them.
func TestAMeasuresViewIsMarkedByEveryDeployRunChange(t *testing.T) {
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	now := to.Add(48 * time.Hour)
	measuresView := &deliveryView{spec: deliveryLiveSpec{kind: deliveryLiveMeasures, from: from, to: to}}
	at := to.Add(-time.Second)
	pipelineView := &deliveryView{spec: deliveryLiveSpec{kind: deliveryLivePipeline, at: &at}}
	if shown := measures.WindowStart(at); !shown.Equal(from) {
		t.Fatalf("the pipeline view at %s shows the days from %s, want the window's from %s", at.Format(time.RFC3339),
			shown.Format(time.DateOnly), from.Format(time.DateOnly))
	}
	marked := 0
	for _, kind := range []string{delivery.ChangeKindRun, delivery.ChangeKindRunJob} {
		for _, created := range []struct {
			what string
			at   time.Time
		}{
			{"a week before the window", from.AddDate(0, 0, -7)},
			{"an hour before the window", from.Add(-time.Hour)},
			{"inside the window", from.Add(36 * time.Hour)},
		} {
			change := delivery.Change{Seq: 1, Kind: kind, RunKind: string(delivery.DeliveryRunKindDeploy), Repo: "acme/widgets", Ref: "7", TOld: &created.at, TNew: &created.at}
			if !measuresView.touchedBy(change, now) {
				t.Errorf("a %s change of a deploy run created %s does not mark the measures view of %s..%s", kind, created.what,
					from.Format(time.DateOnly), to.Format(time.DateOnly))
			} else {
				marked++
			}
			if inside := !created.at.Before(from); pipelineView.touchedBy(change, now) != inside {
				t.Errorf("a %s change of a deploy run created %s marks the pipeline view at %s: %t, want %t", kind, created.what,
					at.Format(time.RFC3339), !inside, inside)
			}
		}
	}
	t.Logf("%d deploy-run changes marked the measures view", marked)
}
