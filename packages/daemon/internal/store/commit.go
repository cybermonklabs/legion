package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// TxPool is what TransactWith opens its transaction on, and the outcome read of a lost commit
// its own read-only one. The daemon's guarded pool (*Pool), a pgx pool and intake's Pool all
// satisfy it.
type TxPool interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// ErrCommitOutcomeUnknown is TransactWith's answer for a commit whose reply was lost while the
// store could not say whether the transaction committed. A warning names the transaction, and its
// hooks wait for the store's answer (watchOutcome).
var ErrCommitOutcomeUnknown = errors.New("the store did not say whether the transaction committed")

// errTransactionOnAnotherPool is TransactWith's refusal of a transaction asked for on another pool
// while the caller's own is open: one body joins one transaction, and a second pool's would commit
// on its own.
var errTransactionOnAnotherPool = errors.New("store: a transaction on another pool was opened inside an open transaction")

const (
	// commitReplyBound is how long a commit waits for its reply after its caller's context ends.
	commitReplyBound = 1 * time.Second
	// outcomeReadBound is how long one read of a transaction's outcome may take.
	outcomeReadBound = 1 * time.Second
	// outcomePollStep is how often a read asks again while the store says the transaction is in
	// progress.
	outcomePollStep = 50 * time.Millisecond
	// outcomeRetryFirst is how long the watcher waits before its first read; each wait after it
	// doubles, up to outcomeRetryMax.
	outcomeRetryFirst = 1 * time.Second
	outcomeRetryMax   = 30 * time.Second
)

// CommitSettleBound is the longest Transact waits, after its caller's context has ended, to learn
// a sent commit's outcome: commitReplyBound for the reply, then outcomeReadBound for the store's
// answer. It is counted from the end of the caller's context, not from the commit's start, and it
// holds no time for the hooks themselves. A caller whose context never ends has no deadline on the
// reply.
const CommitSettleBound = commitReplyBound + outcomeReadBound

// commitHooksKey is the context key a TransactWith scope lives under.
type commitHooksKey struct{}

// commitScope is one open transaction of TransactWith: the hooks its body registers, the
// transaction and its pool, and whether its body still runs. A TransactWith called on a context
// whose scope's body still runs joins that transaction.
type commitScope struct {
	hooks   []func()
	tx      pgx.Tx
	pool    TxPool
	writing atomic.Bool
}

// withCommitHooks scopes OnCommit to the transaction tx of pool: the hooks registered on the
// returned context are appended to the scope's list. TransactWith is its only caller, so a hook
// waits only on a commit whose outcome TransactWith reads (TestOnlyTransactOpensACommitScope).
func withCommitHooks(ctx context.Context, pool TxPool, tx pgx.Tx) (context.Context, *commitScope) {
	scope := &commitScope{tx: tx, pool: pool}
	return context.WithValue(ctx, commitHooksKey{}, scope), scope
}

// scopeOf is ctx's commit scope, or nil for a context no TransactWith scopes or one an outcome
// read cleared (outcomeReadContext).
func scopeOf(ctx context.Context) *commitScope {
	scope, _ := ctx.Value(commitHooksKey{}).(*commitScope)
	return scope
}

// OnCommit runs fn once the transaction of ctx's TransactWith has committed. Handler state kept in
// memory, and the journal line saying what a write did, must never claim more than what
// committed: a later step's failure rolls the transaction back, and a write retried after a
// failed commit would say it twice.
//
// A hook runs once, and only if its transaction committed. It usually runs on the owner's
// goroutine before TransactWith returns. When the commit's outcome was unknown, it runs later, on
// the watcher's goroutine: by then later transactions may have run their own hooks, and the daemon
// may be stopping. So a hook must be safe to run late: it states what committed, or applies an
// effect that no later state makes wrong. Work that is not safe to run late stays after
// TransactWith returns, where its own retry derives it again.
//
// A hook writes nothing to the store. A durable write belongs in the transaction, where the commit
// orders it against every other write; a store write the watcher runs late has no such order and
// can change what a newer write wrote. So a hook's effects are log lines, wakes and in-process
// state, and in-process state only behind a fence a late run cannot cross: admission's held
// release clears only the holds it applied, and a controller mint's hook revokes only the grants
// of generations below the one it confirms.
//
// The hook list is not goroutine-safe: OnCommit is called only on the goroutine that runs
// TransactWith's write. It panics on a context no TransactWith scopes, since a write made outside
// TransactWith has no commit to wait for.
func OnCommit(ctx context.Context, fn func()) {
	scope := scopeOf(ctx)
	if scope == nil {
		panic("store.OnCommit outside a transaction's commit scope (store.Transact)")
	}
	scope.hooks = append(scope.hooks, fn)
}

