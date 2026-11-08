package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan proves each meaning candidate CTE's own
// shape - `order by embedding <=> $qvec limit $k` against embeddings alone, with no shared window
// spanning every kind, filtered to the query embedder's identity - is what lets pgvector's planner
// pick an Index Scan on embeddings_cosine (0072_embeddings_core.up.sql's HNSW index) rather than a
// sequential scan sorted in memory. It explains searchQueryMeaning itself, every parameter bound
// as runMeaningSearch binds it ($11 the fake embedder's identity), and reads each
// meaning_<kind>_candidates subtree of the plan, so the statements under test are the ones search
// runs, not a copy of them. enable_seqscan and enable_sort are forced off for this EXPLAIN only: a
// handful of test rows is too small for the planner to ever prefer the index on cost alone
// (reading a few rows via its primary key and sorting them in memory is cheaper than any index no
// matter what exists - confirmed empirically: without enable_sort off, the planner chose exactly
// that plan, an Index Scan on embeddings_pkey feeding a Sort node, not embeddings_cosine at all),
// so this proves the query's shape permits an index scan, which is the claim under test, not what
// a real corpus's planner would choose - search_bench_test.go's latency bound is what proves that
// at scale.
func TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan(t *testing.T) {
	const title, spec = "A title to embed for the index test", "Body text."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "IDX1", title, spec)
	processAllPending(t, database, embedder)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan off: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local enable_sort = off"); err != nil {
		t.Fatalf("set local enable_sort off: %v", err)
	}

	const q = "index test"
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+searchQueryMeaning, searchMeaningQueryArguments(
		searchParameters{text: q, limit: searchDefaultLimit}, queryVector(), embedder.Identity())...).Scan(&planJSON); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode explain output (%v): %s", err, planJSON)
	}

	for _, kind := range []string{"issue", "document", "comment", "ask", "message"} {
		t.Run(kind, func(t *testing.T) {
			alias := "meaning_" + kind + "_candidates"
			leg := plan[0].Plan.find(func(node planNode) bool { return node.Alias == alias })
			if leg == nil {
				t.Fatalf("the plan has no node for %s:\n%s", alias, planString(t, planJSON))
			}
			legPlan, err := json.MarshalIndent(leg, "", "  ")
			if err != nil {
				t.Fatalf("re-encode %s: %v", alias, err)
			}
			if leg.find(func(node planNode) bool { return node.NodeType == "Seq Scan" }) != nil {
				t.Errorf("%s meaning leg's plan uses a Seq Scan, want an Index Scan on embeddings_cosine:\n%s", kind, legPlan)
			}
			cosine := leg.find(func(node planNode) bool { return node.IndexName == "embeddings_cosine" })
			if cosine == nil {
				t.Fatalf("%s meaning leg's plan never names embeddings_cosine, want it to use that index:\n%s", kind, legPlan)
			}
			if !strings.Contains(cosine.Filter+cosine.IndexCond, "model") {
				t.Errorf("%s meaning leg's index scan does not filter on embeddings.model:\n%s", kind, legPlan)
			}
		})
	}
}

// planNode is the part of one EXPLAIN (FORMAT JSON) plan node these tests read. A keyword arm's
// match can be written on any of the three condition fields - an index condition, the recheck a
// bitmap heap scan makes, or a filter - and the issue arm's test below asks which, so all three
// are read, with the join filter a node that names no relation of its own carries.
type planNode struct {
	NodeType    string     `json:"Node Type"`
	Alias       string     `json:"Alias,omitempty"`
	IndexName   string     `json:"Index Name,omitempty"`
	IndexCond   string     `json:"Index Cond,omitempty"`
	RecheckCond string     `json:"Recheck Cond,omitempty"`
	Filter      string     `json:"Filter,omitempty"`
	JoinFilter  string     `json:"Join Filter,omitempty"`
	Plans       []planNode `json:"Plans,omitempty"`
}

// conditions are every condition the node carries, in no particular order.
func (node planNode) conditions() []string {
	return []string{node.IndexCond, node.RecheckCond, node.Filter, node.JoinFilter}
}

// find answers the first node of the subtree rooted at node, depth first, that match accepts.
func (node *planNode) find(match func(planNode) bool) *planNode {
	if match(*node) {
		return node
	}
	for i := range node.Plans {
		if found := node.Plans[i].find(match); found != nil {
			return found
		}
	}
	return nil
}

func planString(t *testing.T, planJSON []byte) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(planJSON, &decoded); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	pretty, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatalf("re-encode explain output: %v", err)
	}
	return string(pretty)
}

// issuesSearchIndex is the GIN index over issues.search that the keyword search's issue arm
// reads (0010, and 20261108015800 takes its pending list off).
const issuesSearchIndex = "issues_search"

// The issue arm's vector match is an index condition of its own, which is what lets the planner
// read issues_search for it. Before INFRA-1042 the arm made one predicate do two jobs -
// `(i.search @@ q.tsq or i.key = any(q.own_keys))` - and the plan carried that OR whole, as a
// join filter over a sequential scan of issues or at best as the recheck of a BitmapOr, which
// re-tested every candidate row's vector on the heap. Split, the match stands alone and the
// index answers it; the issues the query names by key are a second arm, read through the
// primary key.
//
// Two things are held. The vector match is read from issues_search under a condition naming
// nothing but the match, and no condition anywhere in the plan tests the vector and the key
// together, which is the shape that cost the arm its index.
//
// Sequential scans are off for the plan, as every plan test in this package runs: a fixture of a
// handful of rows is read fastest whole whatever indexes exist, so this says the arm's shape
// permits the index scan, which is the claim under test. What a corpus of production's size
// makes the planner choose is scripts/search-measure's to measure.
func TestTheIssueKeywordArmMatchesItsVectorThroughIssuesSearch(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createInteractionIssue(t, handler, "SRCH", "Navigation instruments", "The astrolabe measures altitude.\n")

	parameters := searchParameters{text: "astrolabe", limit: searchDefaultLimit}
	plan := planWithoutSeqScans(t, database, searchQuery, searchQueryArguments(parameters)...)

	matched := false
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index != issuesSearchIndex {
			continue
		}
		matched = true
		if !strings.Contains(scan.Cond, "@@") {
			t.Errorf("%s is read under %q, want the tsquery match as its condition:\n%s", issuesSearchIndex, scan.Cond, plan)
		}
	}
	if !matched {
		t.Fatalf("no scan of the plan reads %s, so the issue arm's vector match is no index condition:\n%s",
			issuesSearchIndex, plan)
	}

	var root planNode
	if err := json.Unmarshal(plan, &root); err != nil {
		t.Fatalf("decode the plan tree: %v", err)
	}
	both := root.find(func(node planNode) bool {
		for _, condition := range node.conditions() {
			if strings.Contains(condition, "@@") && strings.Contains(condition, "own_keys") {
				return true
			}
		}
		return false
	})
	if both != nil {
		t.Fatalf("the plan still tests the vector and the issue's key in one condition at %s, which no index serves:\n%s",
			both.NodeType, plan)
	}
}
