package api

import (
	"context"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The references read's in statement (refs' linksInQuery) leaves the issue's own nodes out of the
// nodes that link into them by an anti-join, under each plan-cache mode (explaintest's
// PlanCacheModes). Over the seed of
// TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks (seedLinkedInto), its plan,
// read by EXPLAIN (ANALYZE, BUFFERS), holds a join node of join type Anti whose condition names
// the own relation's columns, x.kind and x.id, and no node whose filter holds
// `ROW(...) <> ALL ((InitPlan n).col1)`: a test of each link row against an array of the own
// nodes.
//
// That test costs link rows × own nodes: Postgres does not hash an array an InitPlan reads, so it
// compares each link row with the own nodes in turn, here each of 10,000 link rows with the 2,042
// rows of targets. A bound on the read's time alone cannot fail on a fast machine, as the times
// oneRowPerNode's comment gives show; the plan shows which form the statement takes on any
// machine.
//
// The statement is the one the server builds and binds (referencesInStatement), explained as the
// planner plans it for the server, with no setting of its own (explaintest.ExplainBuffered). Both
// plans are printed in text form.
//
// Negative control: with the array put back in oneRowPerNode,
// `not (row(f.node_kind, f.node_id) = any(array(...)))` over the in statement's list,
// `select row(t.kind, t.id) from targets t`, the plan holds no anti-join, and the Subquery Scan of
// each of the links' five arms filters by
// `ROW(r_n.node_kind, r_n.node_id) <> ALL ((InitPlan n).col1)` under each mode, so the test fails
// naming that filter.
func TestTheReferencesInStatementLeavesTheIssuesOwnNodesOutByAnAntiJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := seedLinkedInto(t)
	in := referencesInStatement(t, ctx, seed)
	for _, mode := range explaintest.PlanCacheModes {
		t.Logf("the in statement's plan under %s:\n%s", mode, explaintest.ExplainBuffered(t, ctx, seed.store.Pool, in, mode, "text"))
		var antiJoins int
		var arrayFilters []string
		for _, node := range explaintest.Nodes(t, explaintest.ExplainBuffered(t, ctx, seed.store.Pool, in, mode, "json")) {
			conditions := node.HashCond + " " + node.MergeCond + " " + node.JoinFilter
			if node.JoinType == "Anti" && ownKind.MatchString(conditions) && ownID.MatchString(conditions) {
				antiJoins++
			}
			for _, filter := range [...]struct{ label, text string }{{"Filter", node.Filter}, {"Join Filter", node.JoinFilter}} {
				if arrayOfOwnNodes.MatchString(filter.text) {
					arrayFilters = append(arrayFilters, "  "+node.String()+": "+filter.label+": "+filter.text)
				}
			}
		}
		if antiJoins == 0 {
			t.Errorf("under %s the in statement's plan holds no anti-join node whose condition names x.kind and x.id, the own nodes' columns", mode)
		}
		if len(arrayFilters) > 0 {
			t.Errorf("under %s the in statement's plan tests each link row against an array of the own nodes:\n%s",
				mode, strings.Join(arrayFilters, "\n"))
		}
	}
}

// referencesInStatement is the in statement of the references read of seed's issue as the server
// builds and binds it, for the acting reader of the route's test request
// (issueReferencesQueryRequest's X-Dispatch-User: alice) as the server's actingReader resolves
// it: refs.Closure runs on seed's store through explaintest.RecordedTx, in a transaction it rolls
// back, and the in statement is the one statement it sent that holds `targets (kind, id)`.
func referencesInStatement(t *testing.T, ctx context.Context, seed linkedIntoSeed) explaintest.Statement {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+seed.issue.Key+"/references", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	refused := httptest.NewRecorder()
	who, ok := seed.server.readerOf(refused, request)
	if !ok {
		t.Fatalf("resolve the reader of the references request: status=%d body=%s", refused.Code, refused.Body.String())
	}
	tx, err := seed.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the references read: %v", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	recorded := &explaintest.RecordedTx{Tx: tx}
	if _, err := refs.Closure(ctx, recorded, who, seed.issue.Key, text.NewOrigins("")); err != nil {
		t.Fatalf("read the references of %s: %v", seed.issue.Key, err)
	}
	return recorded.Holding(t, "targets (kind, id)")
}

// ownKind and ownID match a column of the own nodes as the in statement's anti-join names it, by
// the alias oneRowPerNode gives their relation; arrayOfOwnNodes matches the test of a row against
// the array an InitPlan reads, which is how the statement left the own nodes out before.
var (
	ownKind         = regexp.MustCompile(`\bx\.kind\b`)
	ownID           = regexp.MustCompile(`\bx\.id\b`)
	arrayOfOwnNodes = regexp.MustCompile(`ROW\(.*\) <> ALL \(\(InitPlan \d+\)\.col\d+\)`)
)
