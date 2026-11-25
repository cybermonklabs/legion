package api

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// What a14 holds the plans to and counts them by: every statement each of its six operations'
// requests sends, as the server sends it (dismissedScaleTrace), the work each read does under both
// plans a connection runs it with (dismissedScaleWork), the index each read the test names must
// read, the table it may not scan and the sort no plan may make, and the negative control - the
// shape this change replaced, counted at the pile's own size and held to being outside the bound.
// What each count is held to is api/inbox_dismissed_scale_test.go's, and the seed they run over
// api/inbox_dismissed_scale_seed_test.go's.
//
// It is a file of its own because a plan is held to different things than the operation is: a
// plan off its index, or a count that does not repeat, is read here, where the statement, its
// index and its plan are, and the file that holds the counts to their bounds stays short enough to
// read in one sitting.

// dismissedScaleExplained is one statement the test names, with what its plan is held to: the
// index it must read, the table it may not scan, and the column no Sort key may name. Each is
// matched by its text to a statement the operation's request sent (countDismissedScaleOperation),
// and a named statement the request did not send fails the row, so this list cannot fall behind
// the routes. An empty noSortOn holds the plan to no Sort node at all, which is the rule
// replies_plan_test.go holds every Reply-half read to; an Agent-half read carries sorts of the
// list's own that are older than the dismissal - the roots' newest reply, the lateral that reads
// it, the ordered array_agg - so what it is held to is that no sort stands in for the dismissal,
// named by its column. The two write routes read their rows and then sort them by thread id to
// take their locks in one order (markInboxReadThreads), which is the route's own sort and not the
// index's, so each is held the same way, by the column its state is ordered by.
type dismissedScaleExplained struct {
	operation string
	half      string
	sql       string
	index     string
	// relation is the table the plan may not scan, empty for a read whose cheapest way to take a
	// whole set at this seed is a scan of it, and onceOver a table the plan must read once rather
	// than once per row: the Inbox's own three reads take the person's dismissal marks as one set,
	// and whether the planner takes that set off the index or off the table is its own business,
	// while taking it one root at a time is what made those reads grow with the pile.
	relation string
	noSortOn string
	onceOver string
}

// dismissedScaleCount is one count of the work a14's statements did, by Postgres's own count
// (speedWork, api/priority_speed_test.go): the rows every node of each plan produced or read and
// threw away, each times the node's runs, and the shared buffers each statement accessed, each
// access one read of one page of a table or an index, cached or not. Neither is a time, so neither
// moves with the box's load.
type dismissedScaleCount struct {
	rows    float64
	buffers float64
}

func (c *dismissedScaleCount) add(work speedWork) {
	c.rows += work.rows
	c.buffers += work.buffers
}

func (c dismissedScaleCount) String() string {
	return fmt.Sprintf("%.0f rows and %.0f buffer accesses", c.rows, c.buffers)
}

// dismissedScaleWork is what one operation's request sent on one shape and what it cost. custom
// and generic are its reads' work under each of the two plans a connection runs a statement with
// (speedCustomPlans, api/priority_speed_test.go): the plan made for the statement's own arguments,
// which a connection runs for its first five runs of it, and the generic plan, which it may run
// from then on. statements is every statement the request sent, and effects the rows its other
// statements - writes, row locks, and the transaction's own control - reported changing or
// locking: those cannot be counted under EXPLAIN ANALYZE, which would run them, so a route that
// sends one more of them for each row of the pile, or one that touches the pile, shows in these
// two instead.
type dismissedScaleWork struct {
	custom     dismissedScaleCount
	generic    dismissedScaleCount
	statements int
	effects    int64
}

// dismissedScaleSent is one statement a request sent, as the server's connection sent it: its
// text, its arguments, and the rows its command tag reported (rows answered for a read, changed or
// locked for anything else).
type dismissedScaleSent struct {
	sql  string
	args []any
	rows int64
	err  error
}

// dismissedScaleTrace records the statements a shape's server sends on a request marked
// dismissedScaleTraced, and nothing else its pool runs: the test's own seeding and counting, and
// the server's background work, carry no mark. It sees what the routes send whatever path they
// send it by - a statement on its own, a transaction's begin and commit, each statement of a batch
// (pgx.BatchTracer) and a copy (pgx.CopyFromTracer) - so a statement no test names is still
// counted. It is installed on every connection of the shape's server (testServerOptions.tracer,
// storetest.Traced).
type dismissedScaleTrace struct {
	mu   sync.Mutex
	sent []*dismissedScaleSent
}

