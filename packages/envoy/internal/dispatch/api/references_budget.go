package api

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// referencesClosureBound is the budget every statement of one GET of an issue's references shares
// (inClosureRead). A read that reaches it answers 503 REFERENCES_TIMEOUT, which the CLI prints as
// references unavailable, rather than holding a connection and a CPU for as long as the read
// takes. On 2026-11-20 from 00:27Z the eight-hop walk this read replaced held Dispatch's Cloud SQL
// instance at full CPU: every `dispatch read --issue` and the Artifacts tab ask for it, and Query
// Insights gave that one statement 31,299 s of execution in 20 minutes, 12 times every other
// statement together (cybermonklabs/legion#4554).
const referencesClosureBound = 2 * time.Second

// inClosureRead runs refs.Closure, getIssueReferences's read, in a read transaction of its own
// whose first statement sets jit off (inJITOffRead), on a context that ends a second past
// referencesClosureBound. The read is handed that context and the transaction as closureBudget,
// so every statement it sends - the out and in statements and each loader that names their nodes -
// runs under what is left of the bound since the read started. One statement_timeout of the whole
// bound for the transaction would not hold the bound: the read sends several statements, whose
// sum can pass the context's deadline with none past the bound, and at that deadline pgx closes
// the connection while the server's statement runs on (its default BuildContextWatcherHandler, as
// outbox/lease.go records). The context, a second past the bound, stays as the last guard, for a
// wait no statement_timeout covers, such as the begin's.
//
// JIT is off because compiling the read's statements costs more than running them. Each is index
// probes and sub-plans, and over a large issue the planner's estimate passes jit_inline_above_cost
// and jit_optimize_above_cost (500,000 each, Postgres's defaults). At c3173dd3, over c-read-cost's
// shape "a person with 50,000 rows of unrelated history" (50,412 blocks in the issue's documents),
// the out statement was costed at up to 989,065 and the in statement at up to 976,409, and Postgres
// compiled each on every read: JIT took a p50 of 1,328 ms of the out statement's 1,352 ms and
// 1,283 ms of the in statement's 1,308 ms, so the read passed its 2 s bound and answered 503
// REFERENCES_TIMEOUT (cybermonklabs/legion#4193). Production's database is Cloud SQL, which does
// not support PostgreSQL's LLVM JIT (benchtime.Open says the same), so this setting makes the read
// on any other database what it is in production. The setting starts with refs.ClosureTag, as
// every other statement of the read does, so that pg_stat_activity names it as the read's.
// TestTheIssueReferencesReadRunsWithJITOff holds the route's read to it on a session that starts
// at jit = on.
func (s *server) inClosureRead(ctx context.Context, who model.Actor, issueKey string) (refs.References, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, referencesClosureBound+time.Second)
	defer cancel()
	var references refs.References
	err := s.inJITOffRead(ctx, refs.ClosureTag, func(tx pgx.Tx) error {
		var err error
		references, err = refs.Closure(ctx, closureBudget{tx: tx, started: started}, who, issueKey)
		return err
	})
	return references, err
}

// inJITOffRead runs read in a read transaction of its own, tx, whose first statement is tag and
// then `set local jit = off`: tag is the comment that starts every other statement of the read,
// so that pg_stat_activity names the setting as the read's too, and "" for a read whose
// statements carry none. It serves the reads whose statements cost more to compile than to run,
// each of which says by how much: the issue references read (inClosureRead) and the references
// list read (inReferencesListRead). The setting is the transaction's alone (set local), so the
// session the pool hands back reads jit as it did before the read. The rollback runs on a context
// of its own, as the near-copy gate's does, bounded by the same readRollbackBound: the read's
// context can have expired exactly when the rollback matters, as inClosureRead's has at its bound,
// and pgx answers a rollback on an expired context by destroying the connection instead of
// returning it to the pool.
func (s *server) inJITOffRead(ctx context.Context, tag string, read func(tx pgx.Tx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollback, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), readRollbackBound)
		defer cancelRollback()
		_ = tx.Rollback(rollback)
	}()
	if _, err := tx.Exec(ctx, tag+"set local jit = off"); err != nil {
		return err
	}
	return read(tx)
}

// closureBudget is inClosureRead's transaction tx as its read reads it (refs.Queryer). Before
// each statement it passes on, it gives that statement what is left of referencesClosureBound
// since started, the start of the read (setStatementBudget), so a statement that waits, on a lock
// or for the CPU, takes its wait out of the time of every statement after it. It answers the
// budget's refusal in place of a statement it does not send.
type closureBudget struct {
	tx      pgx.Tx
	started time.Time
}

func (b closureBudget) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := b.spend(ctx); err != nil {
		return nil, err
	}
	return b.tx.Query(ctx, sql, args...)
}

func (b closureBudget) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := b.spend(ctx); err != nil {
		return refusedRow{err: err}
	}
	return b.tx.QueryRow(ctx, sql, args...)
}

// spend gives the next statement of the read what is left of referencesClosureBound since
// started (setStatementBudget).
func (b closureBudget) spend(ctx context.Context) error {
	return setStatementBudget(ctx, b.tx, referencesClosureBound-time.Since(b.started))
}

// refusedRow is the row closureBudget answers for a statement it did not send: its Scan answers
// the refusal.
type refusedRow struct{ err error }

func (r refusedRow) Scan(...any) error { return r.err }

// errClosureBudgetSpent is setStatementBudget's answer when less than a millisecond of
// referencesClosureBound is left before a statement of inClosureRead's read: the read is at its
// bound, and the route answers it as it answers a statement Postgres cancelled there.
var errClosureBudgetSpent = errors.New("the reference closure's budget has less than 1ms left")

// setStatementBudget gives the next statement of inClosureRead's transaction tx what is left of
// referencesClosureBound, left, so that the statements of the read share the one bound rather
// than each having it whole. It sets statement_timeout for the rest of tx to the whole
// milliseconds of left, rounded down, 1ms at 1 ms left, with
// `select set_config('statement_timeout', $1, true)` after refs.ClosureTag, which starts every
// statement of the read, so that pg_stat_activity names this statement as the read's too. Under
// 1 ms left it sends no statement and answers errClosureBudgetSpent, which closureBoundHit reads
// as the bound, because the timeout rounded down would be 0, and a statement_timeout of 0 turns
// the timeout off.
func setStatementBudget(ctx context.Context, tx pgx.Tx, left time.Duration) error {
	milliseconds := left.Milliseconds()
	if milliseconds < 1 {
		return errClosureBudgetSpent
	}
	_, err := tx.Exec(ctx, refs.ClosureTag+"select set_config('statement_timeout', $1, true)", strconv.FormatInt(milliseconds, 10)+"ms")
	return err
}

// closureBoundHit says whether inClosureRead's read ended at its bound: Postgres cancelled a
// statement at the statement_timeout its budget set (SQLSTATE 57014), the budget had less than a
// millisecond left for the next statement (errClosureBudgetSpent), or the context expired first.
func closureBoundHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosureBudgetSpent) {
		return true
	}
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014" // query_canceled
}
