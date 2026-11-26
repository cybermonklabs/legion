package explaintest

import (
	"encoding/json"
	"slices"
	"testing"
)

// filteredJoinPlan is PostgreSQL 18.6's `EXPLAIN (ANALYZE, FORMAT JSON)` of a left join that
// prints both counts this package reads, captured with jit off in a scratch database on the
// server LEGION_TEST_PG_DSN names, which held only the two tables these statements made and was
// dropped after:
//
//	create table owners (id int primary key, name text not null)
//	create table pets (id int primary key, owner_id int not null references owners, legs int not null)
//	insert into owners select g, 'owner ' || g from generate_series(1, 8) g
//	insert into pets select g, g % 8 + 1, g % 5 from generate_series(1, 40) g
//	analyze
//
// by this command, FIXTURE_DSN being LEGION_TEST_PG_DSN with its database replaced by that
// scratch database:
//
//	psql "$FIXTURE_DSN" -AtqX -c "set enable_seqscan = off" -c "explain (analyze, format json) select o.name, p.id from owners o left join pets p on p.legs > o.id % 4 and p.id <= 30 where o.id >= 2 and o.name <> 'owner 3' and coalesce(p.id, 0) + o.id > 10"
//
// The join tests the pair condition as its Join Filter and the where clause's condition, which
// reads the side the join may leave null, as its Filter after the join; the scan of owners
// filters by name; the Materialize that runs once for each of the six owners, and the scan of
// pets under it, carry no filter and print neither count.
const filteredJoinPlan = `[
  {
    "Plan": {
      "Node Type": "Nested Loop",
      "Parallel Aware": false,
      "Async Capable": false,
      "Join Type": "Left",
      "Startup Cost": 0.27,
      "Total Cost": 29.76,
      "Plan Rows": 23,
      "Plan Width": 12,
      "Actual Startup Time": 0.241,
      "Actual Total Time": 0.330,
      "Actual Rows": 81.00,
      "Actual Loops": 1,
      "Disabled": false,
      "Inner Unique": false,
      "Join Filter": "(p.legs > (o.id % 4))",
      "Rows Removed by Join Filter": 84,
      "Filter": "((COALESCE(p.id, 0) + o.id) > 10)",
      "Rows Removed by Filter": 15,
      "Shared Hit Blocks": 4,
      "Shared Read Blocks": 0,
      "Shared Dirtied Blocks": 0,
      "Shared Written Blocks": 0,
      "Local Hit Blocks": 0,
      "Local Read Blocks": 0,
      "Local Dirtied Blocks": 0,
      "Local Written Blocks": 0,
      "Temp Read Blocks": 0,
      "Temp Written Blocks": 0,
      "Plans": [
        {
          "Node Type": "Index Scan",
          "Parent Relationship": "Outer",
          "Parallel Aware": false,
          "Async Capable": false,
          "Scan Direction": "Forward",
          "Index Name": "owners_pkey",
          "Relation Name": "owners",
          "Alias": "o",
          "Startup Cost": 0.13,
          "Total Cost": 12.29,
          "Plan Rows": 7,
          "Plan Width": 12,
          "Actual Startup Time": 0.026,
          "Actual Total Time": 0.032,
          "Actual Rows": 6.00,
          "Actual Loops": 1,
          "Disabled": false,
          "Index Cond": "(id >= 2)",
          "Rows Removed by Index Recheck": 0,
          "Filter": "(name <> 'owner 3'::text)",
          "Rows Removed by Filter": 1,
          "Index Searches": 1,
          "Shared Hit Blocks": 2,
          "Shared Read Blocks": 0,
          "Shared Dirtied Blocks": 0,
          "Shared Written Blocks": 0,
          "Local Hit Blocks": 0,
          "Local Read Blocks": 0,
          "Local Dirtied Blocks": 0,
          "Local Written Blocks": 0,
          "Temp Read Blocks": 0,
          "Temp Written Blocks": 0
        },
        {
          "Node Type": "Materialize",
          "Parent Relationship": "Inner",
          "Parallel Aware": false,
          "Async Capable": false,
          "Startup Cost": 0.14,
          "Total Cost": 12.82,
          "Plan Rows": 30,
          "Plan Width": 8,
          "Actual Startup Time": 0.033,
          "Actual Total Time": 0.041,
          "Actual Rows": 30.00,
          "Actual Loops": 6,
          "Disabled": false,
          "Storage": "Memory",
          "Maximum Storage": 17,
          "Shared Hit Blocks": 2,
          "Shared Read Blocks": 0,
          "Shared Dirtied Blocks": 0,
          "Shared Written Blocks": 0,
          "Local Hit Blocks": 0,
          "Local Read Blocks": 0,
          "Local Dirtied Blocks": 0,
          "Local Written Blocks": 0,
          "Temp Read Blocks": 0,
          "Temp Written Blocks": 0,
          "Plans": [
            {
              "Node Type": "Index Scan",
              "Parent Relationship": "Outer",
              "Parallel Aware": false,
              "Async Capable": false,
              "Scan Direction": "Forward",
              "Index Name": "pets_pkey",
              "Relation Name": "pets",
              "Alias": "p",
              "Startup Cost": 0.14,
              "Total Cost": 12.67,
              "Plan Rows": 30,
              "Plan Width": 8,
              "Actual Startup Time": 0.132,
              "Actual Total Time": 0.145,
              "Actual Rows": 30.00,
              "Actual Loops": 1,
              "Disabled": false,
              "Index Cond": "(id <= 30)",
              "Rows Removed by Index Recheck": 0,
              "Index Searches": 1,
              "Shared Hit Blocks": 2,
              "Shared Read Blocks": 0,
              "Shared Dirtied Blocks": 0,
              "Shared Written Blocks": 0,
              "Local Hit Blocks": 0,
              "Local Read Blocks": 0,
              "Local Dirtied Blocks": 0,
              "Local Written Blocks": 0,
              "Temp Read Blocks": 0,
              "Temp Written Blocks": 0
            }
          ]
        }
      ]
    },
    "Planning": {
      "Shared Hit Blocks": 186,
      "Shared Read Blocks": 0,
      "Shared Dirtied Blocks": 0,
      "Shared Written Blocks": 0,
      "Local Hit Blocks": 0,
      "Local Read Blocks": 0,
      "Local Dirtied Blocks": 0,
      "Local Written Blocks": 0,
      "Temp Read Blocks": 0,
      "Temp Written Blocks": 0
    },
    "Planning Time": 2.012,
    "Triggers": [
    ],
    "Execution Time": 0.464
  }
]`

