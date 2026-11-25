package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// TxPool is what TransactWith opens its transaction on, and the outcome read of a lost commit
// its own read-only one. In the daemon it is always the guarded pool (*Pool), so a body that asks
// for a second connection while its transaction holds one is refused at that call
// (ErrNestedAcquire); the packages that measure their handlers against a real PostgreSQL pass a
// pgx pool of their own.
type TxPool interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// logOf is where TransactWith and its watcher write their lines about a commit over pool: the
// guarded pool's own log (Pool.log), and the process's default logger for a pgx pool of a test's.
func logOf(pool TxPool) *slog.Logger {
	if guarded, ok := pool.(*Pool); ok {
		return guarded.logger()
	}
	return slog.Default()
}

// ErrCommitOutcomeUnknown is TransactWith's answer for a commit whose reply was lost while the
// store could not say whether the transaction committed. A warning names the transaction, and its
// hooks wait for the store's answer (watchOutcome).
var ErrCommitOutcomeUnknown = errors.New("the store did not say whether the transaction committed")

// errTransactionOnAnotherPool is TransactWith's refusal of a transaction asked for on another pool
// while the caller's own is open: one body joins one transaction, and a second pool's would commit
// on its own.
var errTransactionOnAnotherPool = errors.New("store: a transaction on another pool was opened inside an open transaction")

// errJoinOfOtherOptions is TransactWith's refusal of a transaction asked for, inside an open one
// on the same pool, with another isolation level or access mode: a join runs in the open
// transaction, under that transaction's options, so a repeatable-read read would lose its
// snapshot and a read-only one PostgreSQL's refusal of its writes.
var errJoinOfOtherOptions = errors.New("store: a transaction asked for other options than the open transaction it would join")

// errSavepointNotEnded is TransactWith's refusal to commit a transaction in which a joined write's
// savepoint was neither rolled back nor released (join): the join answered an error while that
// write's rows may still be in the transaction, so the transaction rolls back, whatever its body
// returned.
var errSavepointNotEnded = errors.New("store: a joined write's savepoint was neither rolled back nor released, so its transaction does not commit")

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

// afterAcquireKey is the context key a write bound counted from its connection (BoundAfterAcquire)
// lives under, and afterAcquire is the bound; a nil *afterAcquire is no bound.
type afterAcquireKey struct{}

type afterAcquire struct{ acquire, write time.Duration }

// BoundAfterAcquire is ctx with every transaction TransactWith opens under it bounded by its own
// time rather than by its wait for a connection: the transaction waits at most acquire in the
// daemon's pool's queue for a connection, and from the moment the pool hands it one its BEGIN and
// everything after it end write later, its commit's settle aside (CommitSettleBound); Pool.BeginTx
// applies both. The bound is a value of ctx, so it reaches every transaction opened under ctx, a
// join included; the read of a lost commit's outcome clears it (readOutcome), since that read's
// own deadline is its hard end. ctx itself is bounded by all three together, so a call that opens
// no transaction through TransactWith is bounded too.
//
// Only the queue is acquire's. A connection the pool dials for the transaction is dialled under
// write as well (PoolConfig's BeforeConnect); a pooled one is not pinged, its BEGIN is the check,
// and a BEGIN that meets a connection the server closed is tried again on another (Pool.BeginTx).
// So a database that stops answering holds a bounded transaction that holds a connection for up to
// about twice write: twice in the one case of a dial that succeeds just short of write followed by
// a BEGIN the database never answers, which runs its own write from the hand-out, and once write
// in every other. A transaction queued behind others waits in the queue first, up to acquire, as
// the connections ahead of it come back each within its holder's own bound. The hard bound of a
// bounded call is ctx's own deadline: acquire, write and CommitSettleBound together (a claim
// write's are supervise.Deps.BoundWrite's). A deadline counted from before the wait was the queue's
// bound as well as the write's: a wave of transactions on a pool of a few connections cut the last
// of them for the writes ahead of it, however quickly each would have run.
func BoundAfterAcquire(ctx context.Context, acquire, write time.Duration) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, acquire+write+CommitSettleBound)
	return context.WithValue(bounded, afterAcquireKey{}, &afterAcquire{acquire: acquire, write: write}), cancel
}