type dismissedScaleTraced struct{}

type dismissedScaleSentKey struct{}

// take answers every statement recorded since the last take, in the order each was sent.
func (d *dismissedScaleTrace) take() []dismissedScaleSent {
	d.mu.Lock()
	defer d.mu.Unlock()
	taken := make([]dismissedScaleSent, 0, len(d.sent))
	for _, sent := range d.sent {
		taken = append(taken, *sent)
	}
	d.sent = nil
	return taken
}

func (d *dismissedScaleTrace) start(ctx context.Context, sql string, args []any) context.Context {
	if ctx.Value(dismissedScaleTraced{}) == nil {
		return ctx
	}
	sent := &dismissedScaleSent{sql: sql, args: dismissedScaleArgs(args)}
	d.mu.Lock()
	d.sent = append(d.sent, sent)
	d.mu.Unlock()
	return context.WithValue(ctx, dismissedScaleSentKey{}, sent)
}

func (d *dismissedScaleTrace) end(ctx context.Context, tag pgconn.CommandTag, err error) {
	sent, ok := ctx.Value(dismissedScaleSentKey{}).(*dismissedScaleSent)
	if !ok {
		return
	}
	d.mu.Lock()
	sent.rows, sent.err = tag.RowsAffected(), err
	d.mu.Unlock()
}

func (d *dismissedScaleTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return d.start(ctx, data.SQL, data.Args)
}

func (d *dismissedScaleTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	d.end(ctx, data.CommandTag, data.Err)
}

func (d *dismissedScaleTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (d *dismissedScaleTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	d.end(d.start(ctx, data.SQL, data.Args), data.CommandTag, data.Err)
}

func (d *dismissedScaleTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (d *dismissedScaleTrace) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	return d.start(ctx, "copy "+data.TableName.Sanitize()+" from stdin", nil)
}

func (d *dismissedScaleTrace) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	d.end(ctx, data.CommandTag, data.Err)
}

// dismissedScaleArgs is a statement's arguments without the pgx options a Query or Exec may lead
// them with (a query exec mode, result formats), which pgx reads and does not send.
func dismissedScaleArgs(args []any) []any {
	for len(args) > 0 {
		switch args[0].(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
			args = args[1:]
			continue
		}
		break
	}
	return args
}

// The kinds a sent statement is counted as. A read is run under EXPLAIN (ANALYZE, BUFFERS) and
// its work is counted. A statement that changes, locks or signals - an insert, update, delete or
// merge, a CTE holding one, a `for update`/`for share` row lock, an advisory lock (taken directly
// or through public.owner_lock, which the store's owner locks call), a notify or a sequence - and
// the transaction's own control (begin, commit, set) are counted in statements and effects alone,
// since EXPLAIN ANALYZE would run the change. Any other statement stops the row by name: what it
// costs cannot be read here, so it is not left uncounted in silence.
const (
	dismissedScaleRead   = "read"
	dismissedScaleEffect = "effect"
)