// CommitScoped reports whether ctx is within a transaction's commit scope (TransactWith): a caller
// that wants a hook to run once the write it just made has committed can tell a transaction that
// will commit from a write made outside any scope, which has no commit to wait for.
func CommitScoped(ctx context.Context) bool {
	return scopeOf(ctx) != nil
}

// Transact is TransactWith with pgx's default options: a read-write transaction that reads its id
// before it commits.
func Transact(ctx context.Context, pool TxPool, write func(ctx context.Context, tx pgx.Tx) error) error {
	return TransactWith(ctx, pool, pgx.TxOptions{}, write)
}

// TransactWith runs write in one transaction of pool, opened with options, commits it, and runs
// the hooks write registered with OnCommit if and only if the transaction committed. write runs
// once, on the scoped context, and is never retried. Every transaction of the daemon opens here,
// the read-only and repeatable-read ones included. In order:
//
//   - On a context whose scope's write still runs, it opens nothing: on that scope's pool it
//     runs write in the open transaction and returns write's error, so write's hooks wait on the
//     open transaction's commit, and options are the open transaction's; on another pool it runs
//     nothing and returns errTransactionOnAnotherPool.
//   - A rollback is deferred as soon as the transaction begins, so a panic in write, or any
//     return below, gives the connection back to the pool and clears the caller's hold mark. It
//     runs on the commit's context, below, so a rollback after the caller's cancel still reaches
//     the server and the pool gets back a connection whose transaction has ended.
//   - write's error is returned. No hook runs.
//   - The transaction's id is read. No id means the transaction wrote nothing. A read-only
//     transaction, in which PostgreSQL refuses every write to a table or a sequence (SQLSTATE
//     25006), reads none.
//   - A caller's context that has ended is returned as its error, and nothing commits: a context
//     that ended inside write already fails the id read, and this refuses one that ended after it.
//   - The commit runs on a context that the caller's cancel ends only commitReplyBound after that
//     cancel. A caller whose context never ends waits for the reply as long as the server takes.
//     The outcome read's own transaction has no such grace (outcomeReadContext).
//   - A commit that succeeded runs the hooks, in order, on this goroutine.
//   - A commit the server answered as not committed (an ERROR, or a ROLLBACK) returns its error,
//     and so does one that never reached the server (commitErrorKind). No hook runs.
//   - Any other failure lost the reply, and the server may have committed. A transaction that
//     wrote nothing holds the same rows either way, so its hooks run. Otherwise the store is asked
//     on a fresh connection, under a hold mark of its own, for at most outcomeReadBound
//     (readOutcome). Committed logs a warning and runs the hooks. Aborted returns an error, and
//     no hook runs. A store that cannot say logs a warning naming the transaction, the commit's
//     error and the status read's error before TransactWith returns ErrCommitOutcomeUnknown with
//     both errors, and leaves the hooks to a watcher that runs them once the store says the
//     transaction committed (watchOutcome).
//
// So a caller whose context ends waits at most CommitSettleBound more for its commit's outcome.
func TransactWith(ctx context.Context, pool TxPool, options pgx.TxOptions, write func(ctx context.Context, tx pgx.Tx) error) error {
	if open := scopeOf(ctx); open != nil && open.writing.Load() {
		if !samePool(open.pool, pool) {
			return errTransactionOnAnotherPool
		}
		return write(ctx, open.tx)
	}
	tx, err := pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	committing, stop := commitContext(ctx)
	defer stop()
	// Once a commit has been tried, the transaction is closed and this does nothing.
	defer func() { _ = tx.Rollback(committing) }()

	scoped, scope := withCommitHooks(ctx, pool, tx)
	if err := runWrite(scoped, scope, write); err != nil {
		return err
	}
	xid := ""
	if options.AccessMode != pgx.ReadOnly {
		var assigned *string
		if err := tx.QueryRow(ctx, "select pg_current_xact_id_if_assigned()::text").Scan(&assigned); err != nil {
			return fmt.Errorf("read the transaction's id: %w", err)
		}
		if assigned != nil {
			xid = *assigned
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	err = tx.Commit(committing)
	stop()
	switch {
	case err == nil:
		runHooks(scope.hooks)
		return nil
	case commitErrorKind(err) != commitAmbiguous:
		return err
	default:
		return settleLostReply(ctx, pool, xid, scope.hooks, err)
	}
}

// runWrite runs write on the scoped context with the scope marked open, and closes the scope once
// write returns, a panic included: a TransactWith on the scoped context after that opens a
// transaction of its own.
func runWrite(scoped context.Context, scope *commitScope, write func(ctx context.Context, tx pgx.Tx) error) error {
	scope.writing.Store(true)
	defer scope.writing.Store(false)
	return write(scoped, scope.tx)
}

// samePool reports whether a and b are one pool: the same comparable value. A pool whose dynamic
// type cannot be compared is no pool the join can prove the same.
func samePool(a, b TxPool) bool {
	kind := reflect.TypeOf(a)
	return kind == reflect.TypeOf(b) && kind != nil && kind.Comparable() && a == b
}

// settleLostReply is TransactWith's answer for a commit whose reply was lost with lost, for the
// transaction xid, "" when it wrote nothing, and the hooks its write registered. The read of the
// outcome takes a fresh connection under a mark of its own: pgx has already given the
// transaction's connection back, and the caller's mark is clear.
func settleLostReply(ctx context.Context, pool TxPool, xid string, hooks []func(), lost error) error {
	if xid == "" {
		runHooks(hooks)
		return nil
	}
	reading, cancel := context.WithTimeout(ForConcurrentWorker(context.WithoutCancel(ctx)), outcomeReadBound)
	defer cancel()
	result, statusErr := readOutcome(reading, pool, xid)
	switch result {
	case outcomeCommitted:
		slog.Warn("store: a commit's reply was lost; the store says the transaction committed",
			"transaction", xid, "error", lost)
		runHooks(hooks)
		return nil
	case outcomeAborted:
		return fmt.Errorf("transaction %s did not commit: %w", xid, lost)
	default:
		slog.Warn("store: a commit's reply was lost and the store cannot say whether it landed; its commit hooks wait until it can",
			"transaction", xid, "hooks", len(hooks), "error", lost, "status_error", statusErr)
		go watchOutcome(ForConcurrentWorker(context.WithoutCancel(ctx)), pool, xid, hooks)
		return &outcomeUnknownError{xid: xid, lost: lost, status: statusErr}
	}
}

// outcomeUnknownError is the error TransactWith returns when the store cannot say whether a sent
// commit landed: the transaction it names, the commit's error and the status read's.
type outcomeUnknownError struct {
	xid          string
	lost, status error
}

func (e *outcomeUnknownError) Error() string {
	return fmt.Sprintf("%v: transaction %s: %v; %v", ErrCommitOutcomeUnknown, e.xid, e.lost, e.status)
}

// Is makes the error ErrCommitOutcomeUnknown to errors.Is.
func (e *outcomeUnknownError) Is(target error) bool { return target == ErrCommitOutcomeUnknown }

// Unwrap is the commit's error and the status read's.
func (e *outcomeUnknownError) Unwrap() []error { return []error{e.lost, e.status} }

// noCommitGrace marks the context of an outcome read: commitContext gives its COMMIT and its
// rollback no grace, so the read's deadline is its hard end.
type noCommitGrace struct{}

// outcomeReadContext is ctx marked noCommitGrace and holding no commit scope, so a read never
// joins a transaction and never outlives ctx's deadline.
func outcomeReadContext(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, noCommitGrace{}, true), commitHooksKey{}, (*commitScope)(nil))
}

