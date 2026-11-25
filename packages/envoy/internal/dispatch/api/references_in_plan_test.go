package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The references read's in statement (refs' linksInQuery) leaves the issue's own nodes out of the
// nodes that link into them by an anti-join, under each plan-cache mode (explaintest's
// PlanCacheModes). Over the large issue of
// TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks (seedLinkedIntoIssue with 40
// documents of its own and 500 that link into them), its plan, read by EXPLAIN (ANALYZE, BUFFERS),
// holds a join node of join type Anti whose condition names the own relation's columns, x.kind and
// x.id, and no node whose filter holds `ROW(...) <> ALL ((InitPlan n).col1)`: a test of each link
// row against an array of the own nodes.
//
// That test costs link rows × own nodes: Postgres does not hash an array an InitPlan reads, so it
// compares each link row with the own nodes in turn, here each of 10,000 link rows with the 2,042
// rows of targets. A bound on the read's time alone cannot fail on a fast machine: Job
// infra5685-checks-qrfjb read main's array form in 931 ms on this seed, inside the route's 2 s
// budget, against 2.01 s in a shared pod. The plan shows which form the statement takes on any
// machine.
//
// The statement is the one the server builds and binds (referencesInStatement). Both plans are
// printed in text form.
//
// Negative control: with main's array put back in oneRowPerNode, `where not (row(f.node_kind,
// f.node_id) = any(array(select row(x.kind, x.id) from ` + own + ` x)))`, the plan holds no
// anti-join, and the Subquery Scan of each of the links' five arms filters by
// `ROW(r_n.node_kind, r_n.node_id) <> ALL ((InitPlan n).col1)` under each mode, so the test fails
// naming that filter.
func TestTheReferencesInStatementLeavesTheIssuesOwnNodesOutByAnAntiJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := seedLinkedIntoIssue(t, 40, 500)
	in := referencesInStatement(t, ctx, seed)
	for _, mode := range explaintest.PlanCacheModes {
		t.Logf("the in statement's plan under %s:\n%s", mode, explainReferencesIn(t, ctx, seed.served, in, mode, "text"))
		var plan inPlanNode
		if err := json.Unmarshal(explaintest.OnePlan(t, []byte(explainReferencesIn(t, ctx, seed.served, in, mode, "json"))), &plan); err != nil {
			t.Fatalf("read the in statement's plan under %s: %v", mode, err)
		}
		antiJoins, arrayFilters := plan.ownNodesLeftOut()
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
func referencesInStatement(t *testing.T, ctx context.Context, seed linkedIntoIssue) explaintest.Statement {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+seed.key+"/references", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	refused := httptest.NewRecorder()
	who, ok := seed.server.actingReader(refused, request)
	if !ok {
		t.Fatalf("resolve the reader of the references request: status=%d body=%s", refused.Code, refused.Body.String())
	}
	tx, err := seed.served.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the references read: %v", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	recorded := &explaintest.RecordedTx{Tx: tx}
	if _, err := refs.Closure(ctx, recorded, who, seed.key, seed.server.deps.ServerURL); err != nil {
		t.Fatalf("read the references of %s: %v", seed.key, err)
	}
	return recorded.Holding(t, "targets (kind, id)")
}

// explainReferencesIn runs EXPLAIN (ANALYZE, BUFFERS, FORMAT format) of statement under the
// plan-cache mode mode, in a transaction of database it rolls back, and answers the plan's lines.
// It explains as explaintest's explainExecuted does, which gives no BUFFERS: a plan-cache mode
// applies to a prepared statement, and an EXPLAIN that binds its own parameters is planned for
// their values whatever the mode says, so it prepares the statement and explains its EXECUTE, each
// argument written as a literal of the type Postgres inferred for its parameter, in the text pgx
// encodes it in.
func explainReferencesIn(t *testing.T, ctx context.Context, database *store.Store, statement explaintest.Statement, mode, format string) string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction the plan is read in: %v", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set plan_cache_mode = %s: %v", mode, err)
	}
	conn := tx.Conn()
	name := "references_in_" + format + "_" + mode
	description, err := conn.Prepare(ctx, name, statement.SQL)
	if err != nil {
		t.Fatalf("prepare the in statement: %v", err)
	}
	defer func() { _ = conn.Deallocate(context.WithoutCancel(ctx), name) }()
	if len(description.ParamOIDs) != len(statement.Args) {
		t.Fatalf("the in statement takes %d parameters and was given %d arguments", len(description.ParamOIDs), len(statement.Args))
	}
	literals := make([]string, len(statement.Args))
	for index, argument := range statement.Args {
		text, err := conn.TypeMap().Encode(description.ParamOIDs[index], pgtype.TextFormatCode, argument, []byte{})
		if err != nil {
			t.Fatalf("write argument %d (%T) as a literal: %v", index+1, argument, err)
		}
		if text == nil {
			literals[index] = "null"
			continue
		}
		literals[index] = "'" + strings.ReplaceAll(string(text), "'", "''") + "'"
	}
	rows, err := tx.Query(ctx, "explain (analyze, buffers, format "+format+") execute "+name+"("+strings.Join(literals, ", ")+")")
	if err != nil {
		t.Fatalf("explain the in statement under %s: %v", mode, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain the in statement under %s: %v", mode, err)
	}
	return strings.Join(lines, "\n")
}

