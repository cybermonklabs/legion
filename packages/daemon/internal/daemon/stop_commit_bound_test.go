package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// A commit whose caller's context the stop or the drain's end cancels waits at most
// store.CommitSettleBound more for its outcome, counted from that cancel, and the stop budget's
// last fifth, after the drain's end, is what leaves room for it before the budget runs out.
func TestTheCommitSettleFitsTheStopsLastFifth(t *testing.T) {
	if last := stopBudget - drainOf(stopBudget); store.CommitSettleBound > last {
		t.Fatalf("store.CommitSettleBound = %s, longer than the stop budget's last fifth, %s: a commit the drain's end cancels would settle after the budget ran out",
			store.CommitSettleBound, last)
	}
}
