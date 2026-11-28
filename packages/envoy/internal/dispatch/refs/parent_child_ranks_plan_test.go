package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// parentChildRanksPlanBlocks is how many blocks the store under test holds: a plan that walks
// every current tuple out of a block for each parent walks that many blocks' tuples each time,
// and the control, which runs that plan, still finishes in a second.
const parentChildRanksPlanBlocks = 1000

// TestTheParentChildRanksReadProbesEachParentByItsFarEnd explains parentChildRanks, the read of
// the ranks every parent's children hold that a block index pass makes before it gives a block a
// new rank (spaceFromStoredRanks), over parentChildRanksPlanBlocks blocks in chains of four, the
// first of each chain under the issue's node and each other under the block before it, each
// stating its part_of, its in_document and its rank, with graph_nodes and graph_tuples analyzed
// while they held the migrations' seed rows alone: the planner then counts no tuple out of a
// block, as it does in a store whose first blocks the statistics have not reached yet. The blocks
// are written in the explaining transaction, before the read. Under each plan-cache mode the plan
// must read the tuples into each parent by a probe of an index whose first key is the parent's
// kind, and every other tuple by a probe a child's address keys (unkeyedParentReads).
//
// The statement is the one the read sends, with the arguments it binds (explaintest.RecordedTx),
// over every block and the issue's node as the parents. The control is that statement with the
// test that a child is a block inside the probe and no offset 0 around it, as it stood before
// (parentChildRanksProbeBefore): it must plan and run, and under the custom plan, the one a
// block index pass plans its statements by (graphstore.PlanAtCurrentSizes), the check must fail
// on it. Under the generic plan the relation's kind is a parameter, which proves nothing of
// graph_tuples_current's predicate, so that index cannot serve the control there and the check
// holds it to nothing.
func TestTheParentChildRanksReadProbesEachParentByItsFarEnd(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	if _, err := pool.Exec(ctx, `analyze graph_nodes, graph_tuples`); err != nil {
		t.Fatalf("analyze the store's tables: %v", err)
	}
	keys := explaintest.NewIndexKeys(pool)

	const issue, refKey = "PLAN-1", "PLAN-1/tree"
	blocks := strconv.Itoa(parentChildRanksPlanBlocks)
	address := `'` + refKey + `#b' || `
	seed := []explaintest.Statement{
		{SQL: `insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 values ('` + graphstore.KindIssue + `', '` + issue + `', 'issue', 'project', 'live', now(), '{}', 0)`},
		{SQL: `insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 select '` + BlockNodeKind + `', ` + address + `i, '` + BlockSupertag + `', 'project', 'live', now(), '{}', 0
		 from generate_series(1, ` + blocks + `) i`},
		{SQL: `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select '` + BlockNodeKind + `', ` + address + `i, '` + graphstore.TuplePartOf + `',
		        case when i % 4 = 1 then '` + graphstore.KindIssue + `' else '` + BlockNodeKind + `' end,
		        case when i % 4 = 1 then '` + issue + `' else ` + address + `(i - 1) end,
		        now(), '{}'::jsonb, 0
		 from generate_series(1, ` + blocks + `) i
		 union all
		 select '` + BlockNodeKind + `', ` + address + `i, '` + TupleInDocument + `', '` + graphstore.KindArtifact + `', '` + issue + `/spec', now(), '{}'::jsonb, 0
		 from generate_series(1, ` + blocks + `) i
		 union all
		 select '` + BlockNodeKind + `', ` + address + `i, '` + AttributeRank + `', '` + graphstore.KindValue + `', '1' || lpad(i::text, 5, '0'), now(), '{}'::jsonb, 0
		 from generate_series(1, ` + blocks + `) i`},
	}
	parents := make([]blockFarEnd, 0, parentChildRanksPlanBlocks+1)
	parents = append(parents, blockFarEnd{kind: graphstore.KindIssue, id: issue})
	for index := 1; index <= parentChildRanksPlanBlocks; index++ {
		parents = append(parents, blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(refKey, "b"+strconv.Itoa(index))})
	}

	// The statement the read sends, held to answering every block the seed wrote under its parent.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range seed {
		if _, err := tx.Exec(ctx, statement.SQL); err != nil {
			t.Fatalf("seed the blocks: %v\n%s", err, statement.SQL)
		}
	}
	recorded := &explaintest.RecordedTx{Tx: tx}
	children, err := parentChildRanks(ctx, recorded, parents)
	if err != nil {
		t.Fatalf("read the ranks of the parents' children: %v", err)
	}
	if len(children) != parentChildRanksPlanBlocks {
		t.Fatalf("the read answered %d children, want the %d blocks the seed wrote, each under its parent", len(children), parentChildRanksPlanBlocks)
	}
	read := recorded.Holding(t, "ranked.held")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the read back: %v", err)
	}

	const name = "the read of the parents' children"
	for _, mode := range explaintest.PlanCacheModes {
		forbidden, allowed := unkeyedParentReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, seed, read, mode, "json"))
		if len(forbidden) > 0 {
			t.Errorf("%s under %s reads graph_tuples other than by one probe a parent and one a child:\n  %s\nplan:\n%s",
				name, mode, strings.Join(forbidden, "\n  "), explaintest.ExplainAfter(t, ctx, pool, seed, read, mode, "text"))
			continue
		}
		t.Logf("%s under %s probes graph_tuples once a parent and once a child:\n  %s", name, mode, strings.Join(allowed, "\n  "))
	}

	block := explaintest.Parameter(t, read.SQL, regexp.MustCompile(`held\.from_kind = (\$[0-9]+)`))
	control := explaintest.Statement{SQL: explaintest.Spliced(t, read.SQL, parentChildRanksProbeBefore(block)...), Args: read.Args}
	for _, mode := range explaintest.PlanCacheModes {
		forbidden, allowed := unkeyedParentReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, seed, control, mode, "json"))
		switch {
		case mode != "force_custom_plan":
			t.Logf("%s as it stood under %s plans, in a mode its control is not held to, and reads:\n  %s", name, mode,
				strings.Join(append(forbidden, allowed...), "\n  "))
		case len(forbidden) == 0:
			t.Errorf("%s as it stood, the control, passes the check under %s, so the check cannot tell its plan from a probe a parent:\n%s",
				name, mode, explaintest.ExplainAfter(t, ctx, pool, seed, control, mode, "text"))
		default:
			t.Logf("%s as it stood under %s plans and fails the check, as the control must:\n  %s", name, mode, strings.Join(forbidden, "\n  "))
		}
	}
}

