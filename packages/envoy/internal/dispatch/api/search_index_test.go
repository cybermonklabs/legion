package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
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
// bound as $2, under settings. It binds the seventeen parameters of a search as runMeaningSearch
// binds them, and answers the plan's root node and the EXPLAIN output itself, which a failing
// test prints.
func meaningPlanOf(
	t *testing.T, database *store.Store, embedder embed.Embedder, statement, project string, settings ...string,
) (planNode, []byte) {
	t.Helper()
	const q = "index test"
	return planTreeOf(t, database, statement, searchMeaningQueryArguments(
		searchParameters{text: q, project: project, limit: searchDefaultLimit},
		queryVector(), embedder.Identity()), settings...)
}

// planTreeOf is the plan Postgres builds for statement bound to arguments, under settings. It
// runs in a transaction of its own, rolled back when the test ends, and answers the plan's root
// node and the EXPLAIN output itself, which a failing test prints.
func planTreeOf(
	t *testing.T, database *store.Store, statement string, arguments []any, settings ...string,
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

	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, arguments...).Scan(&planJSON); err != nil {
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

// TestTheCrossProjectQuestionStatementReadsTheHNSWIndex holds relatedOtherProjectsSQL to
// embeddings_cosine, as the test at the top of this file holds every meaning leg of search. The
// statement is the cross-project leg of the related-item check (relatedOtherProjects): it reads
// the ask_part windows of the other projects, ordered by the cosine distance to the check's own
// vector and cut to searchWindowCandidates. Every test of its filters (`e.model`, `k.kind`,
// `k.state`, `k.answer` and the project) names a joined table or a column the HNSW index does
// not hold. The joins and those filters stand inside the ordered, limited candidate select,
// between the index scan and the limit, as they do in search's own ask arm
// (meaning_ask_candidates), so what keeps the index is the order alone: the windows are taken
// from embeddings by the cosine distance, which embeddings_cosine serves.
//
// Nothing else in the suite would say that this order was lost. The failure is silent by the
// leg's design: a plan that reads embeddings whole takes far longer than the 300 ms grace of a
// question's check (crossProjectGrace), the stage then answers with no cross-project item and
// records no reason, since the leg is offered beside the check's own result and never decides it
// (TestCrossProjectRelatedQueryFailingKeepsTheOneProjectAnswer). Production offers nothing of
// another project, other_project_offers reads 0, and 0 is also what "no other project qualified"
// reads. This case catches a statement whose order the index cannot serve. It does not catch a
// planner that chooses to join first and order after, as the comment arm's did on the load-test
// corpus, where it read 5,166 ms of a 5,351 ms statement (INFRA-988, searchMeaningCommentArm in
// search.go): the three assertions below do not look for a join between the index scan and its
// limit. TestSearchMeaningCommentArmTakesItsWindowsBeforeItsJoins walks the plan for that, for
// the comment arm alone.
//
// enable_seqscan and enable_sort are off for this EXPLAIN only, for the reason the test at the
// top of this file gives: a fixture's handful of rows is read whole and sorted however the
// statement is written, so without those two the plan says nothing about the statement's shape.
// So this proves that the statement's shape permits the index scan, which is the claim under
// test, not what a real corpus's planner would choose. No test measures this statement on a real
// corpus: search_bench_test.go's TestSearchLatencyOnCorpus times GET /api/v1/search alone.
func TestTheCrossProjectQuestionStatementReadsTheHNSWIndex(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	embedder.vectors[crossProjectQuestion] = queryVector()
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createSuggestionProject(t, handler, "XPLAN")
	createSuggestionProject(t, handler, "XPLOT")
	owner := createSearchIssue(t, handler, "XPLOT", "Backlog runbook", "What to do when the backlog grows.")
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	processAllPending(t, database, embedder)
	windows := embeddedAskWindows(t, database, offered)
	if windows == 0 {
		t.Fatalf("the answered question %s of XPLOT holds no embedded window, so the statement has nothing to plan for", offered)
	}

	// The five parameters relatedOtherProjects binds, in its own order: the question's vector,
	// the project that asks, the embedder's identity, the floor and the depth.
	root, planJSON := planTreeOf(t, database, relatedOtherProjectsSQL, []any{
		embed.Literal(queryVector()), "XPLAN", embedder.Identity(), searchMeaningFloor, contracts.SearchKindDepth,
	}, "set local enable_seqscan = off", "set local enable_sort = off")

	if scan := root.find(func(node planNode) bool { return node.NodeType == "Seq Scan" }); scan != nil {
		t.Errorf("the cross-project statement's plan reads %s with a Seq Scan, want every table read through an index:\n%s",
			scan.Relation, planString(t, planJSON))
	}
	cosine := root.find(func(node planNode) bool { return node.IndexName == "embeddings_cosine" })
	if cosine == nil {
		t.Fatalf("the cross-project statement's plan never names embeddings_cosine, so its windows are not taken by the vector order:\n%s",
			planString(t, planJSON))
	}
	if !strings.Contains(cosine.Filter+cosine.IndexCond, "model") {
		t.Errorf("the cross-project statement's scan of embeddings_cosine carries no condition on embeddings.model, so it reads the vectors of every embedder:\n%s",
			planString(t, planJSON))
	}
	t.Logf("measured: with %d embedded ask window(s) of XPLOT, the cross-project statement plans %q on embeddings_cosine under %q",
		windows, cosine.NodeType, cosine.Filter+cosine.IndexCond)
}

// TestTheScopedMeaningStatementReadsTheProjectIndex is the other half of the pair above, and
// the reason there are three statements rather than one predicate (meaningProjectScope). A
// project-scoped search carries `e.project_key = $2` inside every candidate CTE, and for a
// project holding few of a kind's rows the plan that answers it is an exact read of those rows
// through embeddings_kind_project (migration 20261110155257) and a sort of them, not a walk of
// embeddings_cosine filtered down to them: on the load-test deployment that is 800 of 800
// windows in 13.7 ms against 2 of 800 in 225 ms (INFRA-988). The unscoped statement's own plan
// is the test above, which reads embeddings_cosine for all five arms; one "$2 is empty, or it
// matches" predicate would have to be planned once for all of them and can lose any.
//
// It is also the switch's other side. The statement explained is the one the read path itself
// picks for this project (meaningStatementFor) over the flag it itself reads
// (embeddingProjectKeysFilled), so what the plan says is what a scoped search runs: with the
// fill's row recorded done that is the column's own predicate and this index, and with the
// same row not done it is the joined statement, whose comment candidates cut by the project of
// the comment's own issue or document inside the same limit - the arm the release before this
// change ran, joins and all (searchMeaningCommentArmJoined). A search before the fill finishes
// is answered by the owner each arm joins
// (TestAProjectScopedSearchAnswersWhileTheFillIsStillLabelling); this exact read is
// what finishing the pass buys, and what the joined shape pays for it is the comments row read
// under the limit here.
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

	recordEmbeddingProjectKeyFill(t, database, true)
	root, planJSON := meaningPlanOf(t, database, embedder, meaningStatementAsSearchPicksIt(t, database, "SARM"),
		"SARM", "set local enable_seqscan = off")
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

	if scoped := leg.find(func(node planNode) bool { return node.NodeType == "Limit" }); scoped == nil {
		t.Errorf("the scoped comment arm's candidates are read under no limit:\n%s", planString(t, planJSON))
	} else if owner := scoped.find(func(node planNode) bool { return node.Relation == "comments" }); owner != nil {
		t.Errorf("the scoped comment arm reads comments inside its limit, so the candidate set is spent before the join rather than on the column's own predicate:\n%s",
			planString(t, planJSON))
	}

	// The other side, on the same fixture: with the row back as a running fill leaves it, the
	// statement the read path picks is the arm the release before this change ran, which cuts
	// its candidates by the project of the comment's own issue or document inside that same
	// limit. It pays for that in the joins it takes below the limit, and what it buys is the
	// rows: `e.project_key = $2` loses every row the fill has not labelled yet
	// (searchMeaningCommentArmJoined).
	recordEmbeddingProjectKeyFill(t, database, false)
	joined := meaningStatementAsSearchPicksIt(t, database, "SARM")
	root, planJSON = meaningPlanOf(t, database, embedder, joined, "SARM", "set local enable_seqscan = off")
	leg = root.find(func(node planNode) bool { return node.Alias == "meaning_comment_candidates" })
	if leg == nil {
		t.Fatalf("the plan has no node for meaning_comment_candidates:\n%s", planString(t, planJSON))
	}
	limit := leg.find(func(node planNode) bool { return node.NodeType == "Limit" })
	if limit == nil {
		t.Fatalf("the joined comment arm's candidates are read under no limit:\n%s", planString(t, planJSON))
	}
	if cut := limit.find(func(node planNode) bool {
		return strings.Contains(strings.Join(node.conditions(), " "), "project_key")
	}); cut == nil {
		t.Errorf("while the fill runs the joined comment arm cuts its %d candidates by no project at all, so another project's crowd takes them:\n%s",
			searchWindowCandidates, planString(t, planJSON))
	}
	if owner := limit.find(func(node planNode) bool { return node.Relation == "comments" }); owner == nil {
		t.Errorf("the joined comment arm reads no comments row inside its limit, so the project it cuts its candidates by is not the owner's:\n%s",
			planString(t, planJSON))
	}
}

