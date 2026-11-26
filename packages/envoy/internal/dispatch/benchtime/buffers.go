package benchtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// planForValues and genericPlan name the two plans topBuffers reads a statement under, in every
// line that logs a reading and in the error a pair fails with.
const (
	planForValues = "the plan for its values"
	genericPlan   = "the generic plan"
)

// buffersStatement is the name topBuffers prepares a statement under for its generic plan, and
// cachedPlansStatement the name ReportCachedPlans prepares one under. One statement is prepared
// under each at a time and deallocated before the next, so one name serves them all.
const (
	buffersStatement     = "benchtime_top_buffers"
	cachedPlansStatement = "benchtime_cached_plans"
)

// cachedPlanExecutions is how many times ReportCachedPlans executes a prepared statement, and
// cachedPlanLogged the first execution whose whole plan it logs. Postgres plans the first five
// executions of a prepared statement for their values and may run its generic plan from the
// sixth on, so the sixth and the seventh are the first that can show the plan a cached
// statement keeps.
const (
	cachedPlanExecutions = 7
	cachedPlanLogged     = 6
)

// PlanBuffers is one plan of a statement, run and measured under EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON), with the shared buffers its top node hit and read. A node's buffers count every
// node under it, so the top node's are the whole statement's read: what "fewer buffer reads" is
// judged on, and what the top line of a plan read on production shows.
type PlanBuffers struct {
	// Mode names the plan: the plan for its values or the generic plan.
	Mode string
	// Hit and Read are the top node's shared buffers found in the cache and read into it.
	Hit, Read int64
	// Explained is EXPLAIN's whole answer, with the planning and execution times, as a log
	// prints it. Plan is its one "Plan" node (explaintest.OnePlan), as explaintest's readers and
	// a caller's own reading of the nodes take it.
	Explained, Plan json.RawMessage
}

// Shared is the top node's shared buffers, hit plus read.
func (p PlanBuffers) Shared() int64 { return p.Hit + p.Read }

// Buffers are one statement's top-node shared buffers under the two plans Postgres can run it by
// (explaintest.PlanCacheModes): ForValues, the plan made for the values it is bound to, which is
// the plan every run of a pair reads; and Generic, the plan made with no value known, which a
// prepared statement can settle on from its sixth run, as pgx prepares every statement it sends.
type Buffers struct {
	ForValues, Generic PlanBuffers
}

// Plans are the two readings, the plan for its values first.
func (b Buffers) Plans() []PlanBuffers { return []PlanBuffers{b.ForValues, b.Generic} }

// topBuffers runs statement once under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) as the plan made
// for its values and once as the generic plan, each on the measurement connection, after the
// statement's Setup, in a transaction of its own that it rolls back, and answers the top-node
// buffers of each plan with the plan. name says which statement it is, in a failure.
//
// The generic plan is read as explaintest.AnalyzedGenericPlan reads one: the statement is
// prepared, plan_cache_mode is force_generic_plan for the transaction, and EXPLAIN runs its
// EXECUTE. An EXECUTE under EXPLAIN takes no bound parameter, so each argument is written into it
// as a literal (PreparedArguments) and the statement is prepared with the literals' types. A
// prepared statement belongs to the session rather than to the transaction, so it is deallocated
// after the rollback.
func (r *Runner) topBuffers(t *testing.T, name string, statement Statement) Buffers {
	t.Helper()
	forValues := r.explainBuffers(t, name, planForValues, "", statement)
	generic := statement
	generic.Setup = append(slices.Clone(statement.Setup), "set local plan_cache_mode = force_generic_plan")
	return Buffers{ForValues: forValues, Generic: r.explainBuffers(t, name, genericPlan, buffersStatement, generic)}
}

// PreparedArguments writes each of arguments as the type a statement is prepared with for it and
// the literal an EXECUTE takes in its place (sqlLiteral), in order: an EXECUTE under EXPLAIN takes
// no bound parameter. topBuffers and ReportCachedPlans prepare a measured statement with them, and
// a plan test hands them to explaintest.GenericPlan or explaintest.AnalyzedGenericPlan for the
// arguments it binds the same statement with elsewhere, rather than keeping a second list by hand.
// An argument of a type sqlLiteral writes no literal for fails t, naming name, the statement, and
// the argument's place.
func PreparedArguments(t *testing.T, name string, arguments ...any) (parameters, literals []string) {
	t.Helper()
	parameters = make([]string, 0, len(arguments))
	literals = make([]string, 0, len(arguments))
	for index, argument := range arguments {
		parameter, literal, err := sqlLiteral(argument)
		if err != nil {
			t.Fatalf("%s: the argument $%d: %v", name, index+1, err)
		}
		parameters = append(parameters, parameter)
		literals = append(literals, literal)
	}
	return parameters, literals
}

// preparedForm is statement as a PREPARE under the name prepared, with each argument's type, and
// as the EXECUTE of it that writes each argument in as a literal (PreparedArguments). name says
// which statement it is, in a failure.
func preparedForm(t *testing.T, name, prepared string, statement Statement) (string, string) {
	t.Helper()
	types, literals := PreparedArguments(t, name, statement.Args...)
	prepare, execute := "prepare "+prepared, "execute "+prepared
	if len(types) > 0 {
		prepare += " (" + strings.Join(types, ", ") + ")"
		execute += "(" + strings.Join(literals, ", ") + ")"
	}
	return prepare + " as " + statement.SQL, execute
}

