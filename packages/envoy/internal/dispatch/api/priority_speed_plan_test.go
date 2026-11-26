package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// priority_speed_plan_test.go is how priority_speed_test.go reads what one page read sends and
// what its statements cost (speedTraced): the read runs once through the scan the listing route
// uses (runSpeedPage) on a context the corpus's traced pool counts (countSpeedTraced), each
// statement it sent is explained as it was sent (speedPlan), and the rows of the relation read's
// inheritance walk are read off those plans (speedWalkRows). TestAdvancesListingSpeed holds the
// page with the relation to the statements and round trips of the page without it, and the walk
// to the page's own chains. It is a file of its own because it explains each statement under the
// plan made for the arguments it was sent with, where priority_speed_test.go counts each
// statement's work under both plans a connection runs (countSpeedStatement).

// speedTraced is what one read sent and what its statements cost: the statements and the round
// trips they went in, read off the traced pool (speedTrace), and the shared buffers EXPLAIN
// (ANALYZE, BUFFERS) counts for those statements as they were sent, beside the plan of each
// statement that count was read from (speedPlan).
type speedTraced struct {
	name       string
	statements int
	roundTrips int
	buffers    uint64
	plans      []speedPlanNode
}

func (w speedTraced) String() string {
	return fmt.Sprintf("%s: %d statements in %d round trips, %d shared buffers", w.name, w.statements, w.roundTrips, w.buffers)
}

// countSpeedTraced runs one read on a context speedTrace counts (countedRead), and answers what
// it sent and the shared buffers its statements cost on connection, a connection of the untraced
// pool that every count of the run shares.
func countSpeedTraced(t *testing.T, connection *pgx.Conn, trace *speedTrace, name string, read func(context.Context)) speedTraced {
	t.Helper()
	trace.take()
	read(context.WithValue(context.Background(), countedRead{}, true))
	statements, roundTrips := trace.take()
	work := speedTraced{name: name, statements: len(statements), roundTrips: roundTrips}
	for _, statement := range statements {
		plan := speedPlan(t, connection, statement)
		work.buffers += plan.SharedHit + plan.SharedRead
		work.plans = append(work.plans, plan)
	}
	return work
}

// speedPlanNode is what the counts read of one node of an EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
// plan: the common table expression it computes, if any, the rows it answered over all its
// loops, and the shared buffers it and every node below it asked for.
type speedPlanNode struct {
	SubplanName string          `json:"Subplan Name"`
	ActualRows  float64         `json:"Actual Rows"`
	ActualLoops float64         `json:"Actual Loops"`
	SharedHit   uint64          `json:"Shared Hit Blocks"`
	SharedRead  uint64          `json:"Shared Read Blocks"`
	Plans       []speedPlanNode `json:"Plans"`
}

// speedPlan is the top node of statement's plan under EXPLAIN (ANALYZE, BUFFERS) as the read
// sent it, its text and its arguments. Its shared buffers, hit plus read, are every page the
// statement asked for, whether the cache held it or not. It is run twice on the one connection
// and the second run is the one answered: the first fills the connection's catalog caches, and a
// miss there during execution would count pages of the catalog rather than of the read. Each run
// plans afresh with the read's own arguments (pgx.QueryExecModeDescribeExec), so neither is the
// generic plan a prepared statement turns to from its sixth execution, and every reading of a
// statement is of the one plan.
func speedPlan(t *testing.T, connection *pgx.Conn, statement speedStatement) speedPlanNode {
	t.Helper()
	arguments := append([]any{pgx.QueryExecModeDescribeExec}, statement.arguments...)
	var plan speedPlanNode
	for range 2 {
		var explained []struct {
			Plan speedPlanNode `json:"Plan"`
		}
		if err := connection.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement.sql, arguments...).Scan(&explained); err != nil {
			t.Fatalf("explain %.200s: %v", statement.sql, err)
		}
		if len(explained) != 1 {
			t.Fatalf("explain %.200s gave %d plans, want one", statement.sql, len(explained))
		}
		plan = explained[0].Plan
	}
	return plan
}

// speedWalkRows is the rows the relation read's inheritance walk answered in plans, the plans of
// a page read: the common table expression issueAdvancesTerms names advances_chain, one row for
// each subject and each issue above it. A recursive one is never inlined, so a plan of a text that
// reads the relation holds it exactly once.
func speedWalkRows(t *testing.T, plans []speedPlanNode) int {
	t.Helper()
	var walks []speedPlanNode
	var visit func(node speedPlanNode)
	visit = func(node speedPlanNode) {
		if node.SubplanName == "CTE advances_chain" {
			walks = append(walks, node)
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, plan := range plans {
		visit(plan)
	}
	if len(walks) != 1 {
		t.Fatalf("the page's plans hold %d advances_chain walks, want one", len(walks))
	}
	return int(walks[0].ActualRows * walks[0].ActualLoops)
}

// runSpeedPage reads one page on ctx through the scan the listing route uses, which is everything
// of the read but the JSON encoding of its answer.
func runSpeedPage(t *testing.T, ctx context.Context, corpus speedCorpusShape, query string, arguments []any) {
	t.Helper()
	if _, _, err := corpus.server.scanIssuePage(ctx, query, arguments); err != nil {
		t.Fatalf("read a page of the listing: %v", err)
	}
}