// afterAcquireOf is the bound BoundAfterAcquire put on ctx, nil for none.
func afterAcquireOf(ctx context.Context) *afterAcquire {
	bound, _ := ctx.Value(afterAcquireKey{}).(*afterAcquire)
	return bound
}

// WithoutWriteBound is ctx with no bound of BoundAfterAcquire on it, whatever ctx carries: the
// bound is a value, so a context that drops its parent's deadline (context.WithoutCancel) keeps it
// unless this takes it off. The read of a lost commit's outcome is one such context
// (readOutcome), and so is a test's write that must run under no bound of its caller's.
func WithoutWriteBound(ctx context.Context) context.Context {
	return context.WithValue(ctx, afterAcquireKey{}, (*afterAcquire)(nil))
}

// writing is ctx under b's write bound, counted from now, or ctx itself for no bound.
func (b *afterAcquire) writing(ctx context.Context) (context.Context, context.CancelFunc) {
	if b == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.write)
}

// writingFrom is ctx under b's write bound counted from handedOut, the moment the pool first
// handed the transaction a connection, or ctx itself for no bound.
func (b *afterAcquire) writingFrom(ctx context.Context, handedOut time.Time) (context.Context, context.CancelFunc) {
	if b == nil {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, handedOut.Add(b.write))
}

// acquiring is ctx under b's acquire bound, the wait in the pool's queue, or ctx itself for no
// bound.
func (b *afterAcquire) acquiring(ctx context.Context) (context.Context, context.CancelFunc) {
	if b == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.acquire)
}

// running is the context a begun transaction runs on: ctx under b's write bound, counted from the
// moment the daemon's pool handed tx its first connection (storeTx.handedOut), or from now for a
// transaction of another pool, which records none; ctx itself for no bound.
func (b *afterAcquire) running(ctx context.Context, tx pgx.Tx) (context.Context, context.CancelFunc) {
	if own, ok := tx.(*storeTx); ok && !own.handedOut.IsZero() {
		return b.writingFrom(ctx, own.handedOut)
	}
	return b.writing(ctx)
}

// notBegun is the error of a begin under b that the pool's queue ended (Pool.BeginTx):
// errAcquireBound when acquiring, the wait under b's acquire bound, ended while ctx still ran,
// err itself otherwise.
func (b *afterAcquire) notBegun(ctx, acquiring context.Context, err error) error {
	if b != nil && ctx.Err() == nil && acquiring.Err() != nil {
		return fmt.Errorf("%w (%s): %w", errAcquireBound, b.acquire, err)
	}
	return err
}

// errAcquireBound is a bounded transaction (BoundAfterAcquire) that waited its whole acquire bound
// in the pool's queue and was handed no connection.
var errAcquireBound = errors.New("store: the pool handed out no connection within the wait for one")

// commitHooksKey is the context key a TransactWith scope lives under. It is named only where a
// scope is stored or read: TransactWith stores the one scope that holds hooks, scopeOf reads it,
// and readOutcome and ForConcurrentWorker store none (TestOnlyTransactOpensACommitScope). Its type
// is this package's own, so a scope stored anywhere else names it there.
type commitHooksKey struct{}

// commitScope is one open transaction of TransactWith: the hooks its body registers, the
// transaction, its pool and its options, whether its body still runs, and the error of each joined
// write whose savepoint was neither rolled back nor released (savepointNotEnded), which keeps the
// transaction from committing. A TransactWith called on a context whose scope's body still runs
// joins that transaction (join).
type commitScope struct {
	hooks             []func()
	tx                pgx.Tx
	pool              TxPool
	options           pgx.TxOptions
	writing           atomic.Bool
	savepointNotEnded error
}