// Nodes reads each node's "Rows Removed by Filter" and "Rows Removed by Join Filter" as the plan
// prints them, and 0 for a node that prints neither: the join removed 84 pairs by its Join Filter
// and 15 joined rows by its Filter, the scan of owners one row by its name, and the Materialize and
// the scan of pets under it removed nothing, carrying no filter.
func TestNodesReadTheRowsEachFilterRemoved(t *testing.T) {
	type removed struct {
		node               string
		loops              float64
		filter, joinFilter float64
	}
	want := []removed{
		{node: "Nested Loop", loops: 1, filter: 15, joinFilter: 84},
		{node: "Index Scan on owners using owners_pkey", loops: 1, filter: 1},
		{node: "Materialize", loops: 6},
		{node: "Index Scan on pets using pets_pkey", loops: 1},
	}
	nodes := Nodes(t, OnePlan(t, []byte(filteredJoinPlan)))
	got := make([]removed, len(nodes))
	for index, node := range nodes {
		got[index] = removed{node: node.String(), loops: node.Loops, filter: node.RowsRemovedByFilter, joinFilter: node.RowsRemovedByJoinFilter}
	}
	if len(got) != len(want) {
		t.Fatalf("Nodes = %+v; want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("node %d = %+v; want %+v", index, got[index], want[index])
		}
	}
}

// A check that fails a control's plan is the control failing, as it must, and RequireControlFails
// fails nothing; a check that passes it is one that cannot fail, and RequireControlFails fails
// that, naming the plan, the control and what the check cannot tell apart.
func TestRequireControlFailsFailsACheckThatCannotFail(t *testing.T) {
	plan := json.RawMessage(`{"Node Type": "Result"}`)
	failing := &controlFailures{TB: t}
	RequireControlFails(failing, "the read", "the control", "one read from another", plan, func(t testing.TB) {
		t.Errorf("the read reads events twice:\n%s", plan)
	})
	if lines := failing.firstLines(); len(lines) != 0 {
		t.Errorf("a check that fails the control fails %q, want nothing", lines)
	}
	passing := &controlFailures{TB: t}
	RequireControlFails(passing, "the read", "the control", "one read from another", plan, func(testing.TB) {})
	want := []string{"the read, the control, passes the check, so the check cannot tell one read from another:"}
	if lines := passing.firstLines(); !slices.Equal(lines, want) {
		t.Errorf("a check that passes the control fails %q, want %q", lines, want)
	}
}
