package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan proves each meaning candidate CTE's own
// shape - `order by embedding <=> $qvec limit $k` against embeddings alone, with no shared window
// spanning every kind, filtered to the query embedder's identity - is what lets pgvector's planner
// pick an Index Scan on embeddings_cosine (0072_embeddings_core.up.sql's HNSW index) rather than a
// sequential scan sorted in memory. It explains searchQueryMeaning, the shape a search that
// names no project runs (meaningProjectScope), every parameter bound as runMeaningSearch binds
// it for one ($2 empty, $11 the fake embedder's identity), and reads each
// meaning_<kind>_candidates subtree of the plan, so the statements under test are the ones search
// runs, not a copy of them. The scoped shape's own plan is
// TestTheScopedMeaningStatementReadsTheProjectIndex below. meaningQueryPlan forces
// enable_seqscan and enable_sort off for this
// EXPLAIN only: a handful of test rows is too small for the planner to ever prefer the index on
// cost alone
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

	root, planJSON := meaningQueryPlan(t, database, embedder)

	for _, kind := range []string{"issue", "document", "comment", "ask", "message"} {
		t.Run(kind, func(t *testing.T) {
			alias := "meaning_" + kind + "_candidates"
			leg := root.find(func(node planNode) bool { return node.Alias == alias })
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

// meaningQueryPlan is the plan Postgres builds for searchQueryMeaning, the unscoped shape, with
// every parameter bound as runMeaningSearch binds it for a search that names no project: $2 the
// empty project, $9 the vector every test query embeds to (queryVector), $11 the embedder's own
// identity. enable_seqscan and enable_sort are off for the reason the test above gives: a
// fixture's handful of rows is read whole and sorted exactly however the statement is written,
// so without those two the plan says nothing about the statement's shape.
func meaningQueryPlan(t *testing.T, database *store.Store, embedder embed.Embedder) (planNode, []byte) {
	t.Helper()
	return meaningPlanOf(t, database, embedder, searchQueryMeaning, "",
		"set local enable_seqscan = off", "set local enable_sort = off")
}

// meaningPlanOf is the plan Postgres builds for one of the two meaning statements with project
// bound as $2, under settings. It runs in a transaction of its own, rolled back when the test
// ends, and answers the plan's root node and the EXPLAIN output itself, which a failing test
// prints.
func meaningPlanOf(
	t *testing.T, database *store.Store, embedder embed.Embedder, statement, project string, settings ...string,
) (planNode, []byte) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}

	const q = "index test"
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, q, project, searchDefaultLimit, firstTerm(q), headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK, embed.Literal(queryVector()), searchMeaningFloor, embedder.Identity()).Scan(&planJSON); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode explain output (%v): %s", err, planJSON)
	}
	return plan[0].Plan, planJSON
}

// TestTheScopedMeaningStatementReadsTheProjectIndex is the other half of the pair above, and
// the reason there are two statements rather than one predicate (meaningProjectScope). A
// project-scoped search carries `e.project_key = $2` inside every candidate CTE, and for a
// project holding few of a kind's rows the plan that answers it is an exact read of those rows
// through embeddings_kind_project (migration 20261109120000) and a sort of them, not a walk of
// embeddings_cosine filtered down to them: on the load-test deployment that is 800 of 800
// windows in 13.7 ms against 2 of 800 in 225 ms (INFRA-988). The unscoped statement's own plan
// is the test above, which reads embeddings_cosine for all five arms; one "$2 is empty, or it
// matches" predicate would have to be planned once for both and can lose either.
//
// The fixture is the shape the measurement was taken on: one small project beside a crowd of
// another project's windows, analyzed so the planner's statistics hold the difference.
// enable_seqscan is off, as every plan test of this package reads its plan; enable_sort is left
// on, because the exact sort is half of the plan under test.
func TestTheScopedMeaningStatementReadsTheProjectIndex(t *testing.T) {
	const title, spec = "A title to embed for the scoped plan", "Body text."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "LOUD", "Another project's issue", "Body text.")
	issue := createInteractionIssue(t, handler, "SARM", title, spec)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "One remark, so the arm has a window to read."}, "alice")
	embedWindowsAs(t, database, embedder, "comment_block", "comment/"+comment.ID, angledVector(0.8, 3))
	crowdCommentWindows(t, database, embedder, "LOUD", searchWindowCandidates+1, angledVector(0.9, 1))
	processAllPending(t, database, embedder)
	if _, err := database.Pool.Exec(context.Background(), "analyze embeddings"); err != nil {
		t.Fatalf("analyze embeddings: %v", err)
	}

	root, planJSON := meaningPlanOf(t, database, embedder, searchQueryMeaningScoped, "SARM",
		"set local enable_seqscan = off")
	leg := root.find(func(node planNode) bool { return node.Alias == "meaning_comment_candidates" })
	if leg == nil {
		t.Fatalf("the plan has no node for meaning_comment_candidates:\n%s", planString(t, planJSON))
	}
	scan := leg.find(func(node planNode) bool { return node.IndexName == "embeddings_kind_project" })
	if scan == nil {
		t.Fatalf("the scoped comment arm reads no index named embeddings_kind_project, so the project predicate is not what cuts its candidates:\n%s",
			planString(t, planJSON))
	}
	if !strings.Contains(scan.Filter+scan.IndexCond, "project_key") {
		t.Errorf("the scoped comment arm's scan of embeddings_kind_project carries no project_key condition:\n%s", planString(t, planJSON))
	}
}