var (
	dismissedScaleComment  = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
	dismissedScaleChanging = regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from|merge\s+into)\b`)
	dismissedScaleLocking  = regexp.MustCompile(`(?is)\bfor\s+(no\s+key\s+update|update|key\s+share|share)\b|\bpg_(try_)?advisory_x?act_lock|\bpg_(try_)?advisory_lock|\bpg_notify\b|\bnextval\s*\(|\bowner_lock\s*\(`)
)

func dismissedScaleKind(sql string) (string, bool) {
	text := strings.TrimSpace(dismissedScaleComment.ReplaceAllString(sql, " "))
	first := strings.ToLower(strings.Fields(text + " ")[0])
	switch first {
	case "begin", "commit", "rollback", "set", "savepoint", "release":
		return dismissedScaleEffect, true
	case "insert", "update", "delete", "merge", "copy", "notify":
		return dismissedScaleEffect, true
	case "select", "with", "values":
		if dismissedScaleChanging.MatchString(text) || dismissedScaleLocking.MatchString(text) {
			return dismissedScaleEffect, true
		}
		return dismissedScaleRead, true
	}
	return "", false
}

// explainDismissedScale counts and explains every statement one operation's request sent
// (dismissedScaleTrace) on one shape. Each read is run again under EXPLAIN (ANALYZE, BUFFERS) with
// the arguments it was sent, and enable_seqscan and enable_bitmapscan left on, so the plan is what
// the planner really does at this size: counted under both its plans (countDismissedScale), and
// held to answering the rows the route read. The statements the test names (dismissedScaleStatements)
// are found among them by their text, and each one the request did not send fails the row. On the
// loaded shape each named read's plan made for its arguments is also held to its index and its
// sort rule; on the baselines those are printed and held to nothing, since a scan of an empty table
// is the right plan for an empty table.
func explainDismissedScale(
	t *testing.T, shape *dismissedScaleShape, operation string, sent []dismissedScaleSent, hold bool,
) dismissedScaleWork {
	t.Helper()
	ctx := context.Background()
	connection, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	named := map[string]dismissedScaleExplained{}
	for _, explained := range dismissedScaleStatements() {
		if explained.operation == operation {
			named[explained.sql] = explained
		}
	}
	found := map[string]bool{}
	work := dismissedScaleWork{statements: len(sent)}
	for index, statement := range sent {
		name := fmt.Sprintf("%s / statement %d @ %s", operation, index+1, shape.name)
		explained, isNamed := named[statement.sql]
		if isNamed {
			found[statement.sql] = true
			name = operation + " / " + explained.half + " @ " + shape.name
		}
		if statement.err != nil {
			t.Fatalf("%s failed in the route: %v\n%s", name, statement.err, statement.sql)
		}
		kind, known := dismissedScaleKind(statement.sql)
		if !known {
			t.Fatalf("%s is a statement this row can neither count as a read nor as a change, so its cost would go uncounted:\n%s", name, statement.sql)
		}
		if kind == dismissedScaleEffect {
			work.effects += statement.rows
			t.Logf("%s, not a read: %d rows changed or locked: %s", name, statement.rows, dismissedScaleFirstLine(statement.sql))
			continue
		}
		custom, plan := countDismissedScale(t, connection.Conn(), name, statement.sql, statement.args, false)
		generic, genericPlan := countDismissedScale(t, connection.Conn(), name+", generic plan", statement.sql, statement.args, true)
		for _, counted := range []speedWork{custom, generic} {
			if int64(counted.answered) != statement.rows {
				t.Fatalf("%s answered %.0f rows counted and %d rows in the route: the count did not read what the route read, so nothing was measured\n%s",
					counted.name, counted.answered, statement.rows, statement.sql)
			}
		}
		work.custom.add(custom)
		work.generic.add(generic)
		text := explainDismissedScaleText(t, connection.Conn(), name, statement.sql, statement.args)
		t.Logf("%s; under the generic plan %.0f rows and %.0f buffer accesses (%s):\n%s",
			custom, generic.rows, generic.buffers, dismissedScalePlanNodes(t, genericPlan), text)
		if !hold || !isNamed {
			continue
		}
		if explained.relation != "" && explaintest.SeqScansRelation(t, plan, explained.relation) {
			t.Errorf("%s scans %s:\n%s", name, explained.relation, text)
		}
		if !planReadsIndex(t, plan, explained.index) {
			t.Errorf("%s does not read %s:\n%s", name, explained.index, text)
		}
		if explained.onceOver != "" {
			if loops := planLoopsOver(t, plan, explained.onceOver); loops > 1 {
				t.Errorf("%s reads %s %.0f times over, want once: the Inbox's own reads take the person's marks as one set rather than one read a root, which is what stops them growing with the pile:\n%s",
					name, explained.onceOver, loops, text)
			}
		}
		for _, key := range planSortKeys(t, plan) {
			if explained.noSortOn == "" {
				t.Errorf("%s sorts on %s, which its index already answers in:\n%s", name, key, text)
				continue
			}
			if strings.Contains(key, explained.noSortOn) {
				t.Errorf("%s sorts on %s, which %s already answers in:\n%s", name, key, explained.index, text)
			}
		}
	}
	for _, explained := range dismissedScaleStatements() {
		if explained.operation == operation && !found[explained.sql] {
			t.Errorf("%s / %s @ %s: the request did not send the statement the test names, so its plan is held to nothing", operation, explained.half, shape.name)
		}
	}
	return work
}

// dismissedScalePlanNodes names every node of a plan, top down, with the relation or index it
// reads: how a reader sees which plan a count was of without its whole text.
func dismissedScalePlanNodes(t *testing.T, planJSON json.RawMessage) string {
	t.Helper()
	var top analyzedNode
	if err := json.Unmarshal(planJSON, &top); err != nil {
		t.Fatalf("decode a plan: %v", err)
	}
	names := []string{}
	for _, node := range top.everyNode() {
		names = append(names, node.name())
	}
	return strings.Join(names, " > ")
}

func dismissedScaleFirstLine(sql string) string {
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// planLoopsOver is the greatest number of times any one node of an analysed plan read relation:
// one for a table the read visits once, and a row count for one it visits per row.
func planLoopsOver(t *testing.T, planJSON json.RawMessage, relation string) float64 {
	t.Helper()
	var node struct {
		Relation string            `json:"Relation Name"`
		Loops    float64           `json:"Actual Loops"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	most := float64(0)
	if node.Relation == relation {
		most = node.Loops
	}
	for _, child := range node.Plans {
		if loops := planLoopsOver(t, child, relation); loops > most {
			most = loops
		}
	}
	return most
}

// countDismissedScale counts one statement's work under one of its two plans (speedWork) on one
// connection, as countSpeedPage counts a page of the listing (api/priority_speed_test.go): once to
// warm the connection, whose first run of a statement can read catalog pages a second does not,
// then dismissedScaleCounts times to be counted. Each run is inside a transaction it rolls back.
// The generic plan is the one plan_cache_mode = force_generic_plan makes a prepared statement run
// from its first execution, the plan a connection's later runs may get (speedCustomPlans); its
// arguments are written as literals (dismissedScaleLiterals), since an EXECUTE under EXPLAIN takes
// no bound parameter.
//
// Each number is the least of the counts. A count can differ from the one before it by a few
// accesses, where an earlier run left a page in another state: a read of every row the person
// holds, under the generic plan, counted 3,520 and then 3,530 accesses (2026-11-22). Such a
// statement is printed with every count. A plan that changed between counts, or that has no
// node, is one this read could not see into, so the test stops on either rather than hold an
// operation to its bound. A statement over an empty table can access no buffer at all - the
// Dismissed view's Agent page with no dismissal reads none - so a count of nothing is held where
// it is compared, against a baseline or at the pile (TestDismissedScaleHoldsTheInboxsReads). It
// answers the counted plan too, which the plan checks read.
func countDismissedScale(
	t *testing.T, connection *pgx.Conn, name, statement string, args []any, generic bool,
) (speedWork, json.RawMessage) {
	t.Helper()
	ctx := context.Background()
	explain, bound := "explain (analyze, buffers, format json) "+statement, args
	if generic {
		if _, err := connection.Exec(ctx, "set plan_cache_mode = force_generic_plan"); err != nil {
			t.Fatalf("force the generic plan for %s: %v", name, err)
		}
		described, err := connection.Prepare(ctx, "dismissed_scale", statement)
		if err != nil {
			t.Fatalf("prepare %s: %v", name, err)
		}
		defer func() {
			if err := connection.Deallocate(ctx, "dismissed_scale"); err != nil {
				t.Errorf("deallocate after counting %s: %v", name, err)
			}
			if _, err := connection.Exec(ctx, "reset plan_cache_mode"); err != nil {
				t.Errorf("reset plan_cache_mode after counting %s: %v", name, err)
			}
		}()
		explain = "explain (analyze, buffers, format json) execute dismissed_scale" +
			dismissedScaleLiterals(t, connection, name, described.ParamOIDs, args)
		bound = nil
	}
	count := func() (speedWork, json.RawMessage) {
		tx, err := connection.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the count of %s: %v", name, err)
		}
		defer tx.Rollback(ctx)
		var raw []byte
		if err := tx.QueryRow(ctx, explain, bound...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		var plans []struct {
			Plan analyzedNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("read the plan of %s: %d plans, %v", name, len(plans), err)
		}
		top := plans[0].Plan
		work := speedWork{name: name, answered: top.Rows * max(top.Loops, 1), buffers: top.SharedHitBlocks + top.SharedReadBlocks}
		for _, node := range top.everyNode() {
			work.nodes++
			if node.NodeType == "Recursive Union" {
				work.walks++
			}
			work.rows += node.Loops * (node.Rows + node.RemovedByFilter + node.RemovedByJoin + node.RemovedByRecheck)
		}
		return work, explaintest.OnePlan(t, raw)
	}
	count()
	least, plan := count()
	counts := []string{fmt.Sprintf("%.0f rows and %.0f buffer accesses", least.rows, least.buffers)}
	differ := false
	for range dismissedScaleCounts - 1 {
		next, _ := count()
		if next.nodes != least.nodes || next.walks != least.walks || next.answered != least.answered {
			t.Fatalf("%s ran %d plan nodes answering %.0f rows, then %d answering %.0f: its plan changed between counts, so nothing was measured",
				name, least.nodes, least.answered, next.nodes, next.answered)
		}
		counts = append(counts, fmt.Sprintf("%.0f rows and %.0f buffer accesses", next.rows, next.buffers))
		differ = differ || next.rows != least.rows || next.buffers != least.buffers
		least.rows, least.buffers = min(least.rows, next.rows), min(least.buffers, next.buffers)
	}
	if differ {
		t.Logf("%s counted %s; the least of each is held", name, strings.Join(counts, ", then "))
	}
	if least.nodes == 0 {
		t.Fatalf("%s counted no plan node: the plan was not read, so nothing was measured", name)
	}
	return least, plan
}

// dismissedScaleCounts is how many times each statement is counted after its warm-up run.
const dismissedScaleCounts = 3

// dismissedScaleLiterals writes a statement's arguments as the literals an EXECUTE names: each
// value encoded as text by the connection's own types for the parameter type Postgres described,
// quoted, and cast to that type, so any value a route binds is written as it was sent.
func dismissedScaleLiterals(t *testing.T, connection *pgx.Conn, name string, oids []uint32, args []any) string {
	t.Helper()
	if len(oids) != len(args) {
		t.Fatalf("%s takes %d parameters and was sent %d arguments", name, len(oids), len(args))
	}
	if len(args) == 0 {
		return ""
	}
	types := connection.TypeMap()
	literals := make([]string, 0, len(args))
	for index, argument := range args {
		// Encode answers nil for a NULL and appends to the buffer it is given for anything else,
		// so a non-nil empty buffer keeps an empty string apart from a NULL.
		encoded, err := types.Encode(oids[index], pgx.TextFormatCode, argument, make([]byte, 0, 64))
		if err != nil {
			t.Fatalf("write argument %d of %s (%T) as text: %v", index+1, name, argument, err)
		}
		typeName := "unknown"
		if known, ok := types.TypeForOID(oids[index]); ok {
			typeName = known.Name
		} else {
			t.Fatalf("argument %d of %s has a type (oid %d) this connection does not know", index+1, name, oids[index])
		}
		if encoded == nil {
			literals = append(literals, "null::"+typeName)
			continue
		}
		literals = append(literals, "'"+strings.ReplaceAll(string(encoded), "'", "''")+"'::"+typeName)
	}
	return "(" + strings.Join(literals, ", ") + ")"
}

// explainDismissedScaleText is one statement's plan made for its arguments, run under EXPLAIN
// (ANALYZE, BUFFERS) as text, which the report prints, inside a transaction it rolls back.
func explainDismissedScaleText(t *testing.T, connection *pgx.Conn, name, statement string, args []any) string {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain of %s: %v", name, err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "explain (analyze, buffers) "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	defer rows.Close()
	lines := []string{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan a plan line of %s: %v", name, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	return strings.Join(lines, "\n")
}

// dismissedScaleStatements is the statements the test names, with the index each is held to: the
// list route's two halves per state (api/replies.go and api/replies_agent.go), the dismissal's
// read of the rows it was given (api/inbox_dismiss.go) and the mark-all's two reads
// (api/inbox_read.go). The indexes are the ones replies_plan_test.go pins, which is where the
// states' own reads are held to them. Every other statement a request sends is counted too
// (explainDismissedScale); these are the ones whose plans are held to an index.
func dismissedScaleStatements() []dismissedScaleExplained {
	return []dismissedScaleExplained{
		{"state=inbox", "reply", myRepliesInboxPageQuery, "user_threads_inbox", "user_threads", "", ""},
		// The Inbox's own three reads take the person's dismissal marks as one set, so each is
		// held to reading that table once rather than to an access path of its own.
		{"state=inbox", "agent", myAgentInboxRowsQuery,
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"unread count", "reply count", myRepliesCountQuery, "user_threads_unread", "user_threads", "", ""},
		{"unread count", "reply page", myRepliesPageQuery, "user_threads_unread", "user_threads", "", ""},
		{"unread count", "agent page and total", myAgentCountedRowsQuery,
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"dismissed page 1", "reply page", myRepliesDismissedPageQuery,
			"user_threads_dismissed", "user_threads", "", ""},
		{"dismissed page 1", "agent page", myAgentDismissedRowsQuery,
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		{"dismissed page 50,000 deep", "reply page", myRepliesDismissedFromQuery,
			"user_threads_dismissed", "user_threads", "", ""},
		{"dismissed page 50,000 deep", "agent page", myAgentDismissedFromQuery,
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		// The two write routes sort the rows they matched by thread id, which is the lock order
		// markInboxReadThreads writes down and not an order any index owes them, so what each is
		// held to is that nothing sorts by the state's own order. The dismissal's read of the
		// rows a request named binds the ids and the viewer alone and answers each row's own
		// version: what the page drew in them is the request's, written over that answer in the
		// server rather than sent to this read (readInboxDismissThreads). Each Reply read binds
		// the viewer last, as the list's does (inboxReplyMatchQuery); the Agent read is the shape
		// of a reader that sees every object, as the list's Agent reads above are.
		{"120-thread dismiss", "reply rows", inboxDismissNamedThreads,
			"user_threads_pkey", "user_threads", "replied_at", ""},
		{"mark all read", "reply rows", inboxReadReplyThreads, "user_threads_unread", "user_threads", "replied_at", ""},
		{"mark all read", "agent rows", inboxReadAgentThreads.every, "messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
	}
}

// dismissedScaleControlAgentCount is the shape this change replaced, kept here and nowhere else:
// the Dismissed view's Agent count read over one recursive CTE of every one-off message the person
// ever sent, with one lateral per dismissed root scanning that whole CTE again. It is a14's
// negative control - put the per-root Agent count back and the Dismissed pages go far above their
// bound - so it is spelled out rather than built from the live constants, which no longer hold it.
const dismissedScaleControlAgentCount = `
	with recursive` + unreadDirectRepliesCTE + `,
	one_off_replies as (
		select dt.root_id, dt.session_id, dt.id as reply_id, dt.created_at, dt.author
		from direct_thread dt
		where dt.author->>'kind' = 'session'
		  and not exists (
			select 1 from message_deliveries d
			where d.message_id = dt.root_id and d.accepted_as = 'user_turn'
		  )
	),
	dismissed_roots as (
		select d.message_id as root_id, d.dismissed_at
		from user_agent_thread_dismissed d
		where d.login = $1
		order by d.dismissed_at desc, d.message_id desc
	),
	roots as (
		select dr.root_id, newest.session_id, newest.replied_at, dr.dismissed_at
		from dismissed_roots dr
		join lateral (
			select o.session_id, max(o.created_at) as replied_at
			from one_off_replies o
			where o.root_id = dr.root_id
			group by o.session_id
		) newest on true
		where dr.dismissed_at >= newest.replied_at
	)
	select count(*) from roots`

// dismissedScaleNegativeControl counts that control at the pile's own size and holds it to failing
// the bound the first Dismissed page is held to (dismissedScaleBounds): the page at the pile with
// the read it would have to make before it could answer added to its own work, against the same
// page at one page dismissed, under the plan made for its arguments. A control that came in inside
// the bound would mean these counts cannot tell the shape this change replaced from the one it
// holds, and so prove nothing about the one they passed.
func dismissedScaleNegativeControl(
	t *testing.T, shape *dismissedScaleShape, page, baseline dismissedScaleCount, held dismissedScaleHeld,
) {
	t.Helper()
	ctx := context.Background()
	connection, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the negative control: %v", err)
	}
	defer connection.Release()
	control, _ := countDismissedScale(t, connection.Conn(),
		"negative control (the per-root Agent count over the recursive thread query) @ "+shape.name,
		dismissedScaleControlAgentCount, []any{dismissedScaleLogin, dismissedScaleLogin, nil}, false)
	with := page
	with.add(control)
	rows, buffers := with.rows/baseline.rows, with.buffers/baseline.buffers
	t.Logf("%s; the first Dismissed page with it is %s, %.2fx the rows and %.2fx the buffer accesses of the page at %s (%s), held to %s",
		control, with, rows, buffers, held.baselineShape, baseline, held.bound())
	if !dismissedScaleOver(rows, held.rows) && !dismissedScaleOver(buffers, held.buffers) {
		t.Errorf("the control's page is %.2fx the rows and %.2fx the buffer accesses of its baseline, inside its bound (%s): the counts cannot tell the shape it replaced from the one it holds, so the bounds above prove nothing",
			rows, buffers, held.bound())
	}
}