// scopeOf is ctx's commit scope, or nil for a context no TransactWith scopes or one an outcome
// read cleared (readOutcome).
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
// TransactWith has no commit to wait for, and on a scope whose write has returned, since that
// transaction's hooks were taken then and one appended later would never run.
func OnCommit(ctx context.Context, fn func()) {
	scope := scopeOf(ctx)
	if scope == nil {
		panic("store.OnCommit outside a transaction's commit scope (store.Transact)")
	}
	if !scope.writing.Load() {
		panic("store.OnCommit on a transaction whose write has returned (store.Transact): the hook would never run")
	}
	scope.hooks = append(scope.hooks, fn)
}

// CommitScoped reports whether ctx is within a transaction's commit scope (TransactWith) whose
// write still runs: a caller that wants a hook to run once the write it just made has committed
// can tell a transaction that will commit from a write made outside any scope, or after the
// scope's write returned, neither of which has a commit to wait for.
func CommitScoped(ctx context.Context) bool {
	scope := scopeOf(ctx)
	return scope != nil && scope.writing.Load()
}

// Transact is TransactWith with pgx's default options: a read-write transaction that reads its id
// before it commits.
func Transact(ctx context.Context, pool TxPool, write func(ctx context.Context, tx pgx.Tx) error) error {
	return TransactWith(ctx, pool, pgx.TxOptions{}, write)
}

// ReadOnly runs read in one read-only TransactWith on pool and answers the value it read: the one
// form of a standalone read. An error read returns is answered as it is, since read names what it
// read; an error of the transaction itself (its begin, a cancel, its commit) is answered wrapped as
// "<what>: <error>". On any error the value is T's zero value.
func ReadOnly[T any](ctx context.Context, pool TxPool, what string, read func(ctx context.Context, tx pgx.Tx) (T, error)) (T, error) {
	var value, zero T
	var readErr error
	err := TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		value, readErr = read(ctx, tx)
		return readErr
	})
	switch {
	case err == nil:
		return value, nil
	case readErr == nil:
		return zero, fmt.Errorf("%s: %w", what, err)
	default:
		return zero, err
	}
}