// ReportCachedPlans runs each side of a pair as a prepared statement cachedPlanExecutions times on
// the measurement connection, under the session's own plan_cache_mode, as pgx runs every statement
// it prepares, and bounds nothing. Each execution is explained (EXPLAIN (ANALYZE, BUFFERS, FORMAT
// JSON) EXECUTE) after the side's Setup in a transaction it rolls back. It logs, for each
// execution, the top node's shared buffers and whether Postgres ran its generic plan or a plan
// made for the values, which pg_prepared_statements' counts of each tell after the execution, and
// from the sixth execution on the whole plan. It shows which plan each side keeps once Postgres may
// run its cached generic plan, which topBuffers, forcing one plan or the other, does not.
func (r *Runner) ReportCachedPlans(t *testing.T, name string, before, after Statement) {
	t.Helper()
	ctx := context.Background()
	var cacheMode string
	if err := r.conn.QueryRow(ctx, "select current_setting('plan_cache_mode')").Scan(&cacheMode); err != nil {
		t.Fatalf("%s: read plan_cache_mode: %v", name, err)
	}
	r.cachedPlans(t, name, beforeSide, cacheMode, before)
	r.cachedPlans(t, name, afterSide, cacheMode, after)
}

// cachedPlans is one side of ReportCachedPlans. The statement is prepared once, executed
// cachedPlanExecutions times, and deallocated, a failed run's too.
func (r *Runner) cachedPlans(t *testing.T, name, side, cacheMode string, statement Statement) {
	t.Helper()
	ctx := context.Background()
	measured := name + ": " + side
	prepare, execute := preparedForm(t, measured, cachedPlansStatement, statement)
	if _, err := r.conn.Exec(ctx, prepare); err != nil {
		t.Fatalf("%s: prepare the statement: %v", measured, err)
	}
	defer func() {
		if _, err := r.conn.Exec(ctx, "deallocate "+cachedPlansStatement); err != nil {
			t.Errorf("%s: deallocate the prepared statement: %v", measured, err)
		}
	}()
	var generics int64
	for execution := 1; execution <= cachedPlanExecutions; execution++ {
		mode := fmt.Sprintf("execution %d of %d of the prepared statement, under plan_cache_mode %s", execution, cachedPlanExecutions, cacheMode)
		reading := r.explainBuffers(t, measured, mode, "", Statement{SQL: execute, Setup: statement.Setup})
		var genericCount, customCount int64
		if err := r.conn.QueryRow(ctx, `select generic_plans, custom_plans from pg_prepared_statements where name = $1`,
			cachedPlansStatement).Scan(&genericCount, &customCount); err != nil {
			t.Fatalf("%s: read the plan counts of the prepared statement after %s: %v", measured, mode, err)
		}
		ran := "a plan made for its values"
		if genericCount > generics {
			ran = "its generic plan"
		}
		generics = genericCount
		line := fmt.Sprintf("%s: %s %s ran %s and reads %d shared buffers at its top node (%d hit, %d read); pg_prepared_statements counts %d generic and %d custom plans",
			name, side, mode, ran, reading.Shared(), reading.Hit, reading.Read, genericCount, customCount)
		if execution < cachedPlanLogged {
			t.Log(line)
			continue
		}
		t.Logf("%s, its plan:\n%s", line, reading.Explained)
	}
}

// explainBuffers is one reading of topBuffers or of ReportCachedPlans: in a transaction it rolls
// back, it runs statement's Setup and explains statement with its arguments. Where prepared names
// a statement, it prepares statement under that name after the Setup (preparedForm) and explains
// the EXECUTE of it instead, and deallocates that name after the rollback, a failed run's too, so
// the next reading can prepare its own under the same name.
func (r *Runner) explainBuffers(t *testing.T, name, mode, prepared string, statement Statement) PlanBuffers {
	t.Helper()
	ctx := context.Background()
	prepare, explained, arguments := "", statement.SQL, statement.Args
	if prepared != "" {
		prepare, explained = preparedForm(t, name, prepared, statement)
		arguments = nil
	}
	held := false
	defer func() {
		if !held {
			return
		}
		if _, err := r.conn.Exec(ctx, "deallocate "+prepared); err != nil {
			t.Errorf("%s: deallocate the statement %s's buffers were read under: %v", name, mode, err)
		}
	}()
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin the reading of %s: %v", name, mode, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, setting := range statement.Setup {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %s before the reading of %s: %v", name, setting, mode, err)
		}
	}
	if prepare != "" {
		if _, err := tx.Exec(ctx, prepare); err != nil {
			t.Fatalf("%s: prepare the statement for %s: %v", name, mode, err)
		}
		held = true
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+explained, arguments...).Scan(&raw); err != nil {
		t.Fatalf("%s: explain %s: %v", name, mode, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("%s: roll back the reading of %s: %v", name, mode, err)
	}
	plan := explaintest.OnePlan(t, raw)
	top := explaintest.Nodes(t, plan)[0]
	return PlanBuffers{
		Mode: mode, Hit: int64(top.SharedHitBlocks), Read: int64(top.SharedReadBlocks),
		Explained: raw, Plan: plan,
	}
}