// commitContext is ctx without its cancel, ended commitReplyBound after ctx ends, for the commit
// and the rollback alone: a ctx that never ends never ends it. stop ends it at once and releases
// what it holds. An outcome read's ctx (outcomeReadContext) is its own commit context, with a stop
// that does nothing: its deadline cuts its COMMIT and its rollback as it cuts its query.
func commitContext(ctx context.Context) (committing context.Context, stop func()) {
	if ctx.Value(noCommitGrace{}) != nil {
		return ctx, func() {}
	}
	committing, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopAfter := context.AfterFunc(ctx, func() {
		bound := time.NewTimer(commitReplyBound)
		defer bound.Stop()
		select {
		case <-bound.C:
		case <-committing.Done():
		}
		cancel()
	})
	return committing, func() {
		stopAfter()
		cancel()
	}
}

func runHooks(hooks []func()) {
	for _, hook := range hooks {
		hook()
	}
}

// commitError is what a failed commit says of whether the server committed.
type commitError int

const (
	commitAmbiguous commitError = iota // the server may or may not have committed: read the outcome
	commitAnswered                     // the server answered that the transaction did not commit
	commitUnsent                       // nothing reached the server
)

// commitErrorKind sorts a failed commit's error. The server's answer that the transaction did not
// commit is a ROLLBACK tag (pgx.ErrTxCommitRollback) or an ERROR, read from the unlocalized
// severity where the server sends one. A FATAL or a PANIC is also a *pgconn.PgError, which pgx
// returns for a server that ends the connection: it says that the connection ended, not whether
// the transaction committed, so it is ambiguous, as is every error but one pgx marks as sent
// nothing (pgconn.SafeToRetry).
func commitErrorKind(err error) commitError {
	switch {
	case errors.Is(err, pgx.ErrTxCommitRollback), answeredError(err):
		return commitAnswered
	case pgconn.SafeToRetry(err):
		return commitUnsent
	default:
		return commitAmbiguous
	}
}

// answeredError reports whether err is the server's ERROR answer.
func answeredError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return severity == "ERROR"
}

