package explaintest

import (
	"os"
	"slices"
	"testing"
)

// verboseBuffersPlan is testdata/verbose_buffers.json, PostgreSQL 18.7's `EXPLAIN (ANALYZE,
// BUFFERS, VERBOSE, FORMAT JSON)` of a statement that reads one CTE twice, the CTE a union of two
// scans whose rows each begin with two text literals: the search statements' arms, a kind and a
// list literal under one Append in a CTE read twice, in small. It was captured on the server
// LEGION_TEST_PG_DSN names by:
//
//	psql "$LEGION_TEST_PG_DSN" -X -q -At -c "explain (analyze, buffers, verbose, format json) with k as (select 'a'::text as kind, 'x'::text as list, relname as name from pg_class union all select 'b', 'y', proname from pg_proc) select (select count(*) from k), (select count(*) from k where kind = 'a')"
const verboseBuffersPlan = "testdata/verbose_buffers.json"

// Nodes names each node's parent by its position in the walk, the subplan the node begins by
// the plan's own name for it, and its VERBOSE output: the CTE's Append begins `CTE k` under the
// root, each scalar subquery's Aggregate begins an init plan of its own, the two scans of the
// union are the Append's children and print the literals the statement wrote, and the CTE Scans
// sit under the Aggregates that read them. Subplan stays empty throughout, since none of these
// nodes is under a SubPlan.
//
// The buffers are the fixture's too: the Append holds the two scans' blocks, and the CTE Scan
// that ran the CTE's plan, the first one, holds the same blocks again, though the statement read
// them once.
func TestNodesNameTheirParentSubplanAndOutput(t *testing.T) {
	raw, err := os.ReadFile(verboseBuffersPlan)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	nodes := Nodes(t, OnePlan(t, raw))

	type placed struct {
		node, subplanName, subplan string
		parent                     int
	}
	want := []placed{
		{node: "Result", parent: -1},
		{node: "Append", parent: 0, subplanName: "CTE k"},
		{node: "Seq Scan on pg_class", parent: 1},
		{node: "Seq Scan on pg_proc", parent: 1},
		{node: "Aggregate", parent: 0, subplanName: "InitPlan 2"},
		{node: "CTE Scan on k", parent: 4},
		{node: "Aggregate", parent: 0, subplanName: "InitPlan 3"},
		{node: "CTE Scan on k", parent: 6},
	}
	got := make([]placed, len(nodes))
	for index, node := range nodes {
		got[index] = placed{node: node.String(), subplanName: node.SubplanName, subplan: node.Subplan, parent: node.Parent}
	}
	if len(got) != len(want) {
		t.Fatalf("Nodes = %+v; want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("node %d = %+v; want %+v", index, got[index], want[index])
		}
	}

	outputs := [][]string{
		{"(InitPlan 2).col1", "(InitPlan 3).col1"},
		nil,
		{"'a'::text", "'x'::text", "pg_class.relname"},
		{"'b'::text", "'y'::text", "pg_proc.proname"},
		{"count(*)"},
		{"k.kind", "k.list", "k.name"},
		{"count(*)"},
		{"k_1.kind", "k_1.list", "k_1.name"},
	}
	for index, output := range outputs {
		if !slices.Equal(nodes[index].Output, output) {
			t.Errorf("node %d (%s) Output = %q; want %q", index, nodes[index], nodes[index].Output, output)
		}
	}

	union, first, second := nodes[1], nodes[5], nodes[7]
	if scans := nodes[2].SharedHitBlocks + nodes[3].SharedHitBlocks; union.SharedHitBlocks != scans {
		t.Errorf("the Append hit %v blocks; want its two scans' %v", union.SharedHitBlocks, scans)
	}
	if first.SharedHitBlocks != union.SharedHitBlocks || second.SharedHitBlocks != 0 {
		t.Errorf("the CTE Scans hit %v and %v blocks; want the CTE's %v under the first, which ran it, and none under the second",
			first.SharedHitBlocks, second.SharedHitBlocks, union.SharedHitBlocks)
	}
}
