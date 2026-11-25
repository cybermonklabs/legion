package explaintest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// PlanCacheModes are the two plans Postgres holds a prepared statement to, as plan_cache_mode
// names them: the custom plan, built for one execution's own values, and the generic plan a
// cached statement settles on from its sixth execution, built without them. pgx prepares every
// statement it sends, so a statement whose plan holds only while the planner sees its values
// loses it in production, and either mode alone hides that.
var PlanCacheModes = []string{"force_custom_plan", "force_generic_plan"}

// NestedLoopsOnly is what a batch statement's plan is read under (ExplainAfter). With the two
// join strategies that read a whole side once turned off, a join of a batch to a table is a
// nested loop, and the plan says which side it reads again for each row of the other, which is
// what UnprobedReads judges.
var NestedLoopsOnly = []string{"set local enable_hashjoin = off", "set local enable_mergejoin = off"}

// Statement is one statement as its writer sent it: its text and the arguments it bound.
type Statement struct {
	SQL  string
	Args []any
}

// Beginner opens the transaction a plan is read in: a pool, or one connection of it.
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// batchRecorder is the transaction a writer's batch is sent to by BatchedBy: it keeps every
// statement of each batch with the arguments the writer bound, in the order the batch holds
// them, and runs none of them.
type batchRecorder struct {
	pgx.Tx
	sent []Statement
}

func (r *batchRecorder) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		r.sent = append(r.sent, Statement{SQL: query.SQL, Args: query.Arguments})
	}
	return unsentBatch{}
}

// unsentBatch answers the one call a sender of a batch makes of its results once it has queued
// everything, its Close, for a batch nothing ran.
type unsentBatch struct{ pgx.BatchResults }

func (unsentBatch) Close() error { return nil }

// BatchedBy is every statement send sends as a batch to the transaction it is handed, in the
// order it sent them, none of them run: a plan is then read off the statement the writer built,
// with the arguments it bound, rather than off a copy the test keeps. send is the writer's own
// call, for a store writer `store.RunWrite(ctx, tx, write)`.
func BatchedBy(t testing.TB, send func(pgx.Tx) error) []Statement {
	t.Helper()
	recorder := &batchRecorder{}
	if err := send(recorder); err != nil {
		t.Fatalf("queue the write: %v", err)
	}
	return recorder.sent
}

// RecordedTx is a transaction that keeps every statement sent through it by Query, QueryRow or
// Exec, with the arguments it bound, and runs each on the transaction it wraps: a reader or a
// writer handed it answers as it would, and the test reads the plan of the statement it sent.
type RecordedTx struct {
	pgx.Tx
	Sent []Statement
}

func (r *RecordedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.Sent = append(r.Sent, Statement{SQL: sql, Args: args})
	return r.Tx.Query(ctx, sql, args...)
}

func (r *RecordedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.Sent = append(r.Sent, Statement{SQL: sql, Args: args})
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *RecordedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.Sent = append(r.Sent, Statement{SQL: sql, Args: args})
	return r.Tx.Exec(ctx, sql, args...)
}

