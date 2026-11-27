package delivery

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestTheLatestShipBeforeReadIsBoundedByItsMoment is an eligibility test, and proves no more than
// that. It plans ReadLatestShipBefore's statement, latestShipBefore, under the generic plan pgx's
// cached statement settles on, over empty tables, with sequential and bitmap scans switched off
// (set local enable_seqscan = off, enable_bitmapscan = off), so the plan can only read
// delivery_runs through an index. It shows that the statement gives delivery_runs_kind_created a
// condition bounding the run's creation above by the moment it is asked of: a run whose job
// succeeded before that moment was created before it, so the read newest first can start at the
// moment rather than at the newest run, and does not probe the jobs of every run created since. It
// does not show that the planner takes that index when it may take a scan, which
// TestTheLatestShipBeforeReadStartsAtItsMomentWhateverThePlan does over a year of runs. The same
// statement is embedded in ListPullRequestsWaitingAt's bound (pullRequestsWaitingAtQuery); that
// statement's plan is pinned by no test, and its answers are held to the Go rule by
// TestTheWaitingSQLAgreesWithTheShippingRule and the tests beside it.
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

// TestTheLatestShipBeforeReadStartsAtItsMomentWhateverThePlan plans latestShipBefore as the
// planner plans it for any reader, with nothing switched off, under each plan-cache mode (the plan
// made for the moment's own value and the generic plan a cached statement settles on), over a year
// of 4,000 deploy runs with their jobs, analyzed. Neither plan reads delivery_runs or its jobs by a
// sequential scan, and each reads delivery_runs through delivery_runs_kind_created with the run's
// creation bounded above by the moment, so the read newest first starts there. The moment is now,
// a month after the newest run, so a plan that took every run created before it would read them
// all: that is what the bound's index condition, and not the table's size, keeps it from.
func TestTheLatestShipBeforeReadStartsAtItsMomentWhateverThePlan(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAYearOfDeployRuns(t, ctx, pool)
	statement := explaintest.Statement{
		SQL:  latestShipBefore("$1", "$2", "$3", "$4"),
		Args: []any{"acme/deploy", []string{"production"}, nil, time.Now().UTC()},
	}
	for _, mode := range explaintest.PlanCacheModes {
		plan := json.RawMessage(explaintest.ExplainBuffered(t, ctx, pool, statement, mode, "json"))
		for _, relation := range []string{"delivery_runs", "delivery_run_jobs"} {
			if explaintest.SeqScansRelation(t, plan, relation) {
				t.Fatalf("under %s the latest ship read scans %s whole:\n%s", mode, relation, explaintest.Outline(t, plan))
			}
		}
		scans := explaintest.IndexScans(t, plan)
		bounded := slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool {
			return scan.Index == "delivery_runs_kind_created" && strings.Contains(scan.Cond, "<")
		})
		if !bounded {
			t.Fatalf("under %s the latest ship read reads %v, want delivery_runs_kind_created bounded above by its moment:\n%s", mode, scans, explaintest.Outline(t, plan))
		}
		t.Logf("under %s the latest ship read reads %v", mode, scans)
	}
}