// TransactWith runs write in one transaction of pool, opened with options, commits it, and runs
// the hooks write registered with OnCommit if and only if the transaction committed. write runs
// once, on the scoped context, and is never retried. Every transaction of the daemon opens here,
// the read-only and repeatable-read ones included. In order:
//
//   - On a context whose scope's write still runs, it opens no transaction: it joins the open one
//     (join), so write's hooks wait on the open transaction's commit. A join on another pool, or
//     with another isolation level or access mode, runs nothing and is refused
//     (errTransactionOnAnotherPool, errJoinOfOtherOptions). A joined write runs under a
//     savepoint: one that fails is rolled back to it, and its hooks are cut, whatever the open
//     transaction's body then does with its error; one that succeeds releases it. Either
//     statement that fails fails the whole transaction (below). Under a bound (BoundAfterAcquire)
//     the join's write bound runs from the join, since the open transaction holds its connection.
//   - The transaction begins through the pool's BeginTx, the boundary's one begin. Under a bound
//     the daemon's pool ends its first wait in the queue at the acquire bound (errAcquireBound) and
//     runs the BEGIN under the write bound counted from the moment it first handed out a
//     connection (Pool.BeginTx), and the transaction runs under that same deadline (running). A
//     BEGIN tried again on another connection, because the one it met was closed, answers its own
//     failure where its wait for that other connection ends, not errAcquireBound. A transaction of
//     another pool, which records no hand-out, runs under the write bound counted from its begin.
//   - A rollback is deferred as soon as the transaction begins, so a panic in write, or any
//     return below, gives the connection back to the pool and clears the caller's hold mark. It
//     runs on the commit's context, below, so a rollback after the caller's cancel still reaches
//     the server and the pool gets back a connection whose transaction has ended.
//   - write's error is returned. No hook runs.
//   - A joined write whose savepoint was neither rolled back nor released (join) is returned as
//     errSavepointNotEnded, with that write's error and the savepoint statement's, even when write
//     returned nil, and nothing commits: pgx leaves the transaction open after a failed savepoint
//     statement, so a commit would keep the rows of a join that answered an error. No hook runs.
//   - The transaction's id is read. No id means the transaction wrote nothing. A read-only
//     transaction, in which PostgreSQL refuses every write to a table or a sequence (SQLSTATE
//     25006), reads none.
//   - A caller's context that has ended is returned as its error, and nothing commits: a context
//     that ended inside write already fails the id read, and this refuses one that ended after it.
//   - The commit runs on a context that the caller's cancel ends only commitReplyBound after that
//     cancel. A caller whose context never ends waits for the reply as long as the server takes.
//     The outcome read's own transaction has no such grace (readOutcome).
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
	bound := afterAcquireOf(ctx)
	if open := scopeOf(ctx); open != nil && open.writing.Load() {
		joining, stop := bound.writing(ctx)
		defer stop()
		return open.join(joining, pool, options, write)
	}
	tx, err := pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	ctx, stopBound := bound.running(ctx, tx)
	defer stopBound()
	committing, stop := commitContext(ctx)
	defer stop()
	// Once a commit has been tried, the transaction is closed and this does nothing.
	defer func() { _ = tx.Rollback(committing) }()

	scope := &commitScope{tx: tx, pool: pool, options: options}
	scoped := context.WithValue(ctx, commitHooksKey{}, scope)
	if err := runWrite(scoped, scope, write); err != nil {
		return err
	}
	if scope.savepointNotEnded != nil {
		return fmt.Errorf("%w: %w", errSavepointNotEnded, scope.savepointNotEnded)
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

// join runs write inside the scope's open transaction, on ctx, the scope's own context, under a
// savepoint of that transaction, and answers nil only for a write whose rows and hooks commit or
// roll back with the open transaction's. A write that fails is rolled back to the savepoint, and
// the hooks it registered are cut from the scope's list, so an outer body that swallows its error
// commits neither. A write that succeeds releases the savepoint and keeps its hooks on the scope.
// The rollback and the release run on the commit's context (commitContext), as TransactWith's
// rollback and commit do: on ctx alone, a ctx that has ended sends neither, and a join whose write
// succeeded would answer an error for rows and hooks that commit. A rollback or a release that
// fails all the same is returned with the write's error, and kept on the scope
// (savepointNotEnded): the write's rows may still be in the transaction, so TransactWith then
// commits nothing, whatever the outer body returns. So a join that answers an error leaves
// nothing of its write to commit. A join on another pool, or with another isolation level or
// access mode than the open transaction's, runs nothing.
func (s *commitScope) join(ctx context.Context, pool TxPool, options pgx.TxOptions, write func(ctx context.Context, tx pgx.Tx) error) error {
	if !samePool(s.pool, pool) {
		return errTransactionOnAnotherPool
	}
	if isolation(options) != isolation(s.options) || access(options) != access(s.options) {
		return errJoinOfOtherOptions
	}
	savepoint, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	registered := len(s.hooks)
	written := write(ctx, savepoint)
	ending, stop := commitContext(ctx)
	defer stop()
	var ended error
	if written != nil {
		s.hooks = s.hooks[:registered]
		ended = savepoint.Rollback(ending)
	} else {
		ended = savepoint.Commit(ending)
	}
	if ended != nil {
		s.savepointNotEnded = errors.Join(s.savepointNotEnded, written, ended)
	}
	return errors.Join(written, ended)
}

// isolation is options' isolation level, the server's default (read committed) when it names none.
func isolation(options pgx.TxOptions) pgx.TxIsoLevel {
	if options.IsoLevel == "" {
		return pgx.ReadCommitted
	}
	return options.IsoLevel
}

// access is options' access mode, read-write when it names none.
func access(options pgx.TxOptions) pgx.TxAccessMode {
	if options.AccessMode == "" {
		return pgx.ReadWrite
	}
	return options.AccessMode
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
		logOf(pool).Warn("store: a commit's reply was lost; the store says the transaction committed",
			"transaction", xid, "error", lost)
		runHooks(hooks)
		return nil
	case outcomeAborted:
		return &notCommittedError{xid: xid, lost: lost}
	default:
		logOf(pool).Warn("store: a commit's reply was lost and the store cannot say whether it landed; its commit hooks wait until it can",
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

// notCommittedError is TransactWith's answer for a commit whose reply was lost and which the store
// then says did not commit: the transaction it names and the commit's error. Nothing of the
// transaction landed, so running the write again is safe (transactUntilCommitted).
type notCommittedError struct {
	xid  string
	lost error
}

func (e *notCommittedError) Error() string {
	return fmt.Sprintf("transaction %s did not commit: %v", e.xid, e.lost)
}

// Unwrap is the commit's error.
func (e *notCommittedError) Unwrap() error { return e.lost }

// noCommitGrace marks the context of an outcome read: commitContext gives its COMMIT and its
// rollback no grace, so the read's deadline is its hard end. readOutcome is the one place that
// sets it (TestOnlyTheOutcomeReadWaivesTheCommitGrace).
type noCommitGrace struct{}

// commitContext is ctx without its cancel, ended commitReplyBound after ctx ends, for the
// statements that end a transaction or a joined write's savepoint alone: the commit, the rollback,
// and a savepoint's release and rollback (join). A ctx that never ends never ends it. stop ends it
// at once and releases what it holds. An outcome read's ctx (readOutcome) is its own commit
// context, with a stop that does nothing: its deadline cuts its COMMIT and its rollback as it cuts
// its query.
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
// TransactWith, again every outcomePollStep while it answers `in progress`, until ctx ends. Its
// context is ctx marked noCommitGrace and holding no commit scope, so a read never joins a
// transaction, and ctx's deadline is the hard end of every try: its BEGIN, its query, its COMMIT
// and its rollback. A COMMIT the deadline cuts is a read-only transaction's lost reply, so the
// status the query read stands. A failed read, a status the server does not keep (NULL), and a
// transaction still in progress when ctx ends are unknown, with the error that says which.
func readOutcome(ctx context.Context, pool TxPool, xid string) (outcome, error) {
	// The read is no transaction of its caller's: it holds no commit scope of theirs and no write
	// bound of theirs, whose deadline would cut it short of its own (BoundAfterAcquire).
	reading := WithoutWriteBound(context.WithValue(context.WithValue(ctx, noCommitGrace{}, true),
		commitHooksKey{}, (*commitScope)(nil)))
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

// watchOutcome runs hooks once the store says xid committed, and drops them once it says it
// aborted (pollOutcome, from outcomeRetryFirst). ctx has no cancel and a hold mark of its own,
// since the transaction's owner may hold its next transaction's connection on its old mark, and
// the watcher holds no connection between reads. It ends at the first answer, or once the pool
// has closed, which says that the hooks did not run; so it runs its hooks at most once. It has no
// other time limit and is not joined at stop: a status the server does not keep leaves it reading
// every outcomeRetryMax until the pool closes.
func watchOutcome(ctx context.Context, pool TxPool, xid string, hooks []func()) {
	log := logOf(pool)
	switch result, _ := pollOutcome(ctx, pool, xid, outcomeRetryFirst); result {
	case outcomeCommitted:
		log.Warn("store: a transaction whose outcome was unknown committed; its commit hooks run now",
			"transaction", xid, "hooks", len(hooks))
		runWatchedHooks(log, xid, hooks)
	case outcomeAborted:
		log.Info("store: a transaction whose outcome was unknown did not commit; its commit hooks are dropped",
			"transaction", xid, "hooks", len(hooks))
	default:
		log.Error("store: the pool closed before the store said whether a transaction committed; its commit hooks did not run",
			"transaction", xid, "hooks", len(hooks))
	}
}

// pollOutcome is the one loop that waits for the store to say whether xid committed: it waits
// first (none reads at once), then reads, each read bounded by outcomeReadBound, and waits
// outcomeRetryFirst before the next, twice as long each time up to outcomeRetryMax. It answers
// committed or aborted at the store's first answer, and unknown with the error once the pool has
// closed (puddle.ErrClosedPool) or ctx has ended. Each read takes ctx's hold mark, so a caller
// that may still hold a connection on its own mark gives it a mark of its own.
func pollOutcome(ctx context.Context, pool TxPool, xid string, first time.Duration) (outcome, error) {
	for wait := first; ; wait = min(max(2*wait, outcomeRetryFirst), outcomeRetryMax) {
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return outcomeUnknown, ctx.Err()
			case <-timer.C:
			}
		}
		reading, cancel := context.WithTimeout(ctx, outcomeReadBound)
		result, err := readOutcome(reading, pool, xid)
		cancel()
		switch {
		case result != outcomeUnknown:
			return result, nil
		case errors.Is(err, puddle.ErrClosedPool):
			return outcomeUnknown, err
		}
	}
}

// runWatchedHooks runs hooks in order on the watcher's goroutine, each under a recover. Nothing
// above the watcher recovers a panic, as net/http does for a route's goroutine, so a hook that
// panics there is logged to log with its transaction and its stack, and the next hook still runs.
func runWatchedHooks(log *slog.Logger, xid string, hooks []func()) {
	for _, hook := range hooks {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Error("store: a commit hook the watcher ran panicked",
						"transaction", xid, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				}
			}()
			hook()
		}()
	}
}