// Holding is the one statement of r.Sent whose text holds marker, which names the statement
// under test among the others its reader sent.
func (r *RecordedTx) Holding(t testing.TB, marker string) Statement {
	t.Helper()
	var found []Statement
	for _, statement := range r.Sent {
		if strings.Contains(statement.SQL, marker) {
			found = append(found, statement)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the reader sent %d statements holding %q, want one", len(found), marker)
	}
	return found[0]
}

// preparedPlans numbers the statements explainExecuted prepares, so no two share a name on a
// connection the pool hands out again.
var preparedPlans atomic.Int64

// explainExecuted runs EXPLAIN (ANALYZE, FORMAT format) of statement executed with args in tx,
// under the plan-cache mode mode, which it sets with `set local` inside tx. It answers the one
// "Plan" node for json and every line of the plan for text.
//
// The statement is prepared on tx's connection and explained through EXECUTE, because a
// plan-cache mode is what Postgres does with a prepared statement: an EXPLAIN that binds its own
// parameters is planned for those values, a custom plan, whatever the mode says. EXECUTE takes no
// bind parameter, so each argument is written as the text pgx encodes it in for the type
// Postgres inferred for that parameter, quoted as a literal of that type. The statement runs, so
// the caller explains in a transaction it rolls back.
//
// The statement's own failure - Postgres refusing to prepare it, to plan it or to run it - is
// answered as an error, which a control's caller reads as the control proving nothing
// (RequireTheCheckFails). A failure of the test's own making, an argument pgx cannot write, still
// fails t.
func explainExecuted(t testing.TB, ctx context.Context, tx pgx.Tx, statement string, args []any, mode, format string) ([]byte, error) {
	t.Helper()
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set plan_cache_mode = %s: %v", mode, err)
	}
	conn := tx.Conn()
	name := fmt.Sprintf("explaintest_plan_%d", preparedPlans.Add(1))
	description, err := conn.Prepare(ctx, name, statement)
	if err != nil {
		return nil, fmt.Errorf("prepare the statement explained: %w", err)
	}
	defer func() { _ = conn.Deallocate(context.WithoutCancel(ctx), name) }()
	if len(description.ParamOIDs) != len(args) {
		return nil, fmt.Errorf("the statement takes %d parameters and was given %d arguments", len(description.ParamOIDs), len(args))
	}
	literals := make([]string, len(args))
	for index, argument := range args {
		text, err := conn.TypeMap().Encode(description.ParamOIDs[index], pgtype.TextFormatCode, argument, nil)
		if err != nil {
			t.Fatalf("write argument %d (%T) as a literal: %v", index+1, argument, err)
		}
		if text == nil {
			literals[index] = "null"
			continue
		}
		literals[index] = "'" + strings.ReplaceAll(string(text), "'", "''") + "'"
	}
	execute := "execute " + name
	if len(literals) > 0 {
		execute += "(" + strings.Join(literals, ", ") + ")"
	}
	rows, err := tx.Query(ctx, "explain (analyze, format "+format+") "+execute)
	if err != nil {
		return nil, fmt.Errorf("explain the statement under %s: %w", mode, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("explain the statement under %s: %w", mode, err)
	}
	answer := []byte(strings.Join(lines, "\n"))
	if format == "json" {
		return OnePlan(t, answer), nil
	}
	return answer, nil
}

// ExplainAfter runs before, the statements a writer sent ahead of statement, and then explains
// statement (explainExecuted) under mode and NestedLoopsOnly, all in one transaction of db that
// it rolls back. Each call is a transaction of its own, since the explained statement runs and
// writes what a second explain would read.
func ExplainAfter(t testing.TB, ctx context.Context, db Beginner, before []Statement, statement Statement, mode, format string) []byte {
	t.Helper()
	answer, err := explainAfter(t, ctx, db, before, statement, mode, format)
	if err != nil {
		t.Fatalf("%v\n%s", err, statement.SQL)
	}
	return answer
}

// explainAfter is ExplainAfter answering the explained statement's own failure as an error
// (explainExecuted).
func explainAfter(t testing.TB, ctx context.Context, db Beginner, before []Statement, statement Statement, mode, format string) ([]byte, error) {
	t.Helper()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction a plan is read in: %v", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, earlier := range before {
		if _, err := tx.Exec(ctx, earlier.SQL, earlier.Args...); err != nil {
			t.Fatalf("run a statement sent before the one explained: %v", err)
		}
	}
	for _, setting := range NestedLoopsOnly {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	return explainExecuted(t, ctx, tx, statement.SQL, statement.Args, mode, format)
}

// UnprobedReads reads plan, one "Plan" node of EXPLAIN (ANALYZE, FORMAT JSON) of a statement over
// a batch of rows, for the reads such a statement may not make, and answers each it found
// (forbidden) and every other read of the batch or of a table in keyed, with how often it ran
// (allowed). It reads loops and never time.
//
// A scan of the batch - the function scan of its arrays, or the scan of a CTE of its rows - runs
// once, and nothing the plan built once is read again for each row (a Materialize that runs more
// than once): either is a set rescanned whole per row of what drives it, which is quadratic. A
// read of a table keyed names is a probe of the rows one row of the batch names: an index scan,
// or a bitmap heap scan whose bitmap is one index's, whose condition keys that index's first
// column, through the index keyed names for that table where it names one. Anything else - a
// sequential scan, an index read end to end, a bitmap over several indexes - reads the table or
// one of its indexes whole, which on the outer side of a nested loop over the batch is the
// quadratic plan the other way round. And a probe that runs more than once names a column of the
// row that drives it in its condition (keyedByOuterRow): one keyed on constants alone reads the
// same range again for each row, a whole kind of graph_nodes for each member of a document where
// the plan takes `kind = 'block'` for its condition and leaves the id to a join filter.
func UnprobedReads(t testing.TB, keys *IndexKeys, plan json.RawMessage, keyed map[string]string) (forbidden, allowed []string) {
	t.Helper()
	// bitmaps are the bitmap heap scans of each keyed table, each with its bitmap, in the order
	// the walk below meets them, which is the order BitmapHeapScans answers them in.
	bitmaps := map[string][]BitmapHeapScan{}
	for relation := range keyed {
		bitmaps[relation] = BitmapHeapScans(t, plan, relation)
	}
	for _, node := range Nodes(t, plan) {
		required, isKeyed := keyed[node.Relation]
		switch {
		case node.Type == "Function Scan" || node.Type == "CTE Scan":
			read := fmt.Sprintf("%s ran %.0f time(s), %.0f row(s) each", node, node.Loops, node.Rows)
			if node.Loops > 1 {
				forbidden = append(forbidden, read+": the batch is read again for each row of what drives it")
				continue
			}
			allowed = append(allowed, read)
		case node.Type == "Materialize" && node.Loops > 1:
			forbidden = append(forbidden, fmt.Sprintf("%s ran %.0f time(s), %.0f row(s) each: a set the plan built once is read again for each row of what drives it",
				node, node.Loops, node.Rows))
		case isKeyed && node.Type != "ModifyTable":
			read := fmt.Sprintf("%s ran %.0f time(s)", node, node.Loops)
			// index and cond are the index the read probes and the condition it probes by: the
			// scan's own, or the one index scan its bitmap is built from.
			index, cond := node.Index, node.IndexCond
			probe := node.Type == "Index Scan" || node.Type == "Index Only Scan"
			if node.Type == "Bitmap Heap Scan" {
				scan := bitmaps[node.Relation][0]
				bitmaps[node.Relation] = bitmaps[node.Relation][1:]
				if probe = scan.Bitmap.Type == "Bitmap Index Scan" && len(scan.BitmapIndexScans) == 1; probe {
					index, cond = scan.BitmapIndexScans[0].Index, scan.BitmapIndexScans[0].IndexCond
					read += " through " + index
				}
			}
			probe = probe && cond != "" && CondNamesFirstKey(cond, keys.FirstKey(t, index))
			switch {
			case !probe:
				forbidden = append(forbidden, read+": "+node.Relation+" is read other than by a probe of an index's key")
			case node.Loops > 1 && !keyedByOuterRow(cond, node.Alias):
				forbidden = append(forbidden, read+" under "+cond+": the same range of "+index+
					" is read again for each row of what drives it, since its condition names no column of that row")
			case required != AnyIndex && index != required:
				forbidden = append(forbidden, read+": "+node.Relation+" is probed other than through "+required)
			default:
				allowed = append(allowed, read+" under "+cond)
			}
		}
	}
	return forbidden, allowed
}

// qualifiedColumn matches one column a plan's condition spells with the alias of the relation it
// is read from - `t.from_id`, `written_1.kind` - with what follows it, so a function a schema
// names (`pg_catalog.lower(`) can be told from a column. stringLiteral matches a quoted constant,
// which may hold a dot.
var (
	qualifiedColumn = regexp.MustCompile(`([a-z_][a-z0-9_]*)\.[a-z_][a-z0-9_]*(\()?`)
	stringLiteral   = regexp.MustCompile(`'(?:[^']|'')*'`)
)

// keyedByOuterRow reports whether cond, the condition of an index scan of the relation the
// statement calls alias, names a column of another relation: the row that drives the scan, whose
// values the scan is run with each time. A condition that names none is constants and parameters
// alone, the same range of the index each time the scan runs.
func keyedByOuterRow(cond, alias string) bool {
	for _, match := range qualifiedColumn.FindAllStringSubmatch(stringLiteral.ReplaceAllString(cond, "''"), -1) {
		if match[2] == "" && match[1] != alias {
			return true
		}
	}
	return false
}

// RequireEachRowProbedOnce holds statement, explained after before (ExplainAfter) under each of
// PlanCacheModes, to UnprobedReads finding nothing, and records how each plan read the batch and
// the tables in keyed. name says what the statement is, for the failure and the record.
func RequireEachRowProbedOnce(
	t testing.TB, ctx context.Context, db Beginner, keys *IndexKeys, keyed map[string]string,
	name string, before []Statement, statement Statement,
) {
	t.Helper()
	for _, mode := range PlanCacheModes {
		forbidden, allowed := UnprobedReads(t, keys, ExplainAfter(t, ctx, db, before, statement, mode, "json"), keyed)
		if len(forbidden) > 0 {
			t.Errorf("%s under %s reads its batch more than once or a table other than by its key:\n  %s\nplan:\n%s",
				name, mode, strings.Join(forbidden, "\n  "), ExplainAfter(t, ctx, db, before, statement, mode, "text"))
			continue
		}
		t.Logf("%s under %s probes each row of its batch once:\n  %s", name, mode, strings.Join(allowed, "\n  "))
	}
}

// RequireTheCheckFails holds control, a statement as it stood before it was made to probe, to
// planning and running under each of PlanCacheModes, and then to failing the check
// RequireEachRowProbedOnce makes under each of failing, the plan-cache modes its quadratic plan
// shows under, and records the reads it fails on. A control Postgres cannot prepare, plan or run
// fails the test, since it proves nothing, and so does one that passes the check, since the check
// would then be one that cannot fail. Under each other mode it records the control's reading and
// holds it to nothing, since a plan the planner happens to pick for its estimates there is no
// shape the statement held it to.
func RequireTheCheckFails(
	t testing.TB, ctx context.Context, db Beginner, keys *IndexKeys, keyed map[string]string,
	name string, failing []string, before []Statement, control Statement,
) {
	t.Helper()
	for _, mode := range PlanCacheModes {
		plan, err := explainAfter(t, ctx, db, before, control, mode, "json")
		if err != nil {
			t.Errorf("%s, the control, does not plan and run under %s, so it proves nothing: %v\n%s", name, mode, err, control.SQL)
			continue
		}
		forbidden, allowed := UnprobedReads(t, keys, plan, keyed)
		switch {
		case !slices.Contains(failing, mode):
			t.Logf("%s under %s plans, in a mode its control is not held to, and reads:\n  %s", name, mode,
				strings.Join(append(forbidden, allowed...), "\n  "))
		case len(forbidden) == 0:
			t.Errorf("%s, the control, passes the check under %s, so the check cannot tell a quadratic plan from this one:\n%s",
				name, mode, ExplainAfter(t, ctx, db, before, control, mode, "text"))
		default:
			t.Logf("%s under %s plans and fails the check, as the control must:\n  %s", name, mode, strings.Join(forbidden, "\n  "))
		}
	}
}

// Part is one part of a statement as it stood before it was made to probe, which a control puts
// back into the statement its writer sends now (Spliced): the text that opens the part in that
// statement, which it holds once, the text that opens what follows the part, and the part as it
// stood, written from the same helpers the writer writes its statement with, so the control
// stands in for the code as it is rather than for a copy of it.
type Part struct {
	Name, Opens, Closes, Text string
}

// Spliced is sql with each of parts in place of what runs from its opening text up to its closing
// one, and fails t where sql does not hold a part's place once, or where putting the parts back
// changes nothing: a control that is the statement itself controls nothing.
func Spliced(t testing.TB, sql string, parts ...Part) string {
	t.Helper()
	spliced := sql
	for _, part := range parts {
		if count := strings.Count(spliced, part.Opens); count != 1 {
			t.Fatalf("the statement holds %q %d times, not once, to put %s as it stood at:\n%s", part.Opens, count, part.Name, sql)
		}
		start := strings.Index(spliced, part.Opens)
		end := strings.Index(spliced[start:], part.Closes)
		if end < 0 {
			t.Fatalf("the statement holds no %q after %q to put %s as it stood before:\n%s", part.Closes, part.Opens, part.Name, sql)
		}
		spliced = spliced[:start] + part.Text + spliced[start+end:]
	}
	if spliced == sql {
		t.Fatalf("the statement with %d part(s) as they stood is the statement's own text: the control controls nothing", len(parts))
	}
	return spliced
}

// Parameter is the parameter sql binds where pattern, a regular expression holding one group,
// matches the place of a parameter in it, read off the statement so a control names it where the
// statement does. Every match must name the same parameter, and there must be one.
func Parameter(t testing.TB, sql string, pattern *regexp.Regexp) string {
	t.Helper()
	matches := pattern.FindAllStringSubmatch(sql, -1)
	if len(matches) == 0 {
		t.Fatalf("the statement holds no place %s matches:\n%s", pattern, sql)
	}
	for _, match := range matches[1:] {
		if match[1] != matches[0][1] {
			t.Fatalf("the statement names both %s and %s where %s matches:\n%s", matches[0][1], match[1], pattern, sql)
		}
	}
	return matches[0][1]
}
