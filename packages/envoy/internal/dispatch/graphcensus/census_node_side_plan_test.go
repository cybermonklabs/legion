package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// censusNodeSideBlocks is the size of the document TestTheCensusNodeSideReadsItsRowsOnceWhateverTheStatistics
// seeds: a few hundred blocks, so a plan that reads the row side once for each node reads it a few
// hundred times.
const censusNodeSideBlocks = 300

// planJoin is the part of a plan node that explaintest.Node does not carry: the type of a join. A
// plan in JSON names a join node by its strategy alone (`Nested Loop`, `Hash Join`) and gives the
// type in a field of its own.
type planJoin struct {
	NodeType string     `json:"Node Type"`
	JoinType string     `json:"Join Type"`
	Plans    []planJoin `json:"Plans"`
}

// antiJoins appends to found the name of every node of the plan under n that is a join of an Anti
// type, in the walk's order, named as the text form of a plan names it: `Nested Loop Anti Join`,
// `Hash Anti Join`.
func (n planJoin) antiJoins(found []string) []string {
	if strings.Contains(n.JoinType, "Anti") {
		found = append(found, strings.Replace(n.NodeType, " Join", "", 1)+" "+n.JoinType+" Join")
	}
	for _, child := range n.Plans {
		found = child.antiJoins(found)
	}
	return found
}

// TestTheCensusNodeSideReadsItsRowsOnceWhateverTheStatistics holds the node side of the store
// census's block kind (nodeDifferences) to reading document_blocks once, whatever the planner
// estimates of the node side and the row side.
//
// The seed is the database as the census finds it after a drain. A document of
// censusNodeSideBlocks blocks is written as a build before the store existed wrote it, with no
// node. Autovacuum is turned off on the tables the census reads, so nothing reads them again
// during the test, and they are analyzed: the statistics count no block node and no row that names
// one. Then Backfill gives each block its node and each row its node, as the drain does, and
// nothing analyzes again. The planner now estimates one row on each side, though each holds
// censusNodeSideBlocks. A statement that tests each node with `not exists` over the row side is
// planned from those estimates as a Nested Loop Anti Join that reads the row side once for each
// node.
//
// Each of explaintest.PlanCacheModes explains the statement with ANALYZE, as the census runs it
// (a count of nodeDifferences's rows). The plan must hold no join of an Anti type, and every scan
// of document_blocks in it must have run once. A plan that scans document_blocks nowhere fails
// too, since it would prove nothing of the row side. explaintest.ExplainAfter turns hash and merge
// joins off, so a join of the two sides could only be a nested loop: the plan is the worst one a
// planner can make of any statistics.
//
// The deliberate break is nodeDifferences with its `not exists` put back. The plan then holds a
// Nested Loop Anti Join that scans document_blocks once for each node, and this test fails naming
// that join and the scan's loops, under both modes.
func TestTheCensusNodeSideReadsItsRowsOnceWhateverTheStatistics(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	blockIDs := make([]string, censusNodeSideBlocks)
	for i := range blockIDs {
		blockIDs[i] = fmt.Sprintf("b%d", i+1)
	}
	seedPreStoreDocument(t, database, blockIDs...)
	for _, step := range []struct{ what, sql string }{
		{"turn autovacuum off on graph_nodes", `alter table graph_nodes set (autovacuum_enabled = false)`},
		{"turn autovacuum off on graph_tuples", `alter table graph_tuples set (autovacuum_enabled = false)`},
		{"turn autovacuum off on document_blocks", `alter table document_blocks set (autovacuum_enabled = false)`},
		{"turn autovacuum off on block_refs", `alter table block_refs set (autovacuum_enabled = false)`},
		{"turn autovacuum off on artifacts", `alter table artifacts set (autovacuum_enabled = false)`},
		{"analyze the tables the census reads before any block has a node",
			`analyze graph_nodes, graph_tuples, document_blocks, block_refs, artifacts`},
	} {
		if _, err := database.Pool.Exec(ctx, step.sql); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// The precondition: every block has a node and every row names it, and the statistics of
	// document_blocks still hold node_kind null on every row, so the plans below are read under
	// the statistics this test is about.
	var named, nodes int
	var nullFraction float64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks where node_kind = 'block'),
		       (select count(*) from graph_nodes where kind = 'block'),
		       (select coalesce(null_frac, -1) from pg_stats
		        where schemaname = current_schema() and tablename = 'document_blocks' and attname = 'node_kind')
	`).Scan(&named, &nodes, &nullFraction); err != nil {
		t.Fatalf("read the state the census plan is read in: %v", err)
	}
	if named != censusNodeSideBlocks || nodes != censusNodeSideBlocks || nullFraction != 1 {
		t.Fatalf("after the backfill %d rows name a block node, %d block nodes exist and the statistics of document_blocks.node_kind hold a null fraction of %v; want %d, %d and 1, so this test would read the plans under other statistics than the ones it holds the statement to",
			named, nodes, nullFraction, censusNodeSideBlocks, censusNodeSideBlocks)
	}

	var held projection
	for _, candidate := range projections {
		if candidate.name == "block" {
			held = candidate
		}
	}
	if held.name == "" {
		t.Fatalf("the census holds no block kind")
	}
	statement := explaintest.Statement{SQL: `select count(*) from (` + nodeDifferences(held) + `) nodes_found`}
	for _, mode := range explaintest.PlanCacheModes {
		plan := explaintest.ExplainAfter(t, ctx, database.Pool, nil, statement, mode, "json")
		var root planJoin
		if err := json.Unmarshal(plan, &root); err != nil {
			t.Fatalf("decode the plan under %s: %v", mode, err)
		}
		faults := root.antiJoins(nil)
		scans := 0
		var reads []string
		for _, node := range explaintest.Nodes(t, plan) {
			if node.Relation != "document_blocks" {
				continue
			}
			scans++
			reads = append(reads, fmt.Sprintf("%s ran %.0f times", node, node.Loops))
			if node.Loops != 1 {
				faults = append(faults, fmt.Sprintf("%s ran %.0f times, want once", node, node.Loops))
			}
		}
		if scans == 0 {
			faults = append(faults, "the plan scans document_blocks nowhere, so it reads no row of the row side")
		}
		if len(faults) > 0 {
			t.Errorf("the census's block node side under %s reads its rows more than once:\n  %s\nplan:\n%s",
				mode, strings.Join(faults, "\n  "), explaintest.ExplainAfter(t, ctx, database.Pool, nil, statement, mode, "text"))
			continue
		}
		t.Logf("the census's block node side under %s holds no anti join and reads document_blocks as follows:\n  %s", mode, strings.Join(reads, "\n  "))
	}
}