// outcome is what the store says of a transaction whose commit's reply was lost.
type outcome int

const (
	outcomeUnknown outcome = iota
	outcomeCommitted
	outcomeAborted
)

// readOutcome asks `select pg_xact_status($1::xid8)` on a fresh connection, in a read-only
// TransactWith on outcomeReadContext(ctx), again every outcomePollStep while it answers
// `in progress`, until ctx ends. ctx's deadline is the hard end of every try: its BEGIN, its query,
// its COMMIT and its rollback. A COMMIT the deadline cuts is a read-only transaction's lost reply,
// so the status the query read stands. A failed read, a status the server does not keep (NULL),
// and a transaction still in progress when ctx ends are unknown, with the error that says which.
func readOutcome(ctx context.Context, pool TxPool, xid string) (outcome, error) {
	reading := outcomeReadContext(ctx)
	for {
		var status *string
		if err := TransactWith(reading, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "select pg_xact_status($1::xid8)", xid).Scan(&status)
		}); err != nil {
			return outcomeUnknown, fmt.Errorf("read the status of transaction %s: %w", xid, err)
		}
		switch {
		case status == nil:
			return outcomeUnknown, fmt.Errorf("the store keeps no status of transaction %s", xid)
		case *status == "committed":
			return outcomeCommitted, nil
		case *status == "aborted":
			return outcomeAborted, nil
		}
		poll := time.NewTimer(outcomePollStep)
		select {
		case <-ctx.Done():
			poll.Stop()
			return outcomeUnknown, fmt.Errorf("transaction %s is still in progress: %w", xid, ctx.Err())
		case <-poll.C:
		}
	}
}

// watchOutcome runs hooks once readOutcome says xid committed, and drops them once it says it
// aborted. ctx has no cancel and a hold mark of its own, since the transaction's owner may hold
// its next transaction's connection on its old mark, and the watcher holds no connection between
// reads. It waits outcomeRetryFirst, then twice as long each time up to outcomeRetryMax, and each
// read takes at most outcomeReadBound. It ends at the first answer, or once the pool has closed,
// which says that the hooks did not run; so it runs its hooks at most once. It has no other time
// limit and is not joined at stop: a status the server does not keep leaves it reading every
// outcomeRetryMax until the pool closes.
func watchOutcome(ctx context.Context, pool TxPool, xid string, hooks []func()) {
	for wait := outcomeRetryFirst; ; wait = min(2*wait, outcomeRetryMax) {
		time.Sleep(wait)
		reading, cancel := context.WithTimeout(ctx, outcomeReadBound)
		result, err := readOutcome(reading, pool, xid)
		cancel()
		switch {
		case result == outcomeCommitted:
			slog.Warn("store: a transaction whose outcome was unknown committed; its commit hooks run now",
				"transaction", xid, "hooks", len(hooks))
			runHooks(hooks)
			return
		case result == outcomeAborted:
			slog.Info("store: a transaction whose outcome was unknown did not commit; its commit hooks are dropped",
				"transaction", xid, "hooks", len(hooks))
			return
		case errors.Is(err, puddle.ErrClosedPool):
			slog.Error("store: the pool closed before the store said whether a transaction committed; its commit hooks did not run",
				"transaction", xid, "hooks", len(hooks))
			return
		}
	}
}

// transactUntilCommitted runs write in Transact until one run commits, and returns nil then. A run
// whose outcome is unknown is waited for and taken as that outcome; one that did not commit runs
// again after outcomeRetryFirst, the wait doubling up to outcomeRetryMax. write must be safe to run
// again after a run that did not commit.
func transactUntilCommitted(ctx context.Context, pool TxPool, write func(ctx context.Context, tx pgx.Tx) error) error {
	for wait := outcomeRetryFirst; ; wait = min(2*wait, outcomeRetryMax) {
		err := Transact(ctx, pool, write)
		if err == nil {
			return nil
		}
		var unknown *outcomeUnknownError
		if errors.As(err, &unknown) {
			committed, outcomeErr := awaitOutcome(ctx, pool, unknown.xid)
			if outcomeErr != nil {
				return outcomeErr
			}
			if committed {
				return nil
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// awaitOutcome reads xid's status at the watcher's pace, each read bounded by outcomeReadBound,
// until the store says committed (true) or aborted (false), or ctx ends.
func awaitOutcome(ctx context.Context, pool TxPool, xid string) (bool, error) {
	for wait := outcomeRetryFirst; ; wait = min(2*wait, outcomeRetryMax) {
		reading, cancel := context.WithTimeout(ForConcurrentWorker(ctx), outcomeReadBound)
		result, _ := readOutcome(reading, pool, xid)
		cancel()
		switch result {
		case outcomeCommitted:
			return true, nil
		case outcomeAborted:
			return false, nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}
