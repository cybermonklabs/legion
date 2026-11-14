package delivery

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestTheLatestShipBeforeReadIsBoundedByItsMoment: readLatestShipBefore's statement (and
// ListPullRequestsWaitingAt, which embeds it) reads delivery_runs through
// delivery_runs_kind_created with the run's creation bounded above by the moment it is asked of,
// under the generic plan pgx's cached statement settles on: a run whose job succeeded before that
// moment was created before it, so the read newest first starts at the moment rather than at the
// newest run, and does not probe the jobs of every run created since.
func TestTheLatestShipBeforeReadIsBoundedByItsMoment(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := explaintest.GenericPlan(t, ctx, connection, "latest_ship_before", latestShipBefore("$1", "$2", "$3", "$4"),
		[]string{"text", "text[]", "text", "timestamptz"}, []string{"'acme/deploy'", "array['production']", "null", "now()"},
		"set local enable_seqscan = off", "set local enable_bitmapscan = off")
	scans := explaintest.IndexScans(t, plan)
	bounded := slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool {
		return scan.Index == "delivery_runs_kind_created" && strings.Contains(scan.Cond, "<")
	})
	if !bounded {
		t.Fatalf("the latest ship read reads %v, want delivery_runs_kind_created bounded above by its moment:\n%s", scans, plan)
	}
	t.Logf("the latest ship read reads %v", scans)
}