// TestSearchMeaningCommentArmTakesItsWindowsBeforeItsJoins holds INFRA-988's measurement as a
// plan. embeddings_cosine answers `order by embedding <=> $9` only while nothing between that
// scan and the LIMIT it feeds has to see every row it hands up: the comment arm this replaced
// joined comments, issues, artifacts and projects inside its ordered, limited select, and the
// planner then joined first and ordered after - on the load-test corpus a parallel hash join
// whose 475 planned rows were 120,519, spilling one hash batch into 32, both tables read whole
// and 120 K rows top-N sorted, 5,166 ms of a 5,351 ms statement. The test explains
// searchQueryMeaning itself (meaningQueryPlan), finds the index scan that reads the comment
// windows, and walks up from it: the first node above that could see its rows must be the arm's
// own LIMIT. Against the arm this replaced the walk meets a join instead, and this fails - which
// is what makes the test a test of the arm's order rather than of its index.
func TestSearchMeaningCommentArmTakesItsWindowsBeforeItsJoins(t *testing.T) {
	const title, spec = "A title to embed for the comment arm's plan", "Body text."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "CARM", title, spec)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "One remark, so the arm has a window to read."}, "alice")
	embedWindowsAs(t, database, embedder, "comment_block", "comment/"+comment.ID, angledVector(0.8, 3))
	processAllPending(t, database, embedder)

	root, planJSON := meaningQueryPlan(t, database, embedder)
	path := root.pathTo(func(node planNode) bool {
		return node.IndexName == "embeddings_cosine" && strings.Contains(node.Filter+node.IndexCond, "comment_block")
	})
	if path == nil {
		t.Fatalf("no scan of embeddings_cosine reads the comment windows:\n%s", planString(t, planJSON))
	}
	for above := len(path) - 2; above >= 0; above-- {
		node := path[above]
		if node.NodeType == "Limit" {
			return
		}
		if seesEveryRow(node.NodeType) {
			t.Fatalf("a %s stands between the comment arm's scan of embeddings_cosine and its limit of %d windows, so the arm joins and orders before the index cuts its candidates:\n%s",
				node.NodeType, searchWindowCandidates, planString(t, planJSON))
		}
	}
	t.Fatalf("the comment arm's scan of embeddings_cosine feeds no limit, so it reads every comment window:\n%s", planString(t, planJSON))
}

// seesEveryRow answers whether a node standing between a window arm's index scan and its LIMIT
// would have to read every row that scan hands up: a join brings the item's tables in below the
// limit, and a sort re-orders what the index already ordered. Either is the shape INFRA-988
// measured, and either costs the index its ordering.
func seesEveryRow(nodeType string) bool {
	return strings.Contains(nodeType, "Join") || nodeType == "Nested Loop" ||
		nodeType == "Sort" || nodeType == "Incremental Sort"
}

// planNode is the part of one EXPLAIN (FORMAT JSON) plan node these tests read.
type planNode struct {
	NodeType  string     `json:"Node Type"`
	Alias     string     `json:"Alias,omitempty"`
	IndexName string     `json:"Index Name,omitempty"`
	IndexCond string     `json:"Index Cond,omitempty"`
	Filter    string     `json:"Filter,omitempty"`
	Plans     []planNode `json:"Plans,omitempty"`
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

// pathTo answers the nodes from this one down to the first node of its subtree, depth first,
// that match accepts - this node first, the matched one last - or nil when none does, so a
// caller can read what stands above a scan as well as that the scan is there.
func (node *planNode) pathTo(match func(planNode) bool) []planNode {
	if match(*node) {
		return []planNode{*node}
	}
	for i := range node.Plans {
		if found := node.Plans[i].pathTo(match); found != nil {
			return append([]planNode{*node}, found...)
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