// sqlLiteral writes argument as the literal an EXECUTE takes in its place, with the type the
// statement is prepared with for it: a string as text, a []string as text[], a bool as boolean, a
// []int16 as smallint[], and a time.Time as timestamptz, in UTC, or a *time.Time as one, null
// where it is nil. A time is written to the microsecond Postgres keeps, with the digits below it
// dropped as pgx drops them when it binds a time, rather than with nanoseconds Postgres would
// round, so the literal is the value the plan for its values is bound to. Those are the types the
// measured reads bind: the listing binds a string, a []string, a bool, a []int16 and a *time.Time
// (api's projectListingArguments, as listIssues binds them), and the answers read binds a
// time.Time (sessionanswers' List). Any other type is an error rather than a literal of a guessed
// type.
func sqlLiteral(argument any) (string, string, error) {
	switch value := argument.(type) {
	case string:
		return "text", store.QuoteLiteral(value), nil
	case []string:
		quoted := make([]string, 0, len(value))
		for _, element := range value {
			quoted = append(quoted, store.QuoteLiteral(element))
		}
		return "text[]", "array[" + strings.Join(quoted, ", ") + "]::text[]", nil
	case bool:
		return "boolean", strconv.FormatBool(value), nil
	case []int16:
		numbers := make([]string, 0, len(value))
		for _, number := range value {
			numbers = append(numbers, strconv.Itoa(int(number)))
		}
		return "smallint[]", "array[" + strings.Join(numbers, ", ") + "]::smallint[]", nil
	case time.Time:
		return "timestamptz", store.QuoteLiteral(value.UTC().Format("2006-01-02T15:04:05.000000Z07:00")) + "::timestamptz", nil
	case *time.Time:
		if value == nil {
			return "timestamptz", "null::timestamptz", nil
		}
		return sqlLiteral(*value)
	}
	return "", "", fmt.Errorf("no literal is written for a value of type %T", argument)
}

// PairBuffers reads the top-node shared buffers of a pair's two sides under both plans
// (topBuffers), logs each reading under the name of the side it was read from, and fails t,
// through t.Errorf so the rest of the run still logs, unless the after text reads fewer buffers
// than the before text under each plan. It is the gate of a pair whose claim is fewer buffer
// reads, beside the times its Pair gates. It runs where t runs it, in no subtest of its own, and
// it answers both readings, for a caller that reads more of the plans.
func (r *Runner) PairBuffers(t *testing.T, name string, before, after Statement) (Buffers, Buffers) {
	t.Helper()
	beforeBuffers := r.topBuffers(t, name+": "+beforeSide, before)
	afterBuffers := r.topBuffers(t, name+": "+afterSide, after)
	logBuffers(t, name, beforeSide, beforeBuffers)
	logBuffers(t, name, afterSide, afterBuffers)
	afterPlans := afterBuffers.Plans()
	for index, plan := range beforeBuffers.Plans() {
		if read := afterPlans[index]; read.Shared() >= plan.Shared() {
			t.Errorf("%s: under %s the after text's top node reads %d shared buffers, not fewer than the before text's %d; %s",
				name, plan.Mode, read.Shared(), plan.Shared(), plansLogged(name))
		}
	}
	return beforeBuffers, afterBuffers
}

// ReportBuffers reads the after side's buffers as PairBuffers does, logs both sides as it does,
// and bounds neither: for a candidate text, measured beside the text it may replace, that no rule
// yet holds. before is the before side's reading, which the caller has already read (PairBuffers
// answers it), so the text it may replace is not run again; it answers the after side's.
func (r *Runner) ReportBuffers(t *testing.T, name string, before Buffers, after Statement) Buffers {
	t.Helper()
	afterBuffers := r.topBuffers(t, name+": "+afterSide, after)
	logBuffers(t, name, beforeSide, before)
	logBuffers(t, name, afterSide, afterBuffers)
	return afterBuffers
}

// AfterBuffers reads one statement's buffers as topBuffers does, logs them as PairBuffers logs
// the after side's, and bounds nothing: for a read's new work that no before text did, as
// AfterSeries times it.
func (r *Runner) AfterBuffers(t *testing.T, name string, after Statement) {
	t.Helper()
	logBuffers(t, name, afterSide, r.topBuffers(t, name+": "+afterSide, after))
}

// logBuffers is the one line each plan's buffers are reported in, with the plan, so that a count
// can never reach a log without the side and the plan it was read from.
func logBuffers(t *testing.T, name, side string, buffers Buffers) {
	t.Helper()
	for _, plan := range buffers.Plans() {
		t.Logf("%s: %s under %s reads %d shared buffers at its top node (%d hit, %d read), its plan:\n%s",
			name, side, plan.Mode, plan.Shared(), plan.Hit, plan.Read, plan.Explained)
	}
}
