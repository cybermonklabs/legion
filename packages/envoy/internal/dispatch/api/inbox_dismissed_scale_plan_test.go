package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// What a14 holds the plans to and counts them by: every statement its six operations run, the
// index each must read, the table each may not scan and the sort no plan may make, the work each
// does under both plans a connection runs it with (dismissedScaleWork), and the negative control -
// the shape this change replaced, counted at the pile's own size and held to being outside the
// bound. What each count is held to is api/inbox_dismissed_scale_test.go's, and the seed they run
// over api/inbox_dismissed_scale_seed_test.go's.
//
// It is a file of its own because a plan is held to different things than the operation is: a
// plan off its index, or a count that does not repeat, is read here, where the statement, its
// index and its plan are, and the file that holds the counts to their bounds stays short enough to
// read in one sitting.

// dismissedScaleExplained is one statement an operation runs, with what its plan is held to: the
// index it must read, the table it may not scan, and the column no Sort key may name. An empty
// noSortOn holds the plan to no Sort node at all, which is the rule replies_plan_test.go holds
// every Reply-half read to; an Agent-half read carries sorts of the list's own that are older
// than the dismissal - the roots' newest reply, the lateral that reads it, the ordered array_agg -
// so what it is held to is that no sort stands in for the dismissal, named by its column. The two
// write routes read their rows and then sort them by thread id to take their locks in one order
// (markInboxReadThreads), which is the route's own sort and not the index's, so each is held the
// same way, by the column its state is ordered by.
type dismissedScaleExplained struct {
	operation string
	half      string
	sql       string
	args      []any
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

// dismissedScaleWork is what one operation's statements did on one shape under each of the two
// plans a connection runs a statement with (speedCustomPlans, api/priority_speed_test.go): the plan
// made for the statement's own arguments, which a connection runs for its first five runs of it,
// and the generic plan, which it may run from then on. Each count is the sum over the operation's
// statements.
type dismissedScaleWork struct {
	custom  dismissedScaleCount
	generic dismissedScaleCount
}

// explainDismissedScaleReads counts and explains every statement the six operations run, under
// EXPLAIN (ANALYZE, BUFFERS) with enable_seqscan and enable_bitmapscan left on, so the plans are
// what the planner really does at this size rather than what a pin makes it do. Each statement is
// counted under both its plans (countDismissedScale), and the counts are summed by operation, which
// is what the caller holds to the bounds. On the loaded shape each plan made for its statement's
// arguments is also held to its index and its sort rule; on the baselines those are printed and
// held to nothing, since a scan of an empty table is the right plan for an empty table.
func explainDismissedScaleReads(t *testing.T, shape *dismissedScaleShape, hold bool) map[string]dismissedScaleWork {
	t.Helper()
	ctx := context.Background()
	connection, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	work := map[string]dismissedScaleWork{}
	for _, explained := range dismissedScaleStatements(shape) {
		name := explained.operation + " / " + explained.half + " @ " + shape.name
		custom, plan := countDismissedScale(t, connection.Conn(), name, explained.sql, explained.args, false)
		generic, _ := countDismissedScale(t, connection.Conn(), name+", generic plan", explained.sql, explained.args, true)
		counted := work[explained.operation]
		counted.custom.add(custom)
		counted.generic.add(generic)
		work[explained.operation] = counted
		text := explainDismissedScaleText(t, connection.Conn(), name, explained.sql, explained.args)
		t.Logf("%s; under the generic plan %.0f rows and %.0f buffer accesses:\n%s", custom, generic.rows, generic.buffers, text)
		if !hold {
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
	return work
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
// then dismissedScaleCounts times to be counted. Each run is inside a transaction it rolls back,
// so a write route's read takes no lock past the count. The generic plan is the one
// plan_cache_mode = force_generic_plan makes a prepared statement run from its first execution,
// the plan a connection's later runs may get (speedCustomPlans).
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
		for _, setup := range []string{"set plan_cache_mode = force_generic_plan", "prepare dismissed_scale as " + statement} {
			if _, err := connection.Exec(ctx, setup); err != nil {
				t.Fatalf("prepare %s: %v", name, err)
			}
		}
		defer func() {
			for _, teardown := range []string{"deallocate dismissed_scale", "reset plan_cache_mode"} {
				if _, err := connection.Exec(ctx, teardown); err != nil {
					t.Errorf("%s after counting %s: %v", teardown, name, err)
				}
			}
		}()
		explain = "explain (analyze, buffers, format json) execute dismissed_scale(" + dismissedScaleLiterals(t, args) + ")"
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
		work := speedWork{name: name, answered: top.Rows, buffers: top.SharedHitBlocks + top.SharedReadBlocks}
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

// dismissedScaleLiterals writes a statement's arguments as the literals an EXECUTE names, since an
// EXECUTE under EXPLAIN takes no bound parameter: every value dismissedScaleStatements binds is a
// null, a string, a flag, a page size or a list of ids, and any other stops the test rather than
// be written wrong.
func dismissedScaleLiterals(t *testing.T, args []any) string {
	t.Helper()
	literals := make([]string, 0, len(args))
	for _, argument := range args {
		switch value := argument.(type) {
		case nil:
			literals = append(literals, "null")
		case string:
			literals = append(literals, "'"+strings.ReplaceAll(value, "'", "''")+"'")
		case bool:
			literals = append(literals, strconv.FormatBool(value))
		case *int:
			if value == nil {
				literals = append(literals, "null")
				continue
			}
			literals = append(literals, strconv.Itoa(*value))
		case []string:
			for _, id := range value {
				if id == "" || strings.ContainsAny(id, `{},"'\ `) {
					t.Fatalf("write %q in a list literal: only plain ids are written", id)
				}
			}
			literals = append(literals, "'{"+strings.Join(value, ",")+"}'")
		default:
			t.Fatalf("write a %T as a literal: no literal is written for it", argument)
		}
	}
	return strings.Join(literals, ", ")
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

// dismissedScaleStatements is every statement the six operations run, with the index each is held
// to: the list route's two halves per state (api/replies.go and api/replies_agent.go), the
// dismissal's read of the rows it was given (api/inbox_dismiss.go) and the mark-all's two reads
// (api/inbox_read.go). The indexes are the ones replies_plan_test.go pins, which is where the
// states' own reads are held to them.
func dismissedScaleStatements(shape *dismissedScaleShape) []dismissedScaleExplained {
	var everyRow *int
	page := myRepliesDismissedPage
	one := 1
	replyArgs := func(extra ...any) []any {
		return append([]any{dismissedScaleLogin, "", nil}, extra...)
	}
	agentArgs := func(extra ...any) []any {
		return append([]any{dismissedScaleLogin, dismissedScaleLogin, nil}, extra...)
	}
	cursorAt, cursorThread, _ := strings.Cut(shape.deepCursor, ",")
	mark := time.Now().UTC().Format(time.RFC3339Nano)
	statements := []dismissedScaleExplained{
		{"state=inbox", "reply", myRepliesInboxPageQuery, replyArgs(everyRow), "user_threads_inbox", "user_threads", "", ""},
		// The Inbox's own three reads take the person's dismissal marks as one set, so each is
		// held to reading that table once rather than to an access path of its own.
		{"state=inbox", "agent", myAgentInboxRowsQuery, agentArgs(everyRow),
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"unread count", "reply count", myRepliesCountQuery, replyArgs(), "user_threads_unread", "user_threads", "", ""},
		{"unread count", "reply page", myRepliesPageQuery, replyArgs(&one), "user_threads_unread", "user_threads", "", ""},
		{"unread count", "agent page and total", myAgentCountedRowsQuery, agentArgs(&one),
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"dismissed page 1", "reply page", myRepliesDismissedPageQuery, replyArgs(&page),
			"user_threads_dismissed", "user_threads", "", ""},
		{"dismissed page 1", "agent page", myAgentDismissedRowsQuery, agentArgs(&page),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		// The two write routes sort the rows they matched by thread id, which is the lock order
		// markInboxReadThreads writes down and not an order any index owes them, so what each is
		// held to is that nothing sorts by the state's own order. The dismissal's read of the
		// rows a request named binds the ids alone and answers each row's own version: what the
		// page drew in them is the request's, written over that answer in the server rather
		// than sent to this read (readInboxDismissThreads).
		{"120-thread dismiss", "reply rows", inboxDismissNamedThreads,
			[]any{dismissedScaleLogin, shape.dismissRows},
			"user_threads_pkey", "user_threads", "replied_at", ""},
		{"mark all read", "reply rows", inboxReadReplyThreads,
			[]any{dismissedScaleLogin, "", nil, mark, nil, true, nil}, "user_threads_unread", "user_threads", "replied_at", ""},
		{"mark all read", "agent rows", inboxReadAgentThreads,
			append(agentArgs(), mark), "messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
	}
	if shape.deepCursor != "" {
		statements = append(statements,
			dismissedScaleExplained{"dismissed page 50,000 deep", "reply page", myRepliesDismissedFromQuery,
				replyArgs(&page, cursorAt, cursorThread), "user_threads_dismissed", "user_threads", "", ""},
			dismissedScaleExplained{"dismissed page 50,000 deep", "agent page", myAgentDismissedFromQuery,
				agentArgs(&page, cursorAt, cursorThread),
				"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		)
	}
	return statements
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