// inPlanNode is a node of the in statement's plan as EXPLAIN (FORMAT JSON) writes it, with what
// the test judges of it: its join type, its join conditions and filters, and the nodes under it.
type inPlanNode struct {
	Type       string       `json:"Node Type"`
	Alias      string       `json:"Alias"`
	JoinType   string       `json:"Join Type"`
	MergeCond  string       `json:"Merge Cond"`
	HashCond   string       `json:"Hash Cond"`
	JoinFilter string       `json:"Join Filter"`
	Filter     string       `json:"Filter"`
	Plans      []inPlanNode `json:"Plans"`
}

// ownKind and ownID match a column of the own nodes as the in statement's anti-join names it, by
// the alias oneRowPerNode gives their relation; arrayOfOwnNodes matches the test of a row against
// the array an InitPlan reads, which is how the statement left the own nodes out before.
var (
	ownKind         = regexp.MustCompile(`\bx\.kind\b`)
	ownID           = regexp.MustCompile(`\bx\.id\b`)
	arrayOfOwnNodes = regexp.MustCompile(`ROW\(.*\) <> ALL \(\(InitPlan \d+\)\.col\d+\)`)
)

// ownNodesLeftOut answers how many nodes of n, n and every node under it, are anti-joins whose
// conditions name both x.kind and x.id, and each filter or join filter among them that tests a row
// against an array an InitPlan reads (arrayOfOwnNodes), quoted with its node.
func (n inPlanNode) ownNodesLeftOut() (antiJoins int, arrayFilters []string) {
	conditions := n.MergeCond + " " + n.HashCond + " " + n.JoinFilter
	if n.JoinType == "Anti" && ownKind.MatchString(conditions) && ownID.MatchString(conditions) {
		antiJoins++
	}
	node := n.Type
	if n.Alias != "" {
		node += " " + n.Alias
	}
	for _, filter := range [...]struct{ label, text string }{{"Filter", n.Filter}, {"Join Filter", n.JoinFilter}} {
		if arrayOfOwnNodes.MatchString(filter.text) {
			arrayFilters = append(arrayFilters, "  "+node+": "+filter.label+": "+filter.text)
		}
	}
	for _, child := range n.Plans {
		childAntiJoins, childFilters := child.ownNodesLeftOut()
		antiJoins += childAntiJoins
		arrayFilters = append(arrayFilters, childFilters...)
	}
	return antiJoins, arrayFilters
}
