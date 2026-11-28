package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The reads of c-tuple-indexes beside the walk of an issue's tree (asOfReads): the needs walk in
// both directions, and the backlink count. The coverage walk over serves is a read of package
// api, which this package cannot import, so the test of the same name there plans it
// (api.TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore), and the check's run, over ./..., runs
// both.

// tupleIndexesNeedsSeed is the needs tuples seedAsOfStore writes for the needs walk: each block of
// SEED-1's tree but the first needs the block before it, and SEED-2 needs SEED-1.
var tupleIndexesNeedsSeed = `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select 'block', 'SEED-1/spec#b' || i, '` + graphstore.TupleNeeds + `', 'block', 'SEED-1/spec#b' || (i - 1), now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(2, ` + strconv.Itoa(asOfSeedBlocks) + `) i
		 union all
		 select 'issue', 'SEED-2', '` + graphstore.TupleNeeds + `', 'issue', 'SEED-1', now() - interval '1 day', '{}'::jsonb, 0`

// tupleIndexesWalkReads are the needs walk out of every issue and plan node of a tree, as the plan
// order reads it (planTree.readNeeds); the needs walk into a node, as the store reads what points
// into one (graphstore.CurrentIn), and as R24's neighbourhood read walks needs, beside its other
// relations, into each target an issue reaches (IssueNeighbours); and the backlink count of an
// issue, as the issue read counts its own (BacklinkCounts). Each is the statement its reader
// sent, with the arguments it bound, kept by a recording transaction (explaintest.RecordedTx), so
// the plan is the reader's own and no copy of it; and the needs walks' answers over the seed are
// held to the tuples the seed wrote, so the statement planned is a walk that reads them. The seed
// holds no issues row, so the neighbourhood read answers no candidate; its plan is still the one
// the planner takes for the statement, each probe into a target among it.
func tupleIndexesWalkReads(t *testing.T, database *store.Store) []asOfRead {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction the walks are recorded in: %v", err)
	}
	defer tx.Rollback(ctx)
	var reads []asOfRead

	recorded := &explaintest.RecordedTx{Tx: tx}
	tree := &planTree{root: "SEED-1", issues: map[string]*planIssue{"SEED-1": {}, "SEED-2": {parent: "SEED-1"}}, plans: map[string]*planNode{}}
	for index := range asOfSeedBlocks {
		tree.plans["SEED-1/spec#b"+strconv.Itoa(index+1)] = &planNode{issue: "SEED-1"}
	}
	if err := tree.readNeeds(ctx, recorded, model.Actor{}); err != nil {
		t.Fatalf("read the needs out of SEED-1's tree: %v", err)
	}
	needed := 0
	for _, plan := range tree.plans {
		needed += len(plan.needs)
	}
	if got := tree.plans["SEED-1/spec#b2"].needs; needed != asOfSeedBlocks-1 || !slices.Equal(got, []Target{{Kind: BlockNodeKind, ID: "SEED-1/spec#b1"}}) {
		t.Errorf("the needs walk out of SEED-1's tree answers %d needs of its blocks, b2's %v, want %d, b2's the block b1", needed, got, asOfSeedBlocks-1)
	}
	if got := tree.issues["SEED-2"].needs; !slices.Equal(got, []Target{{Kind: graphstore.KindIssue, ID: "SEED-1"}}) {
		t.Errorf("the needs walk out of SEED-2 answers %v, want the issue SEED-1", got)
	}
	walked := recorded.Holding(t, "t.kind = '"+graphstore.TupleNeeds+"'")
	reads = append(reads, asOfRead{"the needs walk out of every issue and plan node of a tree (planTree.readNeeds)", walked.SQL, walked.Args,
		[]string{"graph_tuples_current", "graph_tuples_as_of_out"}})

	recorded = &explaintest.RecordedTx{Tx: tx}
	into, err := graphstore.CurrentIn(ctx, recorded, BlockNodeKind, "SEED-1/spec#b1", []string{graphstore.TupleNeeds})
	if err != nil {
		t.Fatalf("read the needs into SEED-1's block b1: %v", err)
	}
	if len(into) != 1 || into[0].FromKind != BlockNodeKind || into[0].FromID != "SEED-1/spec#b2" {
		t.Errorf("the needs walk into SEED-1's block b1 answers %+v, want the needs of b2 alone", into)
	}
	if len(recorded.Sent) != 1 {
		t.Fatalf("graphstore.CurrentIn sent %d statements, want one", len(recorded.Sent))
	}
	reads = append(reads, asOfRead{"the needs walk into a node (graphstore.CurrentIn)", recorded.Sent[0].SQL, recorded.Sent[0].Args,
		[]string{"graph_tuples_as_of_in"}})

	recorded = &explaintest.RecordedTx{Tx: tx}
	if _, err := IssueNeighbours(ctx, recorded, model.Actor{}, "SEED-2", 5); err != nil {
		t.Fatalf("read the graph neighbourhood of SEED-2: %v", err)
	}
	if len(recorded.Sent) != 1 {
		t.Fatalf("IssueNeighbours sent %d statements, want one", len(recorded.Sent))
	}
	reads = append(reads, asOfRead{"the needs walk, with R24's other relations, into each target an issue reaches (IssueNeighbours)", recorded.Sent[0].SQL, recorded.Sent[0].Args,
		[]string{"graph_tuples_as_of_in"}})

	recorded = &explaintest.RecordedTx{Tx: tx}
	counts, err := BacklinkCounts(ctx, recorded, model.Actor{}, graphstore.KindIssue, []string{"SEED-1"}, nil)
	if err != nil {
		t.Fatalf("count the backlinks of SEED-1: %v", err)
	}
	if len(recorded.Sent) != 1 {
		t.Fatalf("BacklinkCounts sent %d statements, want one", len(recorded.Sent))
	}
	t.Logf("the backlink count of SEED-1 answers %v", counts)
	reads = append(reads, asOfRead{"the backlink count of an issue, as the issue read counts its own (BacklinkCounts)", recorded.Sent[0].SQL, recorded.Sent[0].Args,
		[]string{"graph_tuples_as_of_in"}})
	return reads
}