// unkeyedParentReads reads plan, one "Plan" node of EXPLAIN (ANALYZE, FORMAT JSON) of
// parentChildRanks' statement, for the reads of graph_tuples that statement may not make, and
// answers each it found (forbidden) and every other read of the table, with how often it ran
// (allowed). A read whose index condition names a far end's address is the read of the tuples
// into a parent: it must go through an index whose first key is the far end's kind, so the scan
// reaches that parent's tuples alone, where an index led by the near end's kind is walked for
// every tuple under that kind, once for each parent. Every other read is the read of a child's
// rank, which the child's address must key. A read under no index condition reads the table or
// an index whole, and a plan that reads no tuple by a parent's address is not the read's.
func unkeyedParentReads(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) (forbidden, allowed []string) {
	t.Helper()
	probes := 0
	for _, node := range explaintest.Nodes(t, plan) {
		bitmap := node.Type == "Bitmap Index Scan" && strings.HasPrefix(node.Index, "graph_tuples")
		if node.Type == "Bitmap Heap Scan" || node.Relation != "graph_tuples" && !bitmap {
			continue
		}
		read := fmt.Sprintf("%s ran %.0f time(s) under `%s`", node, node.Loops, node.IndexCond)
		switch {
		case node.IndexCond == "":
			forbidden = append(forbidden, read+": graph_tuples is read under no index condition")
		case strings.Contains(node.IndexCond, "to_id"):
			probes++
			if first := keys.FirstKey(t, node.Index); first != "to_kind" {
				forbidden = append(forbidden, read+": the tuples into a parent are read through an index whose first key is "+first+
					", not the parent's kind, so every tuple under that key is walked again for each parent")
				continue
			}
			allowed = append(allowed, read)
		case strings.Contains(node.IndexCond, "from_id"):
			allowed = append(allowed, read)
		default:
			forbidden = append(forbidden, read+": the condition names neither a parent's address nor a child's")
		}
	}
	if probes == 0 {
		forbidden = append(forbidden, "the plan reads no tuple of graph_tuples by a parent's address")
	}
	return forbidden, allowed
}

// parentChildRanksProbeBefore is parentChildRanks' probe of a parent's children as it stood
// before, put back into the statement the read sends now: the test that a child is a block, which
// binds block, inside the probe, beside the parent's address, and the probe's answer read with no
// offset 0 around it and no test of the child's kind after it.
func parentChildRanksProbeBefore(block string) []explaintest.Part {
	return []explaintest.Part{
		{
			Name: "the test that a child is a block, inside the probe", Opens: "and t.to_kind <> ", Closes: "order by t.from_kind",
			Text: "and t.from_kind = " + block + "\n\t\t\t\t  and t.to_kind <> '" + graphstore.KindValue + "'\n\t\t\t",
		},
		{
			Name: "the probe's answer with no offset 0 around it", Opens: "offset 0", Closes: ") child on true",
			Text: ") held\n\t\t\twhere held.asserted\n\t\t",
		},
	}
}