// meaningStatementAsSearchPicksIt is the meaning statement a scoped search of project runs on
// this database right now: the read path's own choice (meaningStatementFor) over the read
// path's own reading of the fill's progress row (embeddingProjectKeysFilled), in a transaction
// of its own as a search reads it. A plan test explains what a search sends rather than a
// statement named by hand, so the switch between the two scoped shapes is under test with
// them.
//
// Unfiltered, as the search these plans are measured for is: a search narrowing by status,
// author or updated_since takes the joined statement whatever the fill's row says, and its plan
// is the joined one this test explains on the other side of the switch (meaningStatementFor,
// TestAFilteredMeaningSearchFindsTheCommentItsFiltersKeep).
func meaningStatementAsSearchPicksIt(t *testing.T, database *store.Store, project string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction to read the fill's progress in: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return meaningStatementFor(project, embeddingProjectKeysFilled(ctx, tx), false)
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

// planNode is the part of one EXPLAIN (FORMAT JSON) plan node these tests read. A keyword arm's
// match can be written on any of the three condition fields - an index condition, the recheck a
// bitmap heap scan makes, or a filter - and the issue arm's test below asks which, so all three
// are read, with the join filter a node that names no relation of its own carries. Relation is
// the table a scan reads, which is how a test asks what stands inside an arm's own limit
// (TestTheScopedMeaningStatementReadsTheProjectIndex) rather than above it.
type planNode struct {
	NodeType    string     `json:"Node Type"`
	Alias       string     `json:"Alias,omitempty"`
	Relation    string     `json:"Relation Name,omitempty"`
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