// tupleIndexesUnkeyedScans names each scan of plan that reads graph_tuples or graph_nodes
// through one of its indexes on a condition that names nothing of that index's first key column:
// the scan reads the whole index, or skips over its first column, where a read that binds the
// column is one search. A read that drops the kind it binds into a node, and keeps the predicate
// the in index is partial on, is planned so (explaintest.CondNamesFirstKey): its plan holds no Seq
// Scan, and reads graph_tuples_as_of_in all the same. A scan with no condition at all is the
// whole-index check's to name, which the test makes beside this one.
func tupleIndexesUnkeyedScans(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) []string {
	t.Helper()
	var unkeyed []string
	for _, node := range explaintest.Nodes(t, plan) {
		if node.IndexCond == "" || !strings.HasPrefix(node.Index, "graph_tuples") && !strings.HasPrefix(node.Index, "graph_nodes") {
			continue
		}
		if firstKey := keys.FirstKey(t, node.Index); !explaintest.CondNamesFirstKey(node.IndexCond, firstKey) {
			unkeyed = append(unkeyed, fmt.Sprintf("%s on `%s`, which names nothing of its first key column `%s`", node, node.IndexCond, firstKey))
		}
	}
	return unkeyed
}

// tupleIndexesKindLeftOut is the second control: the blocks of a document read with the kind of
// the document left out of its predicate, and the predicate graph_tuples_as_of_in is partial on
// kept, as a read that loses its kind bind and nothing else is written. The test must judge it a
// read no index serves, by a Seq Scan or by a scan that skips the index's first key column.
const tupleIndexesKindLeftOut = `
	select t.from_kind, t.from_id from graph_tuples t
	where t.to_id = $1 and t.kind = '` + TupleInDocument + `' and t.to_kind <> '` + graphstore.KindValue + `' and t.seq <= $2`
