package api

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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