// transactUntilCommitted runs write in Transact until one run commits, and returns nil then. A run
// whose outcome is unknown is waited for (pollOutcome) and taken as that outcome. A run that did
// not commit runs again after outcomeRetryFirst, the wait doubling up to outcomeRetryMax, only when
// its failure passes (retryable); any other error, a definite error of write included, is
// returned at once. When ctx ends first, or the pool closes while it waits for an outcome, it
// returns the last run's error, so a run whose outcome is still unknown answers
// ErrCommitOutcomeUnknown. write must be safe to run again after a run that did not commit.
//
// A run whose outcome was unknown and that the store then says committed returns nil while its
// hooks are the watcher's (watchOutcome), which may run them after transactUntilCommitted returns.
func transactUntilCommitted(ctx context.Context, pool TxPool, write func(ctx context.Context, tx pgx.Tx) error) error {
	for wait := outcomeRetryFirst; ; wait = min(2*wait, outcomeRetryMax) {
		err := Transact(ctx, pool, write)
		if err == nil {
			return nil
		}
		var unknown *outcomeUnknownError
		switch {
		case errors.As(err, &unknown):
			switch result, _ := pollOutcome(ForConcurrentWorker(ctx), pool, unknown.xid, 0); result {
			case outcomeCommitted:
				return nil
			case outcomeUnknown:
				return err
			}
		case !retryable(err):
			return err
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

// retryable reports whether a run that did not commit failed for a reason that passes, so its
// write is worth running again: the store said a run whose reply was lost did not commit
// (notCommittedError), nothing reached the server (pgconn.SafeToRetry), or the server refused the
// run for a reason of its own state: a connection failure (class 08), a serialization failure or
// a deadlock (class 40), too few resources (class 53), or a server stopping or starting (57P01,
// 57P02, 57P03). A definite error of the write, ErrNestedAcquire and every other answer are not.
func retryable(err error) bool {
	var notCommitted *notCommittedError
	if errors.As(err, &notCommitted) || pgconn.SafeToRetry(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code[:min(2, len(pgErr.Code))] {
	case "08", "40", "53":
		return true
	}
	switch pgErr.Code {
	case "57P01", "57P02", "57P03":
		return true
	}
	return false
}
